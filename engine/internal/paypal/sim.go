package paypal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/db"
)

// Sim mirrors PayPal's semantics closely enough to exercise every Sway code path without credentials:
// idempotent create/capture by request id, refund limits, and payout items that settle after a delay.
// Receivers containing "unclaimed" stay UNCLAIMED; receivers containing "fail" FAIL. Everything it
// creates is labelled SIMULATED in the UI. It never accepts external webhooks.
type Sim struct {
	DB          *db.DB
	SettleAfter time.Duration
}

func (s *Sim) Mode() string { return "sim" }

func simID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToUpper(hex.EncodeToString(b[:]))
}

type simOrder struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AmountCents int64  `json:"amount_cents"`
	ReferenceID string `json:"reference_id"`
	CaptureID   string `json:"capture_id"`
	Refunded    int64  `json:"refunded_cents"`
}

type simItem struct {
	ItemID       string    `json:"item_id"`
	SenderItemID string    `json:"sender_item_id"`
	Receiver     string    `json:"receiver"`
	AmountCents  int64     `json:"amount_cents"`
	Status       string    `json:"status"`
	SettleAt     time.Time `json:"settle_at"`
}

type simBatch struct {
	BatchID string    `json:"batch_id"`
	Items   []simItem `json:"items"`
}

func (s *Sim) get(ctx context.Context, q db.Q, id string, v any, lock bool) (bool, error) {
	sql := `SELECT data FROM paypal_sim_objects WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var raw []byte
	err := q.QueryRow(ctx, sql, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

func (s *Sim) put(ctx context.Context, q db.Q, id, kind string, v any) error {
	raw, _ := json.Marshal(v)
	_, err := q.Exec(ctx, `INSERT INTO paypal_sim_objects (id, kind, data) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET data=EXCLUDED.data, updated_at=now()`, id, kind, raw)
	return err
}

func (s *Sim) CreateOrder(ctx context.Context, r CreateOrderRequest) (Order, error) {
	if r.AmountCents <= 0 {
		return Order{}, &Error{Status: 422, Name: "UNPROCESSABLE_ENTITY", Issue: "AMOUNT_MUST_BE_POSITIVE"}
	}
	var out Order
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var existing string
		if ok, err := s.get(ctx, q, "req:"+r.RequestID, &existing, true); err != nil {
			return err
		} else if ok {
			out = Order{ID: existing, Status: "CREATED"}
			return nil
		}
		o := simOrder{ID: simID("SIM-ORD-"), Status: "CREATED", AmountCents: r.AmountCents, ReferenceID: r.ReferenceID}
		if err := s.put(ctx, q, o.ID, "order", o); err != nil {
			return err
		}
		out = Order{ID: o.ID, Status: o.Status}
		return s.put(ctx, q, "req:"+r.RequestID, "request", o.ID)
	})
	return out, err
}

func (s *Sim) CaptureOrder(ctx context.Context, paypalOrderID, requestID string) (Capture, error) {
	var out Capture
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var o simOrder
		ok, err := s.get(ctx, q, paypalOrderID, &o, true)
		if err != nil {
			return err
		}
		if !ok {
			return &Error{Status: 404, Name: "RESOURCE_NOT_FOUND", Issue: "INVALID_RESOURCE_ID"}
		}
		if o.Status != "COMPLETED" { // capturing twice returns the original capture (idempotent)
			o.Status, o.CaptureID = "COMPLETED", simID("SIM-CAP-")
			if err := s.put(ctx, q, o.ID, "order", o); err != nil {
				return err
			}
			if err := s.put(ctx, q, o.CaptureID, "capture", o.ID); err != nil {
				return err
			}
		}
		out = Capture{OrderID: o.ID, OrderStatus: o.Status, CaptureID: o.CaptureID, Status: "COMPLETED", AmountCents: o.AmountCents,
			PayerEmail: "sim-buyer@personal.example.com", ReferenceID: o.ReferenceID}
		return nil
	})
	return out, err
}

func (s *Sim) RefundCapture(ctx context.Context, captureID string, amountCents int64, requestID, note string) (Refund, error) {
	var out Refund
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var prior Refund
		if ok, err := s.get(ctx, q, "req:"+requestID, &prior, true); err != nil {
			return err
		} else if ok {
			out = prior
			return nil
		}
		var orderID string
		if ok, err := s.get(ctx, q, captureID, &orderID, false); err != nil {
			return err
		} else if !ok {
			return &Error{Status: 404, Name: "RESOURCE_NOT_FOUND", Issue: "INVALID_RESOURCE_ID"}
		}
		var o simOrder
		if _, err := s.get(ctx, q, orderID, &o, true); err != nil {
			return err
		}
		remaining := o.AmountCents - o.Refunded
		if amountCents == 0 {
			amountCents = remaining
		}
		if remaining <= 0 {
			return &Error{Status: 422, Name: "UNPROCESSABLE_ENTITY", Issue: "CAPTURE_FULLY_REFUNDED"}
		}
		if amountCents > remaining {
			return &Error{Status: 422, Name: "UNPROCESSABLE_ENTITY", Issue: "REFUND_AMOUNT_EXCEEDED"}
		}
		o.Refunded += amountCents
		if err := s.put(ctx, q, o.ID, "order", o); err != nil {
			return err
		}
		out = Refund{ID: simID("SIM-REF-"), Status: "COMPLETED", AmountCents: amountCents, CaptureID: captureID}
		return s.put(ctx, q, "req:"+requestID, "refund", out)
	})
	return out, err
}

func (s *Sim) CreatePayoutBatch(ctx context.Context, r PayoutBatchRequest) (PayoutBatch, error) {
	var out PayoutBatch
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var existing simBatch
		if ok, err := s.get(ctx, q, "sbid:"+r.SenderBatchID, &existing, true); err != nil {
			return err
		} else if ok {
			return &Error{Status: 400, Name: "USER_BUSINESS_ERROR", Issue: "DUPLICATE_REQUEST_ID", Message: "duplicate sender_batch_id"}
		}
		b := simBatch{BatchID: simID("SIM-BATCH-")}
		settle := time.Now().Add(s.SettleAfter)
		for _, it := range r.Items {
			if it.AmountCents <= 0 || !strings.Contains(it.Receiver, "@") {
				return &Error{Status: 422, Name: "VALIDATION_ERROR", Issue: "RECEIVER_INVALID"}
			}
			b.Items = append(b.Items, simItem{ItemID: simID("SIM-ITEM-"), SenderItemID: it.SenderItemID, Receiver: it.Receiver,
				AmountCents: it.AmountCents, Status: "PENDING", SettleAt: settle})
		}
		if err := s.put(ctx, q, b.BatchID, "payout_batch", b); err != nil {
			return err
		}
		if err := s.put(ctx, q, "sbid:"+r.SenderBatchID, "payout_batch_ref", b); err != nil {
			return err
		}
		for _, it := range b.Items {
			if err := s.put(ctx, q, it.ItemID, "payout_item_ref", b.BatchID); err != nil {
				return err
			}
		}
		out = b.view()
		out.Status = "PENDING"
		return nil
	})
	return out, err
}

func (b simBatch) view() PayoutBatch {
	out := PayoutBatch{BatchID: b.BatchID, Status: "SUCCESS"}
	for _, it := range b.Items {
		out.Items = append(out.Items, PayoutItem{ItemID: it.ItemID, SenderItemID: it.SenderItemID, Status: it.Status})
		if it.Status == "PENDING" {
			out.Status = "PROCESSING"
		}
	}
	return out
}

func (s *Sim) GetPayoutBatch(ctx context.Context, batchID string) (PayoutBatch, error) {
	var out PayoutBatch
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var b simBatch
		ok, err := s.get(ctx, q, batchID, &b, true)
		if err != nil {
			return err
		}
		if !ok {
			return &Error{Status: 404, Name: "RESOURCE_NOT_FOUND"}
		}
		changed := false
		for i, it := range b.Items {
			if it.Status == "PENDING" && !time.Now().Before(it.SettleAt) {
				switch {
				case strings.Contains(it.Receiver, "unclaimed"):
					b.Items[i].Status = "UNCLAIMED"
				case strings.Contains(it.Receiver, "fail"):
					b.Items[i].Status = "FAILED"
				default:
					b.Items[i].Status = "SUCCESS"
				}
				changed = true
			}
		}
		if changed {
			if err := s.put(ctx, q, b.BatchID, "payout_batch", b); err != nil {
				return err
			}
		}
		out = b.view()
		return nil
	})
	return out, err
}

func (s *Sim) CancelPayoutItem(ctx context.Context, itemID string) (PayoutItem, error) {
	var out PayoutItem
	err := s.DB.Tx(ctx, func(q db.Q) error {
		var batchID string
		if ok, err := s.get(ctx, q, itemID, &batchID, false); err != nil {
			return err
		} else if !ok {
			return &Error{Status: 404, Name: "RESOURCE_NOT_FOUND"}
		}
		var b simBatch
		if _, err := s.get(ctx, q, batchID, &b, true); err != nil {
			return err
		}
		for i, it := range b.Items {
			if it.ItemID != itemID {
				continue
			}
			if it.Status != "UNCLAIMED" {
				return &Error{Status: 400, Name: "ITEM_CANNOT_BE_CANCELLED", Issue: "ITEM_ALREADY_" + it.Status,
					Message: fmt.Sprintf("only UNCLAIMED items can be cancelled (item is %s)", it.Status)}
			}
			b.Items[i].Status = "RETURNED"
			out = PayoutItem{ItemID: itemID, SenderItemID: it.SenderItemID, Status: "RETURNED"}
			return s.put(ctx, q, b.BatchID, "payout_batch", b)
		}
		return &Error{Status: 404, Name: "RESOURCE_NOT_FOUND"}
	})
	return out, err
}

// VerifyWebhook never accepts external events in simulator mode: simulated events are injected internally.
func (s *Sim) VerifyWebhook(context.Context, http.Header, []byte) (bool, error) { return false, nil }

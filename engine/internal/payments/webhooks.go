package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/jobs"
	"github.com/vivekyarra/sway/engine/internal/money"
)

// ErrBadSignature is returned for webhooks PayPal did not sign.
var ErrBadSignature = errors.New("webhook signature verification failed")

type event struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

// IngestWebhook verifies, stores (deduplicated by event id) and enqueues a PayPal webhook (spec 8.5).
// It returns quickly; processing happens in the worker.
func (s *Service) IngestWebhook(ctx context.Context, h http.Header, body []byte) (duplicate bool, err error) {
	ok, err := s.PP.VerifyWebhook(ctx, h, body)
	if err != nil {
		return false, fmt.Errorf("verify webhook: %w", err)
	}
	if !ok {
		return false, ErrBadSignature
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.EventType == "" {
		return false, fmt.Errorf("%w: malformed event", ErrInvalid)
	}
	inserted, err := s.storeEvent(ctx, ev, body, true, false)
	return !inserted, err
}

func resourceID(raw json.RawMessage) string {
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.ID
}

func (s *Service) storeEvent(ctx context.Context, ev event, body []byte, verified, simulated bool) (bool, error) {
	var inserted bool
	err := s.DB.Tx(ctx, func(q db.Q) error {
		tag, err := q.Exec(ctx, `INSERT INTO webhook_events (id, paypal_event_id, event_type, resource_id, verified, simulated, payload) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (paypal_event_id) DO NOTHING`, newID("whe_"), ev.ID, ev.EventType, resourceID(ev.Resource), verified, simulated, body)
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected() == 1
		if !inserted {
			return nil
		}
		return jobs.Enqueue(ctx, q, jobs.TypeWebhook, map[string]string{"event_id": ev.ID}, "wh:"+ev.ID, 0)
	})
	if err == nil && inserted {
		s.wake()
	}
	return inserted, err
}

// InjectEvent records a simulated event and routes it through the same handler a real webhook uses.
// Used by the PayPal simulator and the merchant's "Simulate dispute" button; always flagged simulated.
func (s *Service) InjectEvent(ctx context.Context, eventType string, resource any) error {
	res, err := json.Marshal(resource)
	if err != nil {
		return err
	}
	ev := event{ID: newID("SIM-EVT-"), EventType: eventType, Resource: res}
	body, _ := json.Marshal(map[string]any{"id": ev.ID, "event_type": eventType, "resource": json.RawMessage(res), "simulated": true})
	if _, err := s.storeEvent(ctx, ev, body, false, true); err != nil {
		return err
	}
	return s.ProcessWebhook(ctx, ev.ID)
}

// ProcessWebhook applies a stored event exactly once.
func (s *Service) ProcessWebhook(ctx context.Context, eventID string) error {
	var typ string
	var payload []byte
	var simulated bool
	var processed *string
	err := s.DB.Pool.QueryRow(ctx, `SELECT event_type, payload, simulated, processed_at::text FROM webhook_events WHERE paypal_event_id=$1`, eventID).Scan(&typ, &payload, &simulated, &processed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if processed != nil {
		return nil
	}
	var ev event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	outcome, err := s.route(ctx, typ, ev.Resource, simulated)
	if err != nil {
		return err
	}
	_, err = s.DB.Pool.Exec(ctx, `UPDATE webhook_events SET processed_at=now(), outcome=$2 WHERE paypal_event_id=$1 AND processed_at IS NULL`, eventID, outcome)
	return err
}

func (s *Service) orderByCapture(ctx context.Context, captureID string) (string, error) {
	var id string
	err := s.DB.Pool.QueryRow(ctx, `SELECT id FROM orders WHERE paypal_capture_id=$1`, captureID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func captureFromLinks(raw json.RawMessage) string {
	var r struct {
		Links []struct {
			Rel  string `json:"rel"`
			Href string `json:"href"`
		} `json:"links"`
	}
	_ = json.Unmarshal(raw, &r)
	for _, l := range r.Links {
		if l.Rel == "up" && strings.Contains(l.Href, "/captures/") {
			return l.Href[strings.LastIndex(l.Href, "/")+1:]
		}
	}
	return ""
}

func (s *Service) route(ctx context.Context, typ string, res json.RawMessage, simulated bool) (string, error) {
	switch typ {
	case "CHECKOUT.ORDER.APPROVED":
		id := resourceID(res)
		_, err := s.DB.Pool.Exec(ctx, `UPDATE orders SET status='APPROVED' WHERE paypal_order_id=$1 AND status='CREATED'`, id)
		return "order approved", err

	case "PAYMENT.CAPTURE.COMPLETED":
		var r struct {
			ID     string `json:"id"`
			Amount struct {
				Value string `json:"value"`
			} `json:"amount"`
			Supp struct {
				Related struct {
					OrderID string `json:"order_id"`
				} `json:"related_ids"`
			} `json:"supplementary_data"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return "", err
		}
		var orderID string
		err := s.DB.Pool.QueryRow(ctx, `SELECT id FROM orders WHERE paypal_order_id=$1 OR paypal_capture_id=$2 LIMIT 1`, r.Supp.Related.OrderID, r.ID).Scan(&orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "ignored: unknown order", nil
		}
		if err != nil {
			return "", err
		}
		amt, err := money.Parse(r.Amount.Value)
		if err != nil {
			return "", err
		}
		if err := s.HandleCaptureCompleted(ctx, orderID, r.ID, amt, ""); err != nil {
			return "", err
		}
		return "capture confirmed (idempotent)", nil

	case "PAYMENT.CAPTURE.REFUNDED", "PAYMENT.CAPTURE.REVERSED":
		var r struct {
			ID     string `json:"id"`
			Amount struct {
				Value string `json:"value"`
			} `json:"amount"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return "", err
		}
		captureID := captureFromLinks(res)
		if typ == "PAYMENT.CAPTURE.REVERSED" && captureID == "" {
			captureID = r.ID
		}
		orderID, err := s.orderByCapture(ctx, captureID)
		if err != nil || orderID == "" {
			return "ignored: unknown capture", err
		}
		amt, err := money.Parse(r.Amount.Value)
		if err != nil {
			return "", err
		}
		refundID := r.ID
		if typ == "PAYMENT.CAPTURE.REVERSED" {
			refundID = "reversal-" + r.ID
		}
		if err := s.HandleRefund(ctx, orderID, refundID, amt, strings.ToLower(strings.TrimPrefix(typ, "PAYMENT.CAPTURE.")), simulated); err != nil {
			return "", err
		}
		return "refund applied (idempotent)", nil

	case "CUSTOMER.DISPUTE.CREATED", "CUSTOMER.DISPUTE.UPDATED", "CUSTOMER.DISPUTE.RESOLVED":
		var r struct {
			Status       string `json:"status"`
			Transactions []struct {
				SellerTransactionID string `json:"seller_transaction_id"`
			} `json:"disputed_transactions"`
			Outcome struct {
				Code string `json:"outcome_code"`
			} `json:"dispute_outcome"`
			SwayOrderID string `json:"sway_order_id"` // set by simulated disputes only
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return "", err
		}
		orderID := r.SwayOrderID
		if orderID == "" && len(r.Transactions) > 0 {
			var err error
			if orderID, err = s.orderByCapture(ctx, r.Transactions[0].SellerTransactionID); err != nil {
				return "", err
			}
		}
		if orderID == "" {
			return "ignored: unknown transaction", nil
		}
		action := DisputeOpen
		if typ == "CUSTOMER.DISPUTE.RESOLVED" || r.Status == "RESOLVED" {
			action = DisputeResolveSeller
			if strings.Contains(r.Outcome.Code, "BUYER") {
				action = DisputeResolveBuyer
			}
		}
		if err := s.HandleDispute(ctx, orderID, action, simulated); err != nil && !errors.Is(err, ErrInvalid) {
			return "", err
		}
		return "dispute " + action, nil

	case "PAYMENT.PAYOUTSBATCH.SUCCESS", "PAYMENT.PAYOUTSBATCH.DENIED", "PAYMENT.PAYOUTSBATCH.PROCESSING",
		"PAYMENT.PAYOUTS-ITEM.SUCCEEDED", "PAYMENT.PAYOUTS-ITEM.FAILED", "PAYMENT.PAYOUTS-ITEM.UNCLAIMED", "PAYMENT.PAYOUTS-ITEM.RETURNED",
		"PAYMENT.PAYOUTS-ITEM.BLOCKED", "PAYMENT.PAYOUTS-ITEM.CANCELED", "PAYMENT.PAYOUTS-ITEM.DENIED", "PAYMENT.PAYOUTS-ITEM.REFUNDED":
		var r struct {
			BatchID string `json:"payout_batch_id"`
			Header  struct {
				BatchID string `json:"payout_batch_id"`
			} `json:"batch_header"`
		}
		_ = json.Unmarshal(res, &r)
		ppBatch := r.BatchID
		if ppBatch == "" {
			ppBatch = r.Header.BatchID
		}
		var batchID string
		err := s.DB.Pool.QueryRow(ctx, `SELECT id FROM payout_batches WHERE paypal_payout_batch_id=$1`, ppBatch).Scan(&batchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "ignored: unknown batch", nil
		}
		if err != nil {
			return "", err
		}
		if err := jobs.Enqueue(ctx, s.DB.Pool, jobs.TypePayoutStatus, map[string]string{"batch_id": batchID}, "poll:"+batchID, 0); err != nil {
			return "", err
		}
		s.wake()
		return "payout status refresh queued", nil
	}
	return "ignored: unhandled event type", nil
}

// SimulateDispute injects a clearly labelled dispute event for an order (spec 8.6, demo only).
func (s *Service) SimulateDispute(ctx context.Context, orderID, action string) error {
	if !s.Cfg.DemoMode {
		return ErrDemoOnly
	}
	typ := "CUSTOMER.DISPUTE.CREATED"
	res := map[string]any{"sway_order_id": orderID, "status": "OPEN", "reason": "MERCHANDISE_OR_SERVICE_NOT_AS_DESCRIBED"}
	switch action {
	case DisputeOpen:
	case DisputeResolveSeller:
		typ, res["status"], res["dispute_outcome"] = "CUSTOMER.DISPUTE.RESOLVED", "RESOLVED", map[string]string{"outcome_code": "RESOLVED_SELLER_FAVOUR"}
	case DisputeResolveBuyer:
		typ, res["status"], res["dispute_outcome"] = "CUSTOMER.DISPUTE.RESOLVED", "RESOLVED", map[string]string{"outcome_code": "RESOLVED_BUYER_FAVOUR"}
	default:
		return fmt.Errorf("%w: action must be open, resolve_seller or resolve_buyer", ErrInvalid)
	}
	var dispute string
	if err := s.DB.Pool.QueryRow(ctx, `SELECT dispute_status FROM orders WHERE id=$1`, orderID).Scan(&dispute); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: order %s", ErrNotFound, orderID)
		}
		return err
	}
	if action != DisputeOpen && dispute != "OPEN" {
		return fmt.Errorf("%w: open a dispute first", ErrInvalid)
	}
	if action == DisputeOpen && dispute == "OPEN" {
		return fmt.Errorf("%w: a dispute is already open", ErrInvalid)
	}
	return s.InjectEvent(ctx, typ, res)
}

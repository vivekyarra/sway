// Package payments owns the money lifecycle (spec 5.3, 7.4, 8): PayPal checkout and capture,
// commission reservation, refunds, disputes, vesting, payouts and clawbacks.
//
// Every external signal (API response, webhook, simulated event) funnels into idempotent handlers,
// and Reconcile derives the ledger deltas an order needs from facts (refunded amount, dispute state,
// payout state). Replays, races and out-of-order webhooks therefore converge on the same books.
package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/attribution"
	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/jobs"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/money"
	"github.com/vivekyarra/sway/engine/internal/paypal"
)

type Service struct {
	DB   *db.DB
	PP   paypal.Provider
	Cfg  config.Config
	Log  *slog.Logger
	Wake func() // nudges the job worker after enqueueing
}

// Errors surfaced to API callers.
var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrInvalid        = errors.New("invalid request")
	ErrNotRefundable  = errors.New("order is not refundable")
	ErrDemoOnly       = errors.New("only available in demo mode")
	ErrAlreadyClaimed = errors.New("payout already claimed")
)

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (s *Service) wake() {
	if s.Wake != nil {
		s.Wake()
	}
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Simulated reports whether money in this deployment is simulated.
func (s *Service) Simulated() bool { return s.PP.Mode() != "sandbox" }

// CreatedOrder is returned to the browser for the PayPal buttons.
type CreatedOrder struct {
	OrderID       string `json:"order_id"`
	PayPalOrderID string `json:"paypal_order_id"`
	AmountCents   int64  `json:"amount_cents"`
	Mode          string `json:"mode"`
}

// CreateOrder creates a PayPal order for the product the agent chose (spec 8.2).
// The price always comes from the database, never from the browser.
func (s *Service) CreateOrder(ctx context.Context, decisionID string) (CreatedOrder, error) {
	var productID, merchantID, name string
	var price int64
	var rate int
	err := s.DB.Pool.QueryRow(ctx, `SELECT p.id, p.merchant_id, p.name, p.price_cents, m.commission_rate_bps FROM decisions d
		JOIN products p ON p.id=d.chosen_product_id JOIN merchants m ON m.id=p.merchant_id WHERE d.id=$1`, decisionID).
		Scan(&productID, &merchantID, &name, &price, &rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreatedOrder{}, fmt.Errorf("%w: decision %s", ErrNotFound, decisionID)
	}
	if err != nil {
		return CreatedOrder{}, err
	}
	var orderID string
	var ppID *string
	// Reuse an un-captured order for the same decision (the shopper closed the popup and tried again).
	err = s.DB.Pool.QueryRow(ctx, `SELECT id, paypal_order_id FROM orders WHERE decision_id=$1 AND status IN ('CREATED','APPROVED') ORDER BY created_at DESC LIMIT 1`, decisionID).Scan(&orderID, &ppID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CreatedOrder{}, err
	}
	if orderID == "" {
		var captured bool
		if err := s.DB.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE decision_id=$1 AND status NOT IN ('CREATED','APPROVED','FAILED'))`, decisionID).Scan(&captured); err != nil {
			return CreatedOrder{}, err
		}
		if captured {
			return CreatedOrder{}, fmt.Errorf("%w: this decision was already purchased; ask the agent again to buy another", ErrConflict)
		}
		orderID = newID("ord_")
		if _, err := s.DB.Pool.Exec(ctx, `INSERT INTO orders (id, decision_id, product_id, merchant_id, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, orderID, decisionID, productID, merchantID, price, rate, s.Cfg.SwayFeeBps, s.PP.Mode()); err != nil {
			return CreatedOrder{}, err
		}
	}
	if ppID != nil && *ppID != "" {
		return CreatedOrder{OrderID: orderID, PayPalOrderID: *ppID, AmountCents: price, Mode: s.PP.Mode()}, nil
	}
	o, err := s.PP.CreateOrder(ctx, paypal.CreateOrderRequest{
		RequestID: orderID, ReferenceID: orderID, CustomID: decisionID, AmountCents: price,
		Description: name + " (chosen by Sway agent)",
	})
	if err != nil {
		return CreatedOrder{}, fmt.Errorf("paypal create order: %w", err)
	}
	if _, err := s.DB.Pool.Exec(ctx, `UPDATE orders SET paypal_order_id=$2 WHERE id=$1 AND paypal_order_id IS NULL`, orderID, o.ID); err != nil {
		return CreatedOrder{}, err
	}
	return CreatedOrder{OrderID: orderID, PayPalOrderID: o.ID, AmountCents: price, Mode: s.PP.Mode()}, nil
}

// Capture captures an approved PayPal order (spec 5.3 step 4). Calling it twice is safe.
func (s *Service) Capture(ctx context.Context, paypalOrderID string) (string, error) {
	var orderID, status string
	err := s.DB.Pool.QueryRow(ctx, `SELECT id, status FROM orders WHERE paypal_order_id=$1`, paypalOrderID).Scan(&orderID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: paypal order %s", ErrNotFound, paypalOrderID)
	}
	if err != nil {
		return "", err
	}
	if status != "CREATED" && status != "APPROVED" {
		return orderID, nil // already captured (or refunded): idempotent
	}
	c, err := s.PP.CaptureOrder(ctx, paypalOrderID, "capture-"+orderID)
	if err != nil {
		if paypal.IsIssue(err, "INSTRUMENT_DECLINED") {
			return orderID, fmt.Errorf("%w: the payment method was declined; choose another in PayPal", ErrInvalid)
		}
		return orderID, fmt.Errorf("paypal capture: %w", err)
	}
	if c.Status != "COMPLETED" {
		return orderID, fmt.Errorf("%w: capture status %s", ErrInvalid, c.Status)
	}
	if err := s.HandleCaptureCompleted(ctx, orderID, c.CaptureID, c.AmountCents, c.PayerEmail); err != nil {
		return orderID, err
	}
	if s.Simulated() {
		// Exercise the webhook path too, exactly as PayPal would deliver it.
		_ = s.InjectEvent(ctx, "PAYMENT.CAPTURE.COMPLETED", map[string]any{
			"id": c.CaptureID, "status": "COMPLETED", "amount": map[string]string{"currency_code": "USD", "value": money.Format(c.AmountCents)},
			"supplementary_data": map[string]any{"related_ids": map[string]string{"order_id": paypalOrderID}},
		})
	}
	return orderID, nil
}

// HandleCaptureCompleted is the single idempotent handler for a completed capture (API response or webhook).
func (s *Service) HandleCaptureCompleted(ctx context.Context, orderID, captureID string, amount int64, payer string) error {
	return s.DB.Tx(ctx, func(q db.Q) error {
		var status, mode string
		var orderAmount int64
		var rate, fee int
		if err := q.QueryRow(ctx, `SELECT status, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode FROM orders WHERE id=$1 FOR UPDATE`, orderID).
			Scan(&status, &orderAmount, &rate, &fee, &mode); err != nil {
			return err
		}
		if status != "CREATED" && status != "APPROVED" {
			return nil // already booked
		}
		if amount != orderAmount {
			return fmt.Errorf("capture amount %d does not match order amount %d", amount, orderAmount)
		}
		var vesting int
		if err := q.QueryRow(ctx, `SELECT m.vesting_window_seconds FROM orders o JOIN merchants m ON m.id=o.merchant_id WHERE o.id=$1`, orderID).Scan(&vesting); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE orders SET status='CAPTURED', paypal_capture_id=$2, captured_at=now(), vests_at=now()+make_interval(secs=>$3), buyer_email=$4 WHERE id=$1`,
			orderID, captureID, vesting, payer); err != nil {
			return err
		}
		commission := money.MulBps(orderAmount, rate)
		if _, err := ledger.Post(ctx, q, ledger.Entry{Type: ledger.CommissionReserved, OrderID: orderID, From: ledger.MerchantFunding, To: ledger.OrderCommission,
			Amount: commission, IdemKey: ledger.Key("reserve", orderID), Ref: captureID, Memo: "commission reserved at capture", Simulated: mode != "sandbox"}); err != nil {
			return err
		}
		if err := attribution.Enqueue(ctx, q, orderID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE decisions SET status='PURCHASED' WHERE id=(SELECT decision_id FROM orders WHERE id=$1)`, orderID); err != nil {
			return err
		}
		return jobs.Enqueue(ctx, q, jobs.TypeAttribution, map[string]string{"order_id": orderID}, "attr:"+orderID, 0)
	})
}

// RefundResult describes a refund.
type RefundResult struct {
	RefundID    string `json:"refund_id"`
	AmountCents int64  `json:"amount_cents"`
	Simulated   bool   `json:"simulated"`
}

// Refund refunds all (amount 0) or part of an order through PayPal (spec 8.3).
func (s *Service) Refund(ctx context.Context, orderID string, amount int64, reason, idemKey string) (RefundResult, error) {
	var captureID *string
	var total, refunded int64
	var status string
	var seeded bool
	err := s.DB.Pool.QueryRow(ctx, `SELECT paypal_capture_id, amount_cents, refunded_cents, status, seeded FROM orders WHERE id=$1`, orderID).
		Scan(&captureID, &total, &refunded, &status, &seeded)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{}, fmt.Errorf("%w: order %s", ErrNotFound, orderID)
	}
	if err != nil {
		return RefundResult{}, err
	}
	if status != "CAPTURED" && status != "PARTIALLY_REFUNDED" {
		return RefundResult{}, fmt.Errorf("%w (status %s)", ErrNotRefundable, status)
	}
	remaining := total - refunded
	if amount == 0 {
		amount = remaining
	}
	if amount <= 0 || amount > remaining {
		return RefundResult{}, fmt.Errorf("%w: refund must be between $0.01 and $%s", ErrInvalid, money.Format(remaining))
	}
	if seeded || captureID == nil {
		// Seeded history has no real PayPal capture: book the refund locally, clearly labelled.
		id := "seeded-refund-" + newID("")
		return RefundResult{RefundID: id, AmountCents: amount, Simulated: true}, s.HandleRefund(ctx, orderID, id, amount, reason, true)
	}
	if idemKey == "" {
		idemKey = fmt.Sprintf("refund-%s-%d-%d", orderID, refunded, amount)
	}
	r, err := s.PP.RefundCapture(ctx, *captureID, amount, idemKey, reason)
	if err != nil {
		return RefundResult{}, fmt.Errorf("paypal refund: %w", err)
	}
	if err := s.HandleRefund(ctx, orderID, r.ID, r.AmountCents, reason, s.Simulated()); err != nil {
		return RefundResult{}, err
	}
	if s.Simulated() {
		_ = s.InjectEvent(ctx, "PAYMENT.CAPTURE.REFUNDED", map[string]any{
			"id": r.ID, "status": "COMPLETED", "amount": map[string]string{"currency_code": "USD", "value": money.Format(r.AmountCents)},
			"links": []map[string]string{{"rel": "up", "href": "https://api-m.sandbox.paypal.com/v2/payments/captures/" + *captureID}},
		})
	}
	return RefundResult{RefundID: r.ID, AmountCents: r.AmountCents, Simulated: s.Simulated()}, nil
}

// HandleRefund is the single idempotent refund handler, keyed by refund id (spec 5.3 step 10).
func (s *Service) HandleRefund(ctx context.Context, orderID, refundID string, amount int64, reason string, simulated bool) error {
	return s.DB.Tx(ctx, func(q db.Q) error {
		var total, refunded int64
		if err := q.QueryRow(ctx, `SELECT amount_cents, refunded_cents FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&total, &refunded); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `INSERT INTO refunds (id, order_id, paypal_refund_id, amount_cents, reason, simulated) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (paypal_refund_id) DO NOTHING`, newID("rf_"), orderID, refundID, amount, reason, simulated)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // this refund was already booked
		}
		refunded = min(total, refunded+amount)
		status := "PARTIALLY_REFUNDED"
		if refunded >= total {
			status = "REFUNDED"
		}
		if _, err := q.Exec(ctx, `UPDATE orders SET refunded_cents=$2, status=$3 WHERE id=$1`, orderID, refunded, status); err != nil {
			return err
		}
		return s.reconcile(ctx, q, orderID)
	})
}

// Dispute actions (spec 7.4).
const (
	DisputeOpen          = "open"
	DisputeResolveSeller = "resolve_seller"
	DisputeResolveBuyer  = "resolve_buyer"
)

// HandleDispute applies a dispute event (real webhook or labelled simulation).
func (s *Service) HandleDispute(ctx context.Context, orderID, action string, simulated bool) error {
	var next string
	switch action {
	case DisputeOpen:
		next = "OPEN"
	case DisputeResolveSeller:
		next = "RESOLVED_SELLER"
	case DisputeResolveBuyer:
		next = "RESOLVED_BUYER"
	default:
		return fmt.Errorf("%w: unknown dispute action %q", ErrInvalid, action)
	}
	return s.DB.Tx(ctx, func(q db.Q) error {
		var status, dispute string
		if err := q.QueryRow(ctx, `SELECT status, dispute_status FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&status, &dispute); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: order %s", ErrNotFound, orderID)
			}
			return err
		}
		if status == "CREATED" || status == "APPROVED" {
			return fmt.Errorf("%w: order is not captured", ErrInvalid)
		}
		if (next == "RESOLVED_SELLER" || next == "RESOLVED_BUYER") && dispute != "OPEN" {
			return fmt.Errorf("%w: no open dispute to resolve", ErrInvalid)
		}
		if next == "OPEN" && dispute == "OPEN" {
			return nil
		}
		if _, err := q.Exec(ctx, `UPDATE orders SET dispute_status=$2, dispute_simulated = dispute_simulated OR $3 WHERE id=$1`, orderID, next, simulated); err != nil {
			return err
		}
		return s.reconcile(ctx, q, orderID)
	})
}

// Reconcile re-derives an order's ledger position from the facts. Safe to call any time.
func (s *Service) Reconcile(ctx context.Context, orderID string) error {
	return s.DB.Tx(ctx, func(q db.Q) error { return s.reconcile(ctx, q, orderID) })
}

type party struct {
	creatorID string // "" = Sway's fee
	accrued   int64
}

func (s *Service) reconcile(ctx context.Context, q db.Q, orderID string) error {
	var total, refunded int64
	var dispute, mode string
	var vestedAt *time.Time
	if err := q.QueryRow(ctx, `SELECT amount_cents, refunded_cents, dispute_status, payment_mode, vested_at FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&total, &refunded, &dispute, &mode, &vestedAt); err != nil {
		return err
	}
	lostNum := refunded
	if dispute == "RESOLVED_BUYER" {
		lostNum = total // a dispute lost to the buyer is treated as a full refund
	}
	simulated := mode != "sandbox"

	// Accruals per creator (ordered) and the fee.
	rows, err := q.Query(ctx, `SELECT COALESCE(creator_id,''), SUM(amount_cents)::bigint FROM ledger_entries WHERE order_id=$1 AND type = ANY($2)
		GROUP BY creator_id ORDER BY creator_id NULLS FIRST`, orderID, []string{ledger.AccruePending, ledger.SwayFeePending})
	if err != nil {
		return err
	}
	var creators []party
	var fee party
	for rows.Next() {
		var p party
		if err := rows.Scan(&p.creatorID, &p.accrued); err != nil {
			rows.Close()
			return err
		}
		if p.creatorID == "" {
			fee = p
		} else {
			creators = append(creators, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(creators) == 0 && fee.accrued == 0 {
		return nil // not attributed yet; attribution calls reconcile when it finishes
	}

	// Target cumulative loss per creator: the refunded fraction of each accrual, largest-remainder exact.
	var totalAccrued int64
	weights := make([]float64, len(creators))
	for i, c := range creators {
		totalAccrued += c.accrued
		weights[i] = float64(c.accrued)
	}
	targets := make([]int64, len(creators))
	if lostNum > 0 && totalAccrued > 0 {
		if targets, err = money.Allocate(money.MulRatio(totalAccrued, lostNum, total), weights); err != nil {
			return err
		}
	}
	var entries []ledger.Entry
	for i, c := range creators {
		es, err := s.reconcileParty(ctx, q, orderID, c.creatorID, targets[i], dispute, vestedAt != nil, simulated)
		if err != nil {
			return err
		}
		entries = append(entries, es...)
	}
	if fee.accrued > 0 {
		es, err := s.reconcileParty(ctx, q, orderID, "", money.MulRatio(fee.accrued, lostNum, total), dispute, vestedAt != nil, simulated)
		if err != nil {
			return err
		}
		entries = append(entries, es...)
	}
	_, err = ledger.Post(ctx, q, entries...)
	return err
}

func (s *Service) reconcileParty(ctx context.Context, q db.Q, orderID, creatorID string, target int64, dispute string, vested, simulated bool) ([]ledger.Entry, error) {
	pending, frozen, vestedAcct := ledger.CreatorPending, ledger.CreatorFrozen, ledger.CreatorVested
	label := creatorID
	if creatorID == "" {
		pending, frozen, vestedAcct = ledger.SwayPending, ledger.SwayFrozen, ledger.SwayVested
		label = "sway"
	}
	// Balances for this party within this order.
	bal := map[string]int64{}
	var lost, clawed, freezes, unfreezes int64
	rows, err := q.Query(ctx, `SELECT from_account, to_account, amount_cents, type FROM ledger_entries WHERE order_id=$1 AND COALESCE(creator_id,'')=$2`, orderID, creatorID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var from, to, typ string
		var amt int64
		if err := rows.Scan(&from, &to, &amt, &typ); err != nil {
			rows.Close()
			return nil, err
		}
		bal[from] -= amt
		bal[to] += amt
		switch typ {
		case ledger.Forfeit:
			lost += amt
		case ledger.Clawback:
			lost += amt
			clawed += amt
		case ledger.Freeze:
			freezes++
		case ledger.Unfreeze:
			unfreezes++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []ledger.Entry
	need := target - lost
	forfeit := func(bucket string, amt int64) {
		out = append(out, ledger.Entry{Type: ledger.Forfeit, OrderID: orderID, CreatorID: creatorID, From: bucket, To: ledger.MerchantRecovered,
			Amount: amt, IdemKey: ledger.Key("forfeit", orderID, label, bucket, fmt.Sprint(lost)), Memo: "purchase refunded or disputed: commission forfeited", Simulated: simulated})
		lost += amt
		bal[bucket] -= amt
	}
	for _, bucket := range []string{pending, frozen, vestedAcct} {
		if need <= 0 {
			break
		}
		if x := min(need, bal[bucket]); x > 0 {
			forfeit(bucket, x)
			need -= x
		}
	}
	if need > 0 && creatorID != "" {
		// Money already paid out. Unclaimed items are cancelled first (money comes back, then forfeits);
		// claimed money becomes a clawback the creator owes against future payouts. We never pull money back.
		unclaimed, err := s.unclaimedItemsFor(ctx, q, orderID, creatorID)
		if err != nil {
			return nil, err
		}
		if len(unclaimed) > 0 {
			for _, item := range unclaimed {
				if err := jobs.Enqueue(ctx, q, jobs.TypePayoutCancel, map[string]string{"item_id": item}, "cancel:"+item, 0); err != nil {
					return nil, err
				}
			}
			s.wake()
		} else if x := min(need, bal[ledger.CreatorPaid]-clawed); x > 0 {
			out = append(out, ledger.Entry{Type: ledger.Clawback, OrderID: orderID, CreatorID: creatorID, From: ledger.CreatorOwed, To: ledger.MerchantRecovered,
				Amount: x, IdemKey: ledger.Key("clawback", orderID, label, fmt.Sprint(lost)), Memo: "refund after payout: netted against future payouts", Simulated: simulated})
			lost += x
		}
	}
	// Freeze while a dispute is open; release back where the money came from when it closes.
	if dispute == "OPEN" {
		for _, bucket := range []string{pending, vestedAcct} {
			if x := bal[bucket]; x > 0 {
				out = append(out, ledger.Entry{Type: ledger.Freeze, OrderID: orderID, CreatorID: creatorID, From: bucket, To: frozen, Amount: x,
					IdemKey: ledger.Key("freeze", orderID, label, bucket, fmt.Sprint(freezes)), Memo: "dispute opened: frozen", Simulated: simulated})
				freezes++
				bal[frozen] += x
				bal[bucket] = 0
			}
		}
	} else if x := bal[frozen]; x > 0 {
		dest := pending
		if vested {
			dest = vestedAcct
		}
		out = append(out, ledger.Entry{Type: ledger.Unfreeze, OrderID: orderID, CreatorID: creatorID, From: frozen, To: dest, Amount: x,
			IdemKey: ledger.Key("unfreeze", orderID, label, fmt.Sprint(unfreezes)), Memo: "dispute resolved: released", Simulated: simulated})
	}
	return out, nil
}

func (s *Service) unclaimedItemsFor(ctx context.Context, q db.Q, orderID, creatorID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT pi.id FROM ledger_entries le JOIN payout_items pi ON pi.id = le.ref
		WHERE le.order_id=$1 AND le.creator_id=$2 AND le.type=$3 AND pi.status='UNCLAIMED'`, orderID, creatorID, ledger.PayoutSent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// VestDue vests every order whose return window has passed cleanly (spec 5.3 step 7). Returns orders vested.
func (s *Service) VestDue(ctx context.Context) (int, error) {
	rows, err := s.DB.Pool.Query(ctx, `SELECT o.id FROM orders o JOIN attribution_jobs a ON a.order_id=o.id AND a.status='DONE'
		WHERE o.vested_at IS NULL AND o.captured_at IS NOT NULL AND o.vests_at <= now() AND o.dispute_status <> 'OPEN' ORDER BY o.vests_at LIMIT 500`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		err := s.DB.Tx(ctx, func(q db.Q) error {
			var vestedAt *time.Time
			var dispute, mode string
			if err := q.QueryRow(ctx, `SELECT vested_at, dispute_status, payment_mode FROM orders WHERE id=$1 FOR UPDATE`, id).Scan(&vestedAt, &dispute, &mode); err != nil {
				return err
			}
			if vestedAt != nil || dispute == "OPEN" {
				return nil
			}
			pc, err := ledger.OrderCreatorBalances(ctx, q, id)
			if err != nil {
				return err
			}
			var entries []ledger.Entry
			for creator, b := range pc {
				if x := b[ledger.CreatorPending]; x > 0 {
					entries = append(entries, ledger.Entry{Type: ledger.Vest, OrderID: id, CreatorID: creator, From: ledger.CreatorPending, To: ledger.CreatorVested,
						Amount: x, IdemKey: ledger.Key("vest", id, creator), Memo: "return window passed with no refund or dispute", Simulated: mode != "sandbox"})
				}
			}
			ob, err := ledger.BalancesFor(ctx, q, ledger.Scope{OrderID: id})
			if err != nil {
				return err
			}
			if x := ob[ledger.SwayPending]; x > 0 {
				entries = append(entries, ledger.Entry{Type: ledger.Vest, OrderID: id, From: ledger.SwayPending, To: ledger.SwayVested,
					Amount: x, IdemKey: ledger.Key("vest", id, "sway"), Memo: "Sway fee vested", Simulated: mode != "sandbox"})
			}
			if _, err := ledger.Post(ctx, q, entries...); err != nil {
				return err
			}
			_, err = q.Exec(ctx, `UPDATE orders SET vested_at=now() WHERE id=$1`, id)
			return err
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FastForward makes every captured order vest-eligible now (demo only, spec 10).
func (s *Service) FastForward(ctx context.Context) (int64, error) {
	if !s.Cfg.DemoMode {
		return 0, ErrDemoOnly
	}
	tag, err := s.DB.Pool.Exec(ctx, `UPDATE orders SET vests_at=now() WHERE vested_at IS NULL AND captured_at IS NOT NULL AND vests_at > now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ProductName is a small helper for payout notes.
func ProductName(ctx context.Context, q db.Q, productID string) string {
	p, err := catalog.GetProduct(ctx, q, productID)
	if err != nil {
		return productID
	}
	return p.Name
}

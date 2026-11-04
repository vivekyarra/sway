// Package views builds the read models behind the Shopper, Creator and Merchant screens.
// Every figure is derived from the ledger and source tables at read time; nothing here writes.
package views

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/payments"
)

var ErrNotFound = errors.New("not found")

// AttributionRow is one source's share of one order.
type AttributionRow struct {
	SourceID    string  `json:"source_id"`
	SourceTitle string  `json:"source_title"`
	SourceType  string  `json:"source_type"`
	CreatorID   string  `json:"creator_id"`
	CreatorName string  `json:"creator_name"`
	AvatarHue   int     `json:"avatar_hue"`
	Phi         float64 `json:"phi"`
	PhiStd      float64 `json:"phi_stddev"`
	LOO         float64 `json:"loo_delta"`
	Weight      float64 `json:"weight"`
	AmountCents int64   `json:"amount_cents"`
	Role        string  `json:"role"`
	Explanation *string `json:"explanation"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason"`
}

// AttributionJob is the job progress and summary.
type AttributionJob struct {
	Status        string          `json:"status"`
	Phase         string          `json:"phase"`
	Progress      int             `json:"progress"`
	TotalRuns     int             `json:"total_runs"`
	CacheHits     int             `json:"cache_hits"`
	Method        string          `json:"method"`
	Approximate   bool            `json:"approximate"`
	VFull         *float64        `json:"v_full"`
	VNone         *float64        `json:"v_none"`
	Share         *float64        `json:"share"`
	EfficiencyGap *float64        `json:"efficiency_gap"`
	Error         string          `json:"error,omitempty"`
	Coalitions    json.RawMessage `json:"coalitions,omitempty"`
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
}

// Order is the order page model (spec 4.3).
type Order struct {
	ID              string              `json:"id"`
	DecisionID      *string             `json:"decision_id"`
	ProductID       string              `json:"product_id"`
	ProductName     string              `json:"product_name"`
	ProductBrand    string              `json:"product_brand"`
	Request         string              `json:"request"`
	Rationale       string              `json:"rationale"`
	AmountCents     int64               `json:"amount_cents"`
	RefundedCents   int64               `json:"refunded_cents"`
	Status          string              `json:"status"`
	DisputeStatus   string              `json:"dispute_status"`
	DisputeSim      bool                `json:"dispute_simulated"`
	PaymentMode     string              `json:"payment_mode"`
	Seeded          bool                `json:"seeded"`
	PayPalOrderID   *string             `json:"paypal_order_id"`
	PayPalCaptureID *string             `json:"paypal_capture_id"`
	BuyerEmail      string              `json:"buyer_email"`
	CapturedAt      *time.Time          `json:"captured_at"`
	VestsAt         *time.Time          `json:"vests_at"`
	VestedAt        *time.Time          `json:"vested_at"`
	CreatedAt       time.Time           `json:"created_at"`
	Job             *AttributionJob     `json:"attribution"`
	Attributions    []AttributionRow    `json:"attributions"`
	Split           ledger.OrderSummary `json:"split"`
	Refunds         []Refund            `json:"refunds"`
	Ledger          []ledger.Entry      `json:"ledger"`
}

type Refund struct {
	PayPalRefundID string    `json:"paypal_refund_id"`
	AmountCents    int64     `json:"amount_cents"`
	Reason         string    `json:"reason"`
	Simulated      bool      `json:"simulated"`
	CreatedAt      time.Time `json:"created_at"`
}

// eventStatus labels where one creator's money for one order currently sits.
func eventStatus(b ledger.Balances, accrued, forfeited, clawed int64) string {
	switch {
	case accrued == 0:
		return "No earnings"
	case b[ledger.CreatorFrozen] > 0:
		return "Frozen"
	case b[ledger.CreatorPending] > 0:
		return "Pending"
	case b[ledger.CreatorVested] > 0:
		return "Vested"
	case clawed > 0:
		return "Clawed back"
	case b[ledger.CreatorPaid] > 0:
		return "Paid"
	case forfeited >= accrued:
		return "Forfeited"
	}
	return "Settled"
}

// GetOrder loads everything the order page shows.
func GetOrder(ctx context.Context, q db.Q, id string) (Order, error) {
	var o Order
	err := q.QueryRow(ctx, `SELECT o.id, o.decision_id, o.product_id, p.name, p.brand, COALESCE(d.request_text,''), COALESCE(d.rationale,''), o.amount_cents, o.refunded_cents,
		o.status, o.dispute_status, o.dispute_simulated, o.payment_mode, o.seeded, o.paypal_order_id, o.paypal_capture_id, o.buyer_email, o.captured_at, o.vests_at, o.vested_at, o.created_at
		FROM orders o JOIN products p ON p.id=o.product_id LEFT JOIN decisions d ON d.id=o.decision_id WHERE o.id=$1`, id).
		Scan(&o.ID, &o.DecisionID, &o.ProductID, &o.ProductName, &o.ProductBrand, &o.Request, &o.Rationale, &o.AmountCents, &o.RefundedCents,
			&o.Status, &o.DisputeStatus, &o.DisputeSim, &o.PaymentMode, &o.Seeded, &o.PayPalOrderID, &o.PayPalCaptureID, &o.BuyerEmail, &o.CapturedAt, &o.VestsAt, &o.VestedAt, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	var j AttributionJob
	err = q.QueryRow(ctx, `SELECT status, phase, progress, total_runs, cache_hits, method, approximate, v_full, v_none, share, efficiency_gap, error, COALESCE(coalitions,'null'::jsonb), started_at, finished_at
		FROM attribution_jobs WHERE order_id=$1`, id).Scan(&j.Status, &j.Phase, &j.Progress, &j.TotalRuns, &j.CacheHits, &j.Method, &j.Approximate, &j.VFull, &j.VNone, &j.Share,
		&j.EfficiencyGap, &j.Error, &j.Coalitions, &j.StartedAt, &j.FinishedAt)
	if err == nil {
		o.Job = &j
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return o, err
	}
	if o.Attributions, err = orderAttributions(ctx, q, id); err != nil {
		return o, err
	}
	if o.Split, err = ledger.SummarizeOrder(ctx, q, id); err != nil {
		return o, err
	}
	rows, err := q.Query(ctx, `SELECT paypal_refund_id, amount_cents, reason, simulated, created_at FROM refunds WHERE order_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var r Refund
		if err := rows.Scan(&r.PayPalRefundID, &r.AmountCents, &r.Reason, &r.Simulated, &r.CreatedAt); err != nil {
			rows.Close()
			return o, err
		}
		o.Refunds = append(o.Refunds, r)
	}
	rows.Close()
	o.Ledger, err = Entries(ctx, q, "WHERE order_id=$1", id)
	return o, err
}

func orderAttributions(ctx context.Context, q db.Q, orderID string) ([]AttributionRow, error) {
	rows, err := q.Query(ctx, `SELECT a.source_id, s.title, s.type, a.creator_id, c.display_name, c.avatar_hue, a.phi, a.phi_stddev, a.loo_delta, a.weight, a.amount_cents, a.role, a.explanation
		FROM attributions a JOIN sources s ON s.id=a.source_id JOIN creators c ON c.id=a.creator_id WHERE a.order_id=$1 ORDER BY a.phi DESC, a.source_id`, orderID)
	if err != nil {
		return nil, err
	}
	var out []AttributionRow
	for rows.Next() {
		var r AttributionRow
		if err := rows.Scan(&r.SourceID, &r.SourceTitle, &r.SourceType, &r.CreatorID, &r.CreatorName, &r.AvatarHue, &r.Phi, &r.PhiStd, &r.LOO, &r.Weight, &r.AmountCents, &r.Role, &r.Explanation); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pc, err := ledger.OrderCreatorBalances(ctx, q, orderID)
	if err != nil {
		return nil, err
	}
	lost, err := lostByCreator(ctx, q, orderID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		r := &out[i]
		l := lost[r.CreatorID]
		if r.AmountCents == 0 {
			r.Status = "No earnings"
		} else {
			r.Status = eventStatus(pc[r.CreatorID], l.accrued, l.forfeited, l.clawed)
		}
		r.Reason = reasonFor(r)
	}
	return out, nil
}

func reasonFor(r *AttributionRow) string {
	switch {
	case r.Role == "background":
		return "Background source: small effect on this decision"
	case r.AmountCents > 0:
		return "Positive causal influence on the purchase"
	case r.Phi < -0.01:
		return "Argued for a different product (negative influence)"
	default:
		return "No measurable influence on this decision"
	}
}

type lostAgg struct{ accrued, forfeited, clawed int64 }

func lostByCreator(ctx context.Context, q db.Q, orderID string) (map[string]lostAgg, error) {
	rows, err := q.Query(ctx, `SELECT creator_id,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$2),0)::bigint, COALESCE(SUM(amount_cents) FILTER (WHERE type=$3),0)::bigint, COALESCE(SUM(amount_cents) FILTER (WHERE type=$4),0)::bigint
		FROM ledger_entries WHERE order_id=$1 AND creator_id IS NOT NULL GROUP BY creator_id`, orderID, ledger.AccruePending, ledger.Forfeit, ledger.Clawback)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]lostAgg{}
	for rows.Next() {
		var c string
		var a lostAgg
		if err := rows.Scan(&c, &a.accrued, &a.forfeited, &a.clawed); err != nil {
			return nil, err
		}
		out[c] = a
	}
	return out, rows.Err()
}

// Entries lists ledger entries (newest first, capped).
func Entries(ctx context.Context, q db.Q, where string, args ...any) ([]ledger.Entry, error) {
	rows, err := q.Query(ctx, `SELECT seq, id, idem_key, type, COALESCE(order_id,''), COALESCE(creator_id,''), from_account, to_account, amount_cents, signed_cents,
		ref, memo, simulated, prev_hash, hash, created_at FROM ledger_entries `+where+` ORDER BY seq DESC LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Entry
	for rows.Next() {
		var e ledger.Entry
		if err := rows.Scan(&e.Seq, &e.ID, &e.IdemKey, &e.Type, &e.OrderID, &e.CreatorID, &e.From, &e.To, &e.Amount, &e.Signed, &e.Ref, &e.Memo, &e.Simulated, &e.PrevHash, &e.Hash, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Creator dashboard (spec 4.4).
type Creator struct {
	ID          string                   `json:"id"`
	DisplayName string                   `json:"display_name"`
	PayPalEmail string                   `json:"paypal_email"`
	Verified    bool                     `json:"verified"`
	Bio         string                   `json:"bio"`
	AvatarHue   int                      `json:"avatar_hue"`
	Balances    payments.CreatorBalances `json:"balances"`
	Swayed      int                      `json:"purchases_swayed"`
	Headline    string                   `json:"headline"`
	Events      []CreatorEvent           `json:"events"`
	Sources     []CreatorSource          `json:"sources"`
	Payouts     []PayoutItem             `json:"payouts"`
}

type CreatorEvent struct {
	OrderID     string    `json:"order_id"`
	Date        time.Time `json:"date"`
	ProductName string    `json:"product_name"`
	SourceID    string    `json:"source_id"`
	SourceTitle string    `json:"source_title"`
	Phi         float64   `json:"phi"`
	PhiStd      float64   `json:"phi_stddev"`
	Share       float64   `json:"contribution_pct"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	Explanation *string   `json:"explanation"`
	Seeded      bool      `json:"seeded"`
	Simulated   bool      `json:"simulated"`
}

type CreatorSource struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Type         string  `json:"type"`
	PublishedAt  string  `json:"published_at"`
	Status       string  `json:"status"`
	StatusReason string  `json:"status_reason"`
	Influenced   int     `json:"purchases_influenced"`
	Returned     int     `json:"purchases_returned"`
	ReturnRate   float64 `json:"return_rate"`
	EarnedCents  int64   `json:"earned_cents"`
}

type PayoutItem struct {
	ID            string    `json:"id"`
	BatchID       string    `json:"batch_id"`
	CreatorID     string    `json:"creator_id"`
	CreatorName   string    `json:"creator_name"`
	Receiver      string    `json:"receiver"`
	AmountCents   int64     `json:"amount_cents"`
	OffsetCents   int64     `json:"offset_cents"`
	Status        string    `json:"status"`
	PayPalItemID  *string   `json:"paypal_payout_item_id"`
	PayPalBatchID *string   `json:"paypal_payout_batch_id"`
	Mode          string    `json:"mode"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// GetCreator builds the creator dashboard.
func GetCreator(ctx context.Context, q db.Q, id string) (Creator, error) {
	var c Creator
	err := q.QueryRow(ctx, `SELECT id, display_name, paypal_email, verified, bio, avatar_hue FROM creators WHERE id=$1`, id).
		Scan(&c.ID, &c.DisplayName, &c.PayPalEmail, &c.Verified, &c.Bio, &c.AvatarHue)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if c.Balances, err = payments.Balances(ctx, q, id); err != nil {
		return c, err
	}
	rows, err := q.Query(ctx, `SELECT a.order_id, COALESCE(o.captured_at, o.created_at), p.name, a.source_id, s.title, a.phi, a.phi_stddev, a.weight, a.amount_cents, a.role, a.explanation, o.seeded, o.payment_mode
		FROM attributions a JOIN orders o ON o.id=a.order_id JOIN products p ON p.id=o.product_id JOIN sources s ON s.id=a.source_id
		WHERE a.creator_id=$1 ORDER BY COALESCE(o.captured_at, o.created_at) DESC, a.source_id`, id)
	if err != nil {
		return c, err
	}
	var events []CreatorEvent
	orders := map[string]bool{}
	for rows.Next() {
		var e CreatorEvent
		var role, mode string
		if err := rows.Scan(&e.OrderID, &e.Date, &e.ProductName, &e.SourceID, &e.SourceTitle, &e.Phi, &e.PhiStd, &e.Share, &e.AmountCents, &role, &e.Explanation, &e.Seeded, &mode); err != nil {
			rows.Close()
			return c, err
		}
		e.Simulated = mode != "sandbox"
		e.Share = round2(e.Share * 100)
		r := AttributionRow{Role: role, AmountCents: e.AmountCents, Phi: e.Phi}
		e.Reason = reasonFor(&r)
		events = append(events, e)
		if e.AmountCents > 0 {
			orders[e.OrderID] = true
		}
	}
	rows.Close()
	statusCache := map[string]string{}
	for i := range events {
		e := &events[i]
		if e.AmountCents == 0 {
			e.Status = "No earnings"
			continue
		}
		if st, ok := statusCache[e.OrderID]; ok {
			e.Status = st
			continue
		}
		pc, err := ledger.OrderCreatorBalances(ctx, q, e.OrderID)
		if err != nil {
			return c, err
		}
		lost, err := lostByCreator(ctx, q, e.OrderID)
		if err != nil {
			return c, err
		}
		l := lost[id]
		e.Status = eventStatus(pc[id], l.accrued, l.forfeited, l.clawed)
		statusCache[e.OrderID] = e.Status
	}
	c.Events = events
	c.Swayed = len(orders)
	plural := "s"
	if c.Swayed == 1 {
		plural = ""
	}
	c.Headline = fmt.Sprintf("Your work swayed %d purchase%s. %s pending, %s paid.", c.Swayed, plural, usd(c.Balances.Pending+c.Balances.Frozen), usd(c.Balances.Paid))

	srows, err := q.Query(ctx, `SELECT s.id, s.title, s.type, to_char(s.published_at,'YYYY-MM-DD'), s.status, s.status_reason,
		COUNT(DISTINCT a.order_id) FILTER (WHERE a.amount_cents > 0),
		COUNT(DISTINCT a.order_id) FILTER (WHERE a.amount_cents > 0 AND o.refunded_cents > 0),
		COALESCE(SUM(a.amount_cents),0)::bigint
		FROM sources s LEFT JOIN attributions a ON a.source_id=s.id LEFT JOIN orders o ON o.id=a.order_id
		WHERE s.creator_id=$1 GROUP BY s.id ORDER BY s.published_at DESC`, id)
	if err != nil {
		return c, err
	}
	for srows.Next() {
		var s CreatorSource
		if err := srows.Scan(&s.ID, &s.Title, &s.Type, &s.PublishedAt, &s.Status, &s.StatusReason, &s.Influenced, &s.Returned, &s.EarnedCents); err != nil {
			srows.Close()
			return c, err
		}
		if s.Influenced > 0 {
			s.ReturnRate = round2(float64(s.Returned) / float64(s.Influenced))
		}
		c.Sources = append(c.Sources, s)
	}
	srows.Close()
	c.Payouts, err = Payouts(ctx, q, "WHERE pi.creator_id=$1", id)
	return c, err
}

// Payouts lists payout items.
func Payouts(ctx context.Context, q db.Q, where string, args ...any) ([]PayoutItem, error) {
	rows, err := q.Query(ctx, `SELECT pi.id, pi.batch_id, pi.creator_id, c.display_name, pi.receiver, pi.amount_cents, pi.offset_cents, pi.status,
		pi.paypal_payout_item_id, pb.paypal_payout_batch_id, pb.mode, pi.error, pi.created_at
		FROM payout_items pi JOIN payout_batches pb ON pb.id=pi.batch_id JOIN creators c ON c.id=pi.creator_id `+where+` ORDER BY pi.created_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PayoutItem
	for rows.Next() {
		var p PayoutItem
		if err := rows.Scan(&p.ID, &p.BatchID, &p.CreatorID, &p.CreatorName, &p.Receiver, &p.AmountCents, &p.OffsetCents, &p.Status, &p.PayPalItemID, &p.PayPalBatchID, &p.Mode, &p.Error, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatorSummary is one row of the creators index.
type CreatorSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	AvatarHue   int    `json:"avatar_hue"`
	Sources     int    `json:"sources"`
	Flagged     int    `json:"flagged_sources"`
	EarnedCents int64  `json:"earned_cents"`
	PaidCents   int64  `json:"paid_cents"`
}

func ListCreators(ctx context.Context, q db.Q) ([]CreatorSummary, error) {
	rows, err := q.Query(ctx, `SELECT c.id, c.display_name, c.bio, c.avatar_hue,
		(SELECT count(*) FROM sources s WHERE s.creator_id=c.id), (SELECT count(*) FROM sources s WHERE s.creator_id=c.id AND s.status<>'active'),
		COALESCE((SELECT SUM(amount_cents) FROM ledger_entries WHERE creator_id=c.id AND type=$1),0)::bigint,
		COALESCE((SELECT SUM(CASE WHEN to_account=$2 THEN amount_cents WHEN from_account=$2 THEN -amount_cents ELSE 0 END) FROM ledger_entries WHERE creator_id=c.id),0)::bigint
		FROM creators c ORDER BY 7 DESC, c.display_name`, ledger.AccruePending, ledger.CreatorPaid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreatorSummary
	for rows.Next() {
		var s CreatorSummary
		if err := rows.Scan(&s.ID, &s.DisplayName, &s.Bio, &s.AvatarHue, &s.Sources, &s.Flagged, &s.EarnedCents, &s.PaidCents); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Merchant overview (spec 4.5).
type Merchant struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	CommissionRateBps int                 `json:"commission_rate_bps"`
	SwayFeeBps        int                 `json:"sway_fee_bps"`
	VestingSeconds    int                 `json:"vesting_window_seconds"`
	KPIs              KPIs                `json:"kpis"`
	Content           []ContentRow        `json:"content"`
	Orders            []OrderRow          `json:"orders"`
	Payouts           []PayoutItem        `json:"payouts"`
	Batches           []Batch             `json:"batches"`
	Ledger            []ledger.Entry      `json:"ledger"`
	Webhooks          []Webhook           `json:"webhooks"`
	Verification      ledger.Verification `json:"verification"`
	Daily             []DailyPoint        `json:"daily"`
}

type KPIs struct {
	AgentRevenueCents   int64   `json:"agent_revenue_cents"`
	Orders              int     `json:"orders"`
	CommissionCents     int64   `json:"commission_cents"`
	CommissionPaidCents int64   `json:"commission_paid_cents"`
	ReturnedCents       int64   `json:"commission_returned_cents"`
	ForfeitedCents      int64   `json:"commission_forfeited_cents"`
	PendingCents        int64   `json:"commission_pending_cents"`
	SwayFeeCents        int64   `json:"sway_fee_cents"`
	ReturnRate          float64 `json:"influenced_return_rate"`
	AvgShare            float64 `json:"avg_influence_share"`
}

type ContentRow struct {
	SourceID     string    `json:"source_id"`
	Title        string    `json:"title"`
	Type         string    `json:"type"`
	CreatorID    string    `json:"creator_id"`
	CreatorName  string    `json:"creator_name"`
	Status       string    `json:"status"`
	StatusReason string    `json:"status_reason"`
	Influenced   int       `json:"purchases_influenced"`
	AvgPhi       float64   `json:"avg_contribution"`
	RevenueCents int64     `json:"revenue_influenced_cents"`
	ReturnRate   float64   `json:"return_rate"`
	VestedCents  int64     `json:"commission_vested_cents"`
	EarnedCents  int64     `json:"commission_earned_cents"`
	Spark        []float64 `json:"spark"`
}

type OrderRow struct {
	ID                string     `json:"id"`
	ProductName       string     `json:"product_name"`
	AmountCents       int64      `json:"amount_cents"`
	RefundedCents     int64      `json:"refunded_cents"`
	Status            string     `json:"status"`
	DisputeStatus     string     `json:"dispute_status"`
	DisputeSimulated  bool       `json:"dispute_simulated"`
	AttributionStatus string     `json:"attribution_status"`
	Share             *float64   `json:"share"`
	CreatorPoolCents  int64      `json:"creator_pool_cents"`
	FeeCents          int64      `json:"fee_cents"`
	ReturnedCents     int64      `json:"returned_to_merchant_cents"`
	CommissionCents   int64      `json:"commission_cents"`
	Balanced          bool       `json:"balanced"`
	PaymentMode       string     `json:"payment_mode"`
	Seeded            bool       `json:"seeded"`
	PayPalCaptureID   *string    `json:"paypal_capture_id"`
	CapturedAt        *time.Time `json:"captured_at"`
	VestsAt           *time.Time `json:"vests_at"`
	VestedAt          *time.Time `json:"vested_at"`
}

type Batch struct {
	ID            string    `json:"id"`
	SenderBatchID string    `json:"sender_batch_id"`
	PayPalBatchID *string   `json:"paypal_payout_batch_id"`
	Status        string    `json:"status"`
	Mode          string    `json:"mode"`
	TotalCents    int64     `json:"total_cents"`
	Items         int       `json:"items"`
	CreatedAt     time.Time `json:"created_at"`
}

type Webhook struct {
	EventID    string     `json:"paypal_event_id"`
	EventType  string     `json:"event_type"`
	ResourceID string     `json:"resource_id"`
	Verified   bool       `json:"verified"`
	Simulated  bool       `json:"simulated"`
	ReceivedAt time.Time  `json:"received_at"`
	Processed  *time.Time `json:"processed_at"`
	Outcome    string     `json:"outcome"`
}

type DailyPoint struct {
	Day          string `json:"day"`
	RevenueCents int64  `json:"revenue_cents"`
	PaidCents    int64  `json:"commission_cents"`
}

// GetMerchant builds the merchant dashboard.
func GetMerchant(ctx context.Context, q db.Q, swayFeeBps int) (Merchant, error) {
	var m Merchant
	err := q.QueryRow(ctx, `SELECT id, name, commission_rate_bps, vesting_window_seconds FROM merchants ORDER BY created_at LIMIT 1`).
		Scan(&m.ID, &m.Name, &m.CommissionRateBps, &m.VestingSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.SwayFeeBps = swayFeeBps
	k := &m.KPIs
	if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents - refunded_cents),0)::bigint, count(*) FROM orders WHERE captured_at IS NOT NULL`).Scan(&k.AgentRevenueCents, &k.Orders); err != nil {
		return m, err
	}
	if err := q.QueryRow(ctx, `SELECT
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$1),0)::bigint,
		COALESCE(SUM(CASE WHEN type=$2 THEN amount_cents WHEN type=$3 THEN -amount_cents ELSE 0 END),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$4),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type IN ($5,$6)),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$7),0)::bigint
		FROM ledger_entries`, ledger.CommissionReserved, ledger.PayoutSent, ledger.PayoutFailedReversal, ledger.ReturnToMerchant, ledger.Forfeit, ledger.Clawback, ledger.SwayFeePending).
		Scan(&k.CommissionCents, &k.CommissionPaidCents, &k.ReturnedCents, &k.ForfeitedCents, &k.SwayFeeCents); err != nil {
		return m, err
	}
	b, err := ledger.BalancesFor(ctx, q, ledger.Scope{})
	if err != nil {
		return m, err
	}
	k.PendingCents = b[ledger.CreatorPending] + b[ledger.CreatorFrozen] + b[ledger.CreatorVested]
	var influenced, returned int
	if err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE a.share > 0), count(*) FILTER (WHERE a.share > 0 AND o.refunded_cents > 0), COALESCE(avg(a.share),0)
		FROM attribution_jobs a JOIN orders o ON o.id=a.order_id WHERE a.status='DONE'`).Scan(&influenced, &returned, &k.AvgShare); err != nil {
		return m, err
	}
	if influenced > 0 {
		k.ReturnRate = round2(float64(returned) / float64(influenced))
	}
	k.AvgShare = round2(k.AvgShare)

	if m.Content, err = content(ctx, q); err != nil {
		return m, err
	}
	if m.Orders, err = orderRows(ctx, q); err != nil {
		return m, err
	}
	if m.Payouts, err = Payouts(ctx, q, ""); err != nil {
		return m, err
	}
	brows, err := q.Query(ctx, `SELECT pb.id, pb.sender_batch_id, pb.paypal_payout_batch_id, pb.status, pb.mode, pb.total_cents, (SELECT count(*) FROM payout_items WHERE batch_id=pb.id), pb.created_at
		FROM payout_batches pb WHERE pb.status <> 'EMPTY' ORDER BY pb.created_at DESC LIMIT 100`)
	if err != nil {
		return m, err
	}
	for brows.Next() {
		var x Batch
		if err := brows.Scan(&x.ID, &x.SenderBatchID, &x.PayPalBatchID, &x.Status, &x.Mode, &x.TotalCents, &x.Items, &x.CreatedAt); err != nil {
			brows.Close()
			return m, err
		}
		m.Batches = append(m.Batches, x)
	}
	brows.Close()
	if m.Ledger, err = Entries(ctx, q, ""); err != nil {
		return m, err
	}
	wrows, err := q.Query(ctx, `SELECT paypal_event_id, event_type, resource_id, verified, simulated, received_at, processed_at, outcome FROM webhook_events ORDER BY received_at DESC LIMIT 200`)
	if err != nil {
		return m, err
	}
	for wrows.Next() {
		var w Webhook
		if err := wrows.Scan(&w.EventID, &w.EventType, &w.ResourceID, &w.Verified, &w.Simulated, &w.ReceivedAt, &w.Processed, &w.Outcome); err != nil {
			wrows.Close()
			return m, err
		}
		m.Webhooks = append(m.Webhooks, w)
	}
	wrows.Close()
	if m.Verification, err = cachedVerify(ctx, q); err != nil {
		return m, err
	}
	drows, err := q.Query(ctx, `SELECT to_char(d,'MM-DD'),
		COALESCE((SELECT SUM(amount_cents - refunded_cents) FROM orders WHERE captured_at::date = d),0)::bigint,
		COALESCE((SELECT SUM(amount_cents) FROM ledger_entries WHERE type=$1 AND created_at::date = d),0)::bigint
		FROM generate_series(current_date - 13, current_date, interval '1 day') d ORDER BY d`, ledger.AccruePending)
	if err != nil {
		return m, err
	}
	for drows.Next() {
		var p DailyPoint
		if err := drows.Scan(&p.Day, &p.RevenueCents, &p.PaidCents); err != nil {
			drows.Close()
			return m, err
		}
		m.Daily = append(m.Daily, p)
	}
	drows.Close()
	return m, nil
}

func content(ctx context.Context, q db.Q) ([]ContentRow, error) {
	rows, err := q.Query(ctx, `SELECT s.id, s.title, s.type, c.id, c.display_name, s.status, s.status_reason,
		COUNT(DISTINCT a.order_id) FILTER (WHERE a.amount_cents > 0),
		COALESCE(AVG(a.phi) FILTER (WHERE a.role='player'),0),
		COALESCE(SUM(o.amount_cents) FILTER (WHERE a.amount_cents > 0),0)::bigint,
		COUNT(DISTINCT a.order_id) FILTER (WHERE a.amount_cents > 0 AND o.refunded_cents > 0),
		COALESCE(SUM(a.amount_cents),0)::bigint,
		COALESCE(array_agg(a.phi ORDER BY o.captured_at) FILTER (WHERE a.order_id IS NOT NULL), '{}')
		FROM sources s JOIN creators c ON c.id=s.creator_id LEFT JOIN attributions a ON a.source_id=s.id LEFT JOIN orders o ON o.id=a.order_id
		GROUP BY s.id, c.id ORDER BY 12 DESC, s.id`)
	if err != nil {
		return nil, err
	}
	var out []ContentRow
	for rows.Next() {
		var r ContentRow
		var returned int
		if err := rows.Scan(&r.SourceID, &r.Title, &r.Type, &r.CreatorID, &r.CreatorName, &r.Status, &r.StatusReason, &r.Influenced, &r.AvgPhi, &r.RevenueCents, &returned, &r.EarnedCents, &r.Spark); err != nil {
			rows.Close()
			return nil, err
		}
		if r.Influenced > 0 {
			r.ReturnRate = round2(float64(returned) / float64(r.Influenced))
		}
		r.AvgPhi = round2(r.AvgPhi * 100)
		for i := range r.Spark {
			r.Spark[i] = round2(r.Spark[i])
		}
		out = append(out, r)
	}
	rows.Close()
	// Vested commission per source: the vested-or-paid part of each source's accrual.
	vrows, err := q.Query(ctx, `SELECT ref, COALESCE(SUM(amount_cents),0)::bigint FROM ledger_entries le WHERE type=$1
		AND EXISTS (SELECT 1 FROM ledger_entries v WHERE v.order_id=le.order_id AND v.creator_id=le.creator_id AND v.type=$2) GROUP BY ref`, ledger.AccruePending, ledger.Vest)
	if err != nil {
		return nil, err
	}
	vested := map[string]int64{}
	for vrows.Next() {
		var ref string
		var amt int64
		if err := vrows.Scan(&ref, &amt); err != nil {
			vrows.Close()
			return nil, err
		}
		vested[ref] = amt
	}
	vrows.Close()
	for i := range out {
		out[i].VestedCents = vested[out[i].SourceID]
	}
	return out, nil
}

func orderRows(ctx context.Context, q db.Q) ([]OrderRow, error) {
	rows, err := q.Query(ctx, `SELECT o.id, p.name, o.amount_cents, o.refunded_cents, o.status, o.dispute_status, o.dispute_simulated, COALESCE(a.status,'—'), a.share,
		o.payment_mode, o.seeded, o.paypal_capture_id, o.captured_at, o.vests_at, o.vested_at
		FROM orders o JOIN products p ON p.id=o.product_id LEFT JOIN attribution_jobs a ON a.order_id=o.id
		WHERE o.captured_at IS NOT NULL ORDER BY o.captured_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	var out []OrderRow
	for rows.Next() {
		var r OrderRow
		if err := rows.Scan(&r.ID, &r.ProductName, &r.AmountCents, &r.RefundedCents, &r.Status, &r.DisputeStatus, &r.DisputeSimulated, &r.AttributionStatus, &r.Share,
			&r.PaymentMode, &r.Seeded, &r.PayPalCaptureID, &r.CapturedAt, &r.VestsAt, &r.VestedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	// One grouped aggregate for every order's split (no per-order queries).
	ids := make([]string, len(out))
	idx := make(map[string]int, len(out))
	for i, o := range out {
		ids[i] = o.ID
		idx[o.ID] = i
	}
	srows, err := q.Query(ctx, `SELECT order_id,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$2),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$3),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$4),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$5),0)::bigint
		FROM ledger_entries WHERE order_id = ANY($1) GROUP BY order_id`, ids, ledger.CommissionReserved, ledger.AccruePending, ledger.SwayFeePending, ledger.ReturnToMerchant)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var id string
		var c, pool, fee, ret int64
		if err := srows.Scan(&id, &c, &pool, &fee, &ret); err != nil {
			return nil, err
		}
		r := &out[idx[id]]
		r.CommissionCents, r.CreatorPoolCents, r.FeeCents, r.ReturnedCents = c, pool, fee, ret
		r.Balanced = c == pool+fee+ret
	}
	for i := range out {
		if out[i].CommissionCents == 0 {
			out[i].Balanced = true // nothing reserved yet: trivially balanced
		}
	}
	return out, srows.Err()
}

var verifyCache struct {
	sync.Mutex
	full time.Time // last full walk
	seq  int64     // last verified seq
	v    ledger.Verification
}

// cachedVerify keeps dashboards cheap at any ledger size: new entries are verified incrementally against the
// last verified hash, and the whole chain is re-walked every 60s to catch edits to old rows.
// /api/ledger/verify and the MCP tool always do a fresh full walk.
func cachedVerify(ctx context.Context, q db.Q) (ledger.Verification, error) {
	verifyCache.Lock()
	defer verifyCache.Unlock()
	c := &verifyCache
	if c.seq == 0 || !c.v.OK || time.Since(c.full) > time.Minute {
		v, last, err := ledger.VerifyAfter(ctx, q, 0, ledger.GenesisHash)
		if err != nil {
			return v, err
		}
		c.v, c.seq, c.full = v, last, time.Now()
		return v, nil
	}
	tail, last, err := ledger.VerifyAfter(ctx, q, c.seq, c.v.Head)
	if err != nil {
		return tail, err
	}
	tail.Entries += c.v.Entries
	c.v, c.seq = tail, last
	return tail, nil
}

func round2(x float64) float64 { return float64(int64(x*100+sign(x)*0.5)) / 100 }

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

func usd(c int64) string { return "$" + payments.FormatCents(c) }

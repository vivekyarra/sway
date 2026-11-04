package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/jobs"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/money"
	"github.com/vivekyarra/sway/engine/internal/paypal"
)

// PayoutPlan is what RunPayouts created.
type PayoutPlan struct {
	BatchID    string `json:"batch_id"`
	Items      int    `json:"items"`
	TotalCents int64  `json:"total_cents"`
	Offsets    int64  `json:"offset_cents"`
}

type chunk struct {
	orderID string
	amount  int64
}

// RunPayouts batches every creator whose vested balance, net of any owed clawbacks, reaches the minimum payout
// (spec 5.3 step 8). The ledger entries and batch rows commit first; submitting to PayPal is a queued job
// (transactional outbox), so a crash can never send money that the books don't show.
// mode "seeded" books a batch without calling PayPal (seeded history only).
func (s *Service) RunPayouts(ctx context.Context, mode string) (PayoutPlan, error) {
	if mode == "" {
		mode = s.PP.Mode()
	}
	var plan PayoutPlan
	err := s.DB.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(515151)`); err != nil {
			return err
		}
		// Lock every order that may hold vested money (sorted, before any ledger write), then read balances
		// only for those orders: a concurrent dispute or refund cannot move money we are about to pay out.
		var candidates []string
		crow, err := q.Query(ctx, `SELECT DISTINCT order_id FROM ledger_entries WHERE to_account=$1 AND order_id IS NOT NULL`, ledger.CreatorVested)
		if err != nil {
			return err
		}
		for crow.Next() {
			var id string
			if err := crow.Scan(&id); err != nil {
				crow.Close()
				return err
			}
			candidates = append(candidates, id)
		}
		crow.Close()
		if err := ledger.LockOrders(ctx, q, candidates); err != nil {
			return err
		}
		// Per (creator, order) vested balances.
		rows, err := q.Query(ctx, `SELECT creator_id, order_id, SUM(delta)::bigint FROM (
			SELECT creator_id, order_id, amount_cents AS delta FROM ledger_entries WHERE to_account=$1 AND creator_id IS NOT NULL AND order_id = ANY($2)
			UNION ALL SELECT creator_id, order_id, -amount_cents FROM ledger_entries WHERE from_account=$1 AND creator_id IS NOT NULL AND order_id = ANY($2)) x
			GROUP BY creator_id, order_id HAVING SUM(delta) > 0 ORDER BY creator_id, order_id`, ledger.CreatorVested, candidates)
		if err != nil {
			return err
		}
		vested := map[string][]chunk{}
		var creators []string
		for rows.Next() {
			var c, o string
			var amt int64
			if err := rows.Scan(&c, &o, &amt); err != nil {
				rows.Close()
				return err
			}
			if len(vested[c]) == 0 {
				creators = append(creators, c)
			}
			vested[c] = append(vested[c], chunk{o, amt})
		}
		rows.Close()
		if len(creators) == 0 {
			return nil
		}
		owed := map[string]int64{}
		orows, err := q.Query(ctx, `SELECT creator_id, SUM(delta)::bigint FROM (
			SELECT creator_id, amount_cents AS delta FROM ledger_entries WHERE to_account=$1 UNION ALL
			SELECT creator_id, -amount_cents FROM ledger_entries WHERE from_account=$1) x GROUP BY creator_id`, ledger.CreatorOwed)
		if err != nil {
			return err
		}
		for orows.Next() {
			var c string
			var b int64
			if err := orows.Scan(&c, &b); err != nil {
				orows.Close()
				return err
			}
			if b < 0 {
				owed[c] = -b
			}
		}
		orows.Close()

		batchID := newID("pob_")
		plan.BatchID = batchID
		if _, err := q.Exec(ctx, `INSERT INTO payout_batches (id, sender_batch_id, status, mode) VALUES ($1,$2,'CREATED',$3)`, batchID, "sway_"+batchID, mode); err != nil {
			return err
		}
		simulated := mode != "sandbox"
		var entries []ledger.Entry
		sort.Strings(creators)
		for _, c := range creators {
			var vestedTotal int64
			for _, ch := range vested[c] {
				vestedTotal += ch.amount
			}
			offset := min(owed[c], vestedTotal)
			pay := vestedTotal - offset
			sendable := pay >= s.Cfg.MinPayoutCents
			if !sendable && offset == 0 {
				continue
			}
			var receiver string
			if err := q.QueryRow(ctx, `SELECT paypal_email FROM creators WHERE id=$1`, c).Scan(&receiver); err != nil {
				return err
			}
			itemID := newID("poi_")
			remainingOffset := offset
			var orders int
			for _, ch := range vested[c] {
				amt := ch.amount
				if remainingOffset > 0 {
					o := min(remainingOffset, amt)
					entries = append(entries, ledger.Entry{Type: ledger.ClawbackOffset, OrderID: ch.orderID, CreatorID: c, From: ledger.CreatorVested, To: ledger.CreatorOwed,
						Amount: o, IdemKey: ledger.Key("offset", itemID, ch.orderID), Ref: itemID, Memo: "owed clawback netted against this payout", Simulated: simulated})
					remainingOffset -= o
					amt -= o
				}
				if amt > 0 && sendable {
					entries = append(entries, ledger.Entry{Type: ledger.PayoutSent, OrderID: ch.orderID, CreatorID: c, From: ledger.CreatorVested, To: ledger.CreatorPaid,
						Amount: amt, IdemKey: ledger.Key("payout", itemID, ch.orderID), Ref: itemID, Memo: "paid via PayPal Payouts", Simulated: simulated})
					orders++
				}
			}
			plan.Offsets += offset
			if sendable {
				status := "CREATED"
				if mode == "seeded" {
					status = "SUCCESS"
				}
				if _, err := q.Exec(ctx, `INSERT INTO payout_items (id, batch_id, creator_id, receiver, amount_cents, offset_cents, status) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
					itemID, batchID, c, receiver, pay, offset, status); err != nil {
					return err
				}
				plan.Items++
				plan.TotalCents += pay
			}
		}
		if _, err := ledger.Post(ctx, q, entries...); err != nil {
			return err
		}
		if plan.Items == 0 {
			if _, err := q.Exec(ctx, `UPDATE payout_batches SET status='EMPTY', total_cents=0 WHERE id=$1`, batchID); err != nil {
				return err
			}
			return nil
		}
		status := "QUEUED"
		if mode == "seeded" {
			status = "SUCCESS"
		}
		if _, err := q.Exec(ctx, `UPDATE payout_batches SET status=$2, total_cents=$3 WHERE id=$1`, batchID, status, plan.TotalCents); err != nil {
			return err
		}
		if mode == "seeded" {
			return nil
		}
		return jobs.Enqueue(ctx, q, jobs.TypePayoutSubmit, map[string]string{"batch_id": batchID}, "submit:"+batchID, 0)
	})
	if err == nil && plan.Items > 0 {
		s.wake()
	}
	return plan, err
}

// SubmitPayoutBatch sends a queued batch to PayPal Payouts (spec 8.4).
func (s *Service) SubmitPayoutBatch(ctx context.Context, batchID string) error {
	var senderBatchID, status string
	if err := s.DB.Pool.QueryRow(ctx, `SELECT sender_batch_id, status FROM payout_batches WHERE id=$1`, batchID).Scan(&senderBatchID, &status); err != nil {
		return err
	}
	if status != "QUEUED" {
		return nil
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT pi.id, pi.receiver, pi.amount_cents,
		(SELECT count(DISTINCT order_id) FROM ledger_entries WHERE ref=pi.id AND type=$2) FROM payout_items pi WHERE pi.batch_id=$1 ORDER BY pi.id`, batchID, ledger.PayoutSent)
	if err != nil {
		return err
	}
	var items []paypal.PayoutItemRequest
	for rows.Next() {
		var it paypal.PayoutItemRequest
		var n int
		if err := rows.Scan(&it.SenderItemID, &it.Receiver, &it.AmountCents, &n); err != nil {
			rows.Close()
			return err
		}
		plural := "s"
		if n == 1 {
			plural = ""
		}
		it.Note = fmt.Sprintf("Sway: %d purchase%s influenced and kept", n, plural)
		items = append(items, it)
	}
	rows.Close()
	b, err := s.PP.CreatePayoutBatch(ctx, paypal.PayoutBatchRequest{SenderBatchID: senderBatchID, Subject: "You swayed a sale",
		Message: "Your content influenced purchases that buyers kept.", Items: items})
	if err != nil {
		var pe *paypal.Error
		if errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 && pe.Status != 429 && !paypal.IsIssue(err, "DUPLICATE_REQUEST_ID") {
			// Definitive rejection: money never left. Return it to the creators' vested balances.
			s.log().Error("payout batch rejected", "batch", batchID, "err", err)
			return s.failBatch(ctx, batchID, err.Error())
		}
		return err // transient: retried with the same sender_batch_id, which PayPal de-duplicates
	}
	err = s.DB.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `UPDATE payout_batches SET paypal_payout_batch_id=$2, status=$3, updated_at=now() WHERE id=$1`, batchID, b.BatchID, orDefault(b.Status, "PENDING")); err != nil {
			return err
		}
		for _, it := range b.Items {
			if _, err := q.Exec(ctx, `UPDATE payout_items SET paypal_payout_item_id=$2, status=$3, updated_at=now() WHERE id=$1`, it.SenderItemID, it.ItemID, orDefault(it.Status, "PENDING")); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE payout_items SET status='PENDING' WHERE batch_id=$1 AND status='CREATED'`, batchID); err != nil {
			return err
		}
		return jobs.Enqueue(ctx, q, jobs.TypePayoutStatus, map[string]string{"batch_id": batchID}, "poll:"+batchID, 2*time.Second)
	})
	s.wake()
	return err
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Service) failBatch(ctx context.Context, batchID, reason string) error {
	return s.DB.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `UPDATE payout_batches SET status='DENIED', updated_at=now() WHERE id=$1`, batchID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE payout_items SET status='FAILED', error=$2, updated_at=now() WHERE batch_id=$1`, batchID, reason); err != nil {
			return err
		}
		ids, err := itemIDs(ctx, q, batchID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.reverseItem(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func itemIDs(ctx context.Context, q db.Q, batchID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id FROM payout_items WHERE batch_id=$1`, batchID)
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

// reverseItem returns a failed/returned item's money from paid back to vested, order by order, then reconciles
// those orders (a refund that arrived while the money was out can now be forfeited instead of clawed back).
func (s *Service) reverseItem(ctx context.Context, q db.Q, itemID string) error {
	rows, err := q.Query(ctx, `SELECT order_id, creator_id, amount_cents, simulated FROM ledger_entries WHERE ref=$1 AND type=$2`, itemID, ledger.PayoutSent)
	if err != nil {
		return err
	}
	type sent struct {
		order, creator string
		amt            int64
		sim            bool
	}
	var ss []sent
	var orders []string
	for rows.Next() {
		var x sent
		if err := rows.Scan(&x.order, &x.creator, &x.amt, &x.sim); err != nil {
			rows.Close()
			return err
		}
		ss = append(ss, x)
		orders = append(orders, x.order)
	}
	rows.Close()
	if err := ledger.LockOrders(ctx, q, orders); err != nil {
		return err
	}
	var entries []ledger.Entry
	for _, x := range ss {
		entries = append(entries, ledger.Entry{Type: ledger.PayoutFailedReversal, OrderID: x.order, CreatorID: x.creator, From: ledger.CreatorPaid, To: ledger.CreatorVested,
			Amount: x.amt, IdemKey: ledger.Key("reversal", itemID, x.order), Ref: itemID, Memo: "payout failed or returned: back to vested", Simulated: x.sim})
	}
	if _, err := ledger.Post(ctx, q, entries...); err != nil {
		return err
	}
	for _, x := range ss {
		if err := s.reconcile(ctx, q, x.order); err != nil {
			return err
		}
	}
	return nil
}

// PollPayoutBatch refreshes a batch from PayPal (spec 8.4). It asks to be retried while items are in flight.
func (s *Service) PollPayoutBatch(ctx context.Context, batchID string) error {
	var ppBatch *string
	if err := s.DB.Pool.QueryRow(ctx, `SELECT paypal_payout_batch_id FROM payout_batches WHERE id=$1`, batchID).Scan(&ppBatch); err != nil {
		return err
	}
	if ppBatch == nil {
		return nil
	}
	b, err := s.PP.GetPayoutBatch(ctx, *ppBatch)
	if err != nil {
		return err
	}
	inFlight, unclaimed := false, false
	err = s.DB.Tx(ctx, func(q db.Q) error {
		for _, it := range b.Items {
			var ourID, prev string
			if err := q.QueryRow(ctx, `SELECT id, status FROM payout_items WHERE paypal_payout_item_id=$1 OR id=$2 LIMIT 1 FOR UPDATE`, it.ItemID, it.SenderItemID).Scan(&ourID, &prev); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `UPDATE payout_items SET status=$2, error=$3, paypal_payout_item_id=COALESCE(paypal_payout_item_id,$4), updated_at=now() WHERE id=$1`,
				ourID, it.Status, it.Error, it.ItemID); err != nil {
				return err
			}
			if paypal.FailedItemStatus(it.Status) && !paypal.FailedItemStatus(prev) {
				if err := s.reverseItem(ctx, q, ourID); err != nil {
					return err
				}
			}
			switch {
			case it.Status == "UNCLAIMED":
				unclaimed = true
			case !paypal.FinalItemStatus(it.Status):
				inFlight = true
			}
		}
		_, err := q.Exec(ctx, `UPDATE payout_batches SET status=$2, updated_at=now() WHERE id=$1`, batchID, orDefault(b.Status, "PENDING"))
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case inFlight:
		return jobs.Retry(3 * time.Second)
	case unclaimed:
		return jobs.Retry(2 * time.Minute) // unclaimed items settle when the receiver signs up (or expire after 30 days)
	}
	return nil
}

// CancelPayoutItem cancels an UNCLAIMED item so a refunded order's money comes back (spec 7.4).
func (s *Service) CancelPayoutItem(ctx context.Context, itemID string) error {
	var ppItem *string
	var status string
	if err := s.DB.Pool.QueryRow(ctx, `SELECT paypal_payout_item_id, status FROM payout_items WHERE id=$1`, itemID).Scan(&ppItem, &status); err != nil {
		return err
	}
	if status != "UNCLAIMED" || ppItem == nil {
		return nil
	}
	it, err := s.PP.CancelPayoutItem(ctx, *ppItem)
	if err != nil {
		var pe *paypal.Error
		if errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 {
			// Claimed in the meantime: refresh, and the next reconcile turns the remainder into a clawback.
			s.log().Info("payout item could not be cancelled", "item", itemID, "err", err)
			return s.afterCancelFailed(ctx, itemID)
		}
		return err
	}
	return s.DB.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `UPDATE payout_items SET status=$2, updated_at=now() WHERE id=$1`, itemID, orDefault(it.Status, "RETURNED")); err != nil {
			return err
		}
		return s.reverseItem(ctx, q, itemID)
	})
}

func (s *Service) afterCancelFailed(ctx context.Context, itemID string) error {
	var batchID string
	if err := s.DB.Pool.QueryRow(ctx, `SELECT batch_id FROM payout_items WHERE id=$1`, itemID).Scan(&batchID); err != nil {
		return err
	}
	if err := s.PollPayoutBatch(ctx, batchID); err != nil {
		var r jobs.RetryError
		if !errors.As(err, &r) {
			return err
		}
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT DISTINCT order_id FROM ledger_entries WHERE ref=$1`, itemID)
	if err != nil {
		return err
	}
	var orders []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return err
		}
		orders = append(orders, o)
	}
	rows.Close()
	for _, o := range orders {
		if err := s.Reconcile(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// JobHandlers wires the payment jobs into the worker.
func (s *Service) JobHandlers() map[string]jobs.Handler {
	payload := func(j jobs.Job, key string) string {
		var m map[string]string
		_ = json.Unmarshal(j.Payload, &m)
		return m[key]
	}
	return map[string]jobs.Handler{
		jobs.TypePayoutSubmit: func(ctx context.Context, j jobs.Job) error { return s.SubmitPayoutBatch(ctx, payload(j, "batch_id")) },
		jobs.TypePayoutStatus: func(ctx context.Context, j jobs.Job) error { return s.PollPayoutBatch(ctx, payload(j, "batch_id")) },
		jobs.TypePayoutCancel: func(ctx context.Context, j jobs.Job) error { return s.CancelPayoutItem(ctx, payload(j, "item_id")) },
		jobs.TypeReconcile:    func(ctx context.Context, j jobs.Job) error { return s.Reconcile(ctx, payload(j, "order_id")) },
		jobs.TypeWebhook:      func(ctx context.Context, j jobs.Job) error { return s.ProcessWebhook(ctx, payload(j, "event_id")) },
	}
}

// FormatCents is re-exported for notes.
func FormatCents(c int64) string { return money.Format(c) }

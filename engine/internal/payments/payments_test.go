package payments_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/testkit"
)

const headphones = "I need noise-cancelling headphones for long-haul flights, under $250."

func bal(t *testing.T, m *testkit.Money, creator string) payments.CreatorBalances {
	t.Helper()
	b, err := payments.Balances(context.Background(), m.DB.Pool, creator)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func accrued(t *testing.T, m *testkit.Money, orderID, creator string) int64 {
	t.Helper()
	var x int64
	if err := m.DB.Pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(amount_cents),0)::bigint FROM ledger_entries WHERE order_id=$1 AND creator_id=$2 AND type=$3`,
		orderID, creator, ledger.AccruePending).Scan(&x); err != nil {
		t.Fatal(err)
	}
	return x
}

// checkBooks asserts E11 for every order plus chain integrity and non-negative buckets.
func checkBooks(t *testing.T, m *testkit.Money) {
	t.Helper()
	ctx := context.Background()
	rows, err := m.DB.Pool.Query(ctx, `SELECT DISTINCT order_id FROM ledger_entries WHERE order_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s, err := ledger.SummarizeOrder(ctx, m.DB.Pool, id)
		if err != nil || !s.Balanced {
			var jobErrs []string
			jr, _ := m.DB.Pool.Query(ctx, `SELECT type, status, attempts, last_error FROM jobs WHERE status <> 'DONE'`)
			for jr.Next() {
				var typ, st, le string
				var at int
				_ = jr.Scan(&typ, &st, &at, &le)
				jobErrs = append(jobErrs, fmt.Sprintf("%s/%s/%d: %s", typ, st, at, le))
			}
			jr.Close()
			var attErr string
			_ = m.DB.Pool.QueryRow(ctx, `SELECT status || ': ' || error FROM attribution_jobs WHERE order_id=$1`, id).Scan(&attErr)
			t.Fatalf("order %s unbalanced: %+v %v; attribution %q; open jobs %v", id, s, err, attErr, jobErrs)
		}
		pc, _ := ledger.OrderCreatorBalances(ctx, m.DB.Pool, id)
		for c, b := range pc {
			for _, acct := range []string{ledger.CreatorPending, ledger.CreatorFrozen, ledger.CreatorVested, ledger.CreatorPaid} {
				if b[acct] < 0 {
					t.Fatalf("order %s creator %s: %s negative (%d)", id, c, acct, b[acct])
				}
			}
		}
		ob, _ := ledger.BalancesFor(ctx, m.DB.Pool, ledger.Scope{OrderID: id})
		if ob[ledger.OrderCommission] != 0 {
			t.Fatalf("order %s has undistributed commission %d", id, ob[ledger.OrderCommission])
		}
	}
	v, err := ledger.Verify(ctx, m.DB.Pool)
	if err != nil || !v.OK || !v.TrialZero {
		t.Fatalf("ledger verify %+v %v", v, err)
	}
}

// E1: request -> Halvo Q7 -> buy -> attribution -> vest -> payout. Priya and Maya paid, Dev and Tomás $0.
func TestE1HappyPath(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	orderID, product := m.Buy(t, headphones)
	if product != "p_halvo_q7" {
		t.Fatalf("bought %s", product)
	}
	if b := bal(t, m, "c_priya"); b.Pending <= 0 || b.Vested != 0 {
		t.Fatalf("priya before vesting %+v", b)
	}
	// Not vested before the window passes.
	if n, _ := m.Pay.VestDue(ctx); n != 0 {
		t.Fatal("vested before the return window")
	}
	plan := m.VestAndPay(t)
	if plan.Items != 2 {
		t.Fatalf("expected Priya and Maya paid, plan %+v", plan)
	}
	priya, maya, dev, tomas := bal(t, m, "c_priya"), bal(t, m, "c_maya"), bal(t, m, "c_dev"), bal(t, m, "c_tomas")
	if priya.Paid != accrued(t, m, orderID, "c_priya") || priya.Pending != 0 || priya.Vested != 0 {
		t.Fatalf("priya %+v", priya)
	}
	if maya.Paid <= 0 || dev.Earned != 0 || tomas.Earned != 0 {
		t.Fatalf("maya %+v dev %+v tomas %+v", maya, dev, tomas)
	}
	var status string
	if err := m.DB.Pool.QueryRow(ctx, `SELECT status FROM payout_batches WHERE id=$1`, plan.BatchID).Scan(&status); err != nil || status != "SUCCESS" {
		t.Fatalf("batch status %q %v", status, err)
	}
	// A second payout run finds nothing to pay.
	if p, _ := m.Pay.RunPayouts(ctx, ""); p.Items != 0 {
		t.Fatalf("double payout %+v", p)
	}
	checkBooks(t, m)
}

// E3: refund before vesting forfeits every pending amount for the order.
func TestE3RefundBeforeVesting(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	orderID, _ := m.Buy(t, headphones)
	if _, err := m.Pay.Refund(ctx, orderID, 0, "changed mind", ""); err != nil {
		t.Fatal(err)
	}
	priya := bal(t, m, "c_priya")
	if priya.Pending != 0 || priya.Forfeited != accrued(t, m, orderID, "c_priya") {
		t.Fatalf("priya %+v", priya)
	}
	if plan := m.VestAndPay(t); plan.Items != 0 {
		t.Fatalf("refunded order paid out: %+v", plan)
	}
	// Refunding again is rejected; the books do not move.
	if _, err := m.Pay.Refund(ctx, orderID, 0, "again", ""); err == nil {
		t.Fatal("second full refund should fail")
	}
	checkBooks(t, m)
}

// E4: a 50% refund forfeits exactly half of each creator's pending amount (cents exact).
func TestE4PartialRefund(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	orderID, _ := m.Buy(t, headphones)
	var total int64
	_ = m.DB.Pool.QueryRow(ctx, `SELECT amount_cents FROM orders WHERE id=$1`, orderID).Scan(&total)
	if _, err := m.Pay.Refund(ctx, orderID, total/2, "half", ""); err != nil {
		t.Fatal(err)
	}
	var sumForfeit, sumAccrued int64
	for _, c := range []string{"c_priya", "c_maya"} {
		a, b := accrued(t, m, orderID, c), bal(t, m, c)
		if d := b.Forfeited*2 - a; d < -1 || d > 1 {
			t.Fatalf("%s forfeited %d of %d", c, b.Forfeited, a)
		}
		sumForfeit += b.Forfeited
		sumAccrued += a
	}
	if want := (sumAccrued + 1) / 2; sumForfeit != want && sumForfeit != sumAccrued/2 {
		t.Fatalf("total forfeit %d of %d", sumForfeit, sumAccrued)
	}
	// The remaining half vests and pays.
	plan := m.VestAndPay(t)
	if plan.TotalCents != sumAccrued-sumForfeit {
		t.Fatalf("paid %d want %d", plan.TotalCents, sumAccrued-sumForfeit)
	}
	checkBooks(t, m)
}

// E5: refund after the payout was claimed -> clawback; the next payout is reduced by the owed amount.
func TestE5ClawbackNettedAgainstNextPayout(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	first, _ := m.Buy(t, headphones)
	m.VestAndPay(t)
	paidFirst := accrued(t, m, first, "c_priya")
	if _, err := m.Pay.Refund(ctx, first, 0, "returned", ""); err != nil {
		t.Fatal(err)
	}
	priya := bal(t, m, "c_priya")
	if priya.Owed != paidFirst || priya.ClawedBack != paidFirst || priya.Paid != paidFirst {
		t.Fatalf("after clawback %+v (paid first %d)", priya, paidFirst)
	}
	second, _ := m.Buy(t, headphones+" ")
	earnedSecond := accrued(t, m, second, "c_priya")
	plan := m.VestAndPay(t)
	priya = bal(t, m, "c_priya")
	if priya.Owed != 0 {
		t.Fatalf("debt should be settled: %+v", priya)
	}
	if priya.Paid != paidFirst+earnedSecond-paidFirst {
		t.Fatalf("second payout should be reduced by the owed amount: %+v plan %+v", priya, plan)
	}
	var offset int64
	if err := m.DB.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents),0)::bigint FROM ledger_entries WHERE type=$1 AND creator_id='c_priya'`, ledger.ClawbackOffset).Scan(&offset); err != nil || offset != paidFirst {
		t.Fatalf("priya offset %d want %d (%v)", offset, paidFirst, err)
	}
	checkBooks(t, m)
}

// E6: dispute opened -> frozen (no vesting), resolved for the seller -> vests normally.
func TestE6DisputeResolvedForSeller(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	orderID, _ := m.Buy(t, headphones)
	if err := m.Pay.TestDispute(ctx, orderID, payments.DisputeOpen); err != nil {
		t.Fatal(err)
	}
	if b := bal(t, m, "c_priya"); b.Frozen == 0 || b.Pending != 0 {
		t.Fatalf("not frozen %+v", b)
	}
	if plan := m.VestAndPay(t); plan.Items != 0 {
		t.Fatal("frozen money must not vest or pay")
	}
	if err := m.Pay.TestDispute(ctx, orderID, payments.DisputeResolveSeller); err != nil {
		t.Fatal(err)
	}
	if b := bal(t, m, "c_priya"); b.Frozen != 0 || b.Pending == 0 {
		t.Fatalf("not released %+v", b)
	}
	if plan := m.VestAndPay(t); plan.Items != 2 {
		t.Fatalf("expected payouts after release, got %+v", plan)
	}
	var testMode bool
	_ = m.DB.Pool.QueryRow(ctx, `SELECT bool_and(test_mode) FROM webhook_events WHERE event_type LIKE 'CUSTOMER.DISPUTE%'`).Scan(&testMode)
	if !testMode {
		t.Fatal("testMode disputes must be labelled")
	}
	checkBooks(t, m)
}

// E7: a dispute resolved for the buyer is treated as a full refund.
func TestE7DisputeResolvedForBuyer(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	orderID, _ := m.Buy(t, headphones)
	_ = m.Pay.TestDispute(ctx, orderID, payments.DisputeOpen)
	if err := m.Pay.TestDispute(ctx, orderID, payments.DisputeResolveBuyer); err != nil {
		t.Fatal(err)
	}
	b := bal(t, m, "c_priya")
	if b.Pending+b.Frozen+b.Vested != 0 || b.Forfeited != accrued(t, m, orderID, "c_priya") {
		t.Fatalf("priya %+v", b)
	}
	checkBooks(t, m)
}

// E10: the same webhook twice and capture called twice create no double entries.
func TestE10Idempotency(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	dv, err := m.Shop.Decide(ctx, headphones)
	if err != nil {
		t.Fatal(err)
	}
	co, _ := m.Pay.CreateOrder(ctx, dv.ID)
	co2, _ := m.Pay.CreateOrder(ctx, dv.ID)
	if co.OrderID != co2.OrderID || co.PayPalOrderID != co2.PayPalOrderID {
		t.Fatal("creating the order twice must return the same order")
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Pay.Capture(ctx, co.PayPalOrderID); err != nil {
			t.Fatal(err)
		}
	}
	m.Drain(t)
	var reserves int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE type=$1`, ledger.CommissionReserved).Scan(&reserves)
	if reserves != 1 {
		t.Fatalf("reserved %d times", reserves)
	}
	// Replay the stored capture webhook and a refund webhook twice.
	var evID string
	_ = m.DB.Pool.QueryRow(ctx, `SELECT paypal_event_id FROM webhook_events WHERE event_type='PAYMENT.CAPTURE.COMPLETED'`).Scan(&evID)
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE webhook_events SET processed_at=NULL`)
	if err := m.Pay.ProcessWebhook(ctx, evID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pay.Refund(ctx, co.OrderID, 5000, "partial", "idem-1"); err != nil {
		t.Fatal(err)
	}
	before := countEntries(t, m)
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE webhook_events SET processed_at=NULL`)
	rows, _ := m.DB.Pool.Query(ctx, `SELECT paypal_event_id FROM webhook_events`)
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		for i := 0; i < 2; i++ {
			if err := m.Pay.ProcessWebhook(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if after := countEntries(t, m); after != before {
		t.Fatalf("replayed webhooks wrote %d entries", after-before)
	}
	var refunded int64
	_ = m.DB.Pool.QueryRow(ctx, `SELECT refunded_cents FROM orders WHERE id=$1`, co.OrderID).Scan(&refunded)
	if refunded != 5000 {
		t.Fatalf("refunded %d", refunded)
	}
	checkBooks(t, m)
}

func countEntries(t *testing.T, m *testkit.Money) int {
	var n int
	_ = m.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM ledger_entries`).Scan(&n)
	return n
}

// refund after payout while the payout item is still UNCLAIMED -> cancel item, reverse, forfeit.
func TestRefundAfterUnclaimedPayoutCancels(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE creators SET paypal_email='priya.unclaimed@personal.example.com' WHERE id='c_priya'`)
	orderID, _ := m.Buy(t, headphones)
	m.VestAndPay(t)
	var st string
	_ = m.DB.Pool.QueryRow(ctx, `SELECT status FROM payout_items WHERE creator_id='c_priya'`).Scan(&st)
	if st != "UNCLAIMED" {
		t.Fatalf("item status %s", st)
	}
	if _, err := m.Pay.Refund(ctx, orderID, 0, "returned", ""); err != nil {
		t.Fatal(err)
	}
	m.Drain(t)
	_ = m.DB.Pool.QueryRow(ctx, `SELECT status FROM payout_items WHERE creator_id='c_priya'`).Scan(&st)
	b := bal(t, m, "c_priya")
	if st != "RETURNED" || b.Owed != 0 || b.Paid != 0 || b.Forfeited != accrued(t, m, orderID, "c_priya") {
		t.Fatalf("status %s balances %+v", st, b)
	}
	checkBooks(t, m)
}

// A failed payout item returns the money to the creator's vested balance.
func TestFailedPayoutReverses(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE creators SET paypal_email='maya.fail@personal.example.com' WHERE id='c_maya'`)
	orderID, _ := m.Buy(t, headphones)
	m.VestAndPay(t)
	b := bal(t, m, "c_maya")
	if b.Paid != 0 || b.Vested != accrued(t, m, orderID, "c_maya") {
		t.Fatalf("maya %+v", b)
	}
	checkBooks(t, m)
}

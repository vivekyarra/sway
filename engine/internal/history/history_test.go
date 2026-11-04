package history_test

import (
	"context"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/history"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/testkit"
	"github.com/vivekyarra/sway/engine/internal/views"
)

// E11: the order history covers vested, paid, refunded, clawback and frozen states; books balance; reset rebuilds.
func TestHistoryAndReset(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	b := &history.Builder{DB: m.DB, Cfg: m.Cfg, World: m.World, Pay: m.Pay}
	if err := b.History(ctx); err != nil {
		t.Fatal(err)
	}
	var historical, refunded, disputed, paidItems int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE refunded_cents>0), count(*) FILTER (WHERE dispute_status='OPEN') FROM orders WHERE historical`).Scan(&historical, &refunded, &disputed)
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM payout_items WHERE status='SUCCESS'`).Scan(&paidItems)
	if historical != 12 || refunded != 2 || disputed != 1 || paidItems == 0 {
		t.Fatalf("historical %d refunded %d disputed %d paid items %d", historical, refunded, disputed, paidItems)
	}
	var clawbacks, ppIDs int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE type=$1`, ledger.Clawback).Scan(&clawbacks)
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE historical AND (paypal_order_id IS NOT NULL OR paypal_capture_id IS NOT NULL)`).Scan(&ppIDs)
	if clawbacks == 0 || ppIDs != 0 {
		t.Fatalf("clawbacks %d, PayPal ids on historical orders %d", clawbacks, ppIDs)
	}
	priya, err := views.GetCreator(ctx, m.DB.Pool, "c_priya")
	if err != nil {
		t.Fatal(err)
	}
	if priya.Balances.Paid == 0 || priya.Swayed == 0 || len(priya.Events) == 0 {
		t.Fatalf("priya dashboard %+v", priya.Balances)
	}
	t.Log(priya.Headline)
	merch, err := views.GetMerchant(ctx, m.DB.Pool, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if !merch.Verification.OK || len(merch.Orders) != 12 || len(merch.Content) == 0 {
		t.Fatalf("merchant view: verify %+v orders %d", merch.Verification, len(merch.Orders))
	}
	for _, o := range merch.Orders {
		if !o.Balanced {
			t.Fatalf("order %s unbalanced", o.ID)
		}
	}
	ov, err := views.GetOrder(ctx, m.DB.Pool, "ord_seed_01")
	if err != nil || ov.Job == nil || len(ov.Attributions) == 0 {
		t.Fatalf("order view %+v %v", ov, err)
	}
	// Reset wipes and rebuilds; the ledger chain verifies afterwards.
	if err := b.Reset(ctx, nil); err != nil {
		t.Fatal(err)
	}
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&historical)
	if v, _ := ledger.Verify(ctx, m.DB.Pool); historical != 12 || !v.OK {
		t.Fatalf("after reset: %d orders, verify %+v", historical, v)
	}
	if _, err := payments.Balances(ctx, m.DB.Pool, "c_priya"); err != nil {
		t.Fatal(err)
	}
	// Outside a reset, TRUNCATE of the ledger is refused.
	if _, err := m.DB.Pool.Exec(ctx, `TRUNCATE ledger_entries CASCADE`); err == nil {
		t.Fatal("ledger truncate must be refused")
	}
}

// The judge tour's live headphone purchase must pay Priya on top of the order history (no owed balance absorbs it).
func TestLivePurchaseAfterHistoryPaysPriya(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	b := &history.Builder{DB: m.DB, Cfg: m.Cfg, World: m.World, Pay: m.Pay}
	if err := b.History(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := payments.Balances(ctx, m.DB.Pool, "c_priya")
	if before.Owed != 0 {
		t.Fatalf("Priya must not start owing money: %+v", before)
	}
	m.Buy(t, "I need noise-cancelling headphones for long-haul flights, under $250.")
	m.VestAndPay(t)
	var n int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM payout_items pi JOIN payout_batches pb ON pb.id=pi.batch_id WHERE pi.creator_id='c_priya' AND pb.mode<>'historical' AND pi.status='SUCCESS'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected a live payout to Priya, got %d", n)
	}
}

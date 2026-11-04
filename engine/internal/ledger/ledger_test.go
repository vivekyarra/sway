package ledger_test

import (
	"context"
	"sync"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/testdb"
)

func seed(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO merchants (id,name,commission_rate_bps,vesting_window_seconds) VALUES ('m1','Larkline Audio',800,120)`,
		`INSERT INTO products (id,merchant_id,name,brand,category,price_cents) VALUES ('p1','m1','Halvo Q7','Halvo','headphones',22900)`,
		`INSERT INTO creators (id,display_name,paypal_email) VALUES ('c1','Priya','priya@example.com'),('c2','Maya','maya@example.com')`,
		`INSERT INTO orders (id,product_id,merchant_id,amount_cents,commission_rate_bps,sway_fee_bps,payment_mode,status) VALUES ('o1','p1','m1',22900,800,1500,'sim','CAPTURED')`,
	}
	for _, s := range stmts {
		if _, err := d.Pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func post(t *testing.T, d *db.DB, es ...ledger.Entry) []ledger.Entry {
	t.Helper()
	var out []ledger.Entry
	err := d.Tx(context.Background(), func(q db.Q) error {
		var err error
		out, err = ledger.Post(context.Background(), q, es...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPostIdempotentBalancedAndChained(t *testing.T) {
	d := testdb.New(t)
	seed(t, d)
	ctx := context.Background()
	entries := []ledger.Entry{
		{Type: ledger.CommissionReserved, OrderID: "o1", From: ledger.MerchantFunding, To: ledger.OrderCommission, Amount: 1832, IdemKey: "res|o1"},
		{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c1", From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 1200, IdemKey: "acc|o1|c1"},
		{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c2", From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 357, IdemKey: "acc|o1|c2"},
		{Type: ledger.SwayFeePending, OrderID: "o1", From: ledger.OrderCommission, To: ledger.SwayPending, Amount: 275, IdemKey: "fee|o1"},
	}
	if got := post(t, d, entries...); len(got) != 4 {
		t.Fatalf("wrote %d", len(got))
	}
	// Replaying the same entries (e.g. a duplicate webhook) writes nothing.
	if got := post(t, d, entries...); len(got) != 0 {
		t.Fatalf("replay wrote %d entries", len(got))
	}
	s, err := ledger.SummarizeOrder(ctx, d.Pool, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Balanced || s.Commission != 1832 || s.CreatorPoolNet != 1557 || s.SwayFee != 275 {
		t.Fatalf("summary %+v", s)
	}
	b, _ := ledger.BalancesFor(ctx, d.Pool, ledger.Scope{OrderID: "o1"})
	if b[ledger.OrderCommission] != 0 || b[ledger.CreatorPending] != 1557 {
		t.Fatalf("balances %+v", b)
	}
	v, err := ledger.Verify(ctx, d.Pool)
	if err != nil || !v.OK || v.Entries != 4 || !v.TrialZero {
		t.Fatalf("verify %+v %v", v, err)
	}
	pc, _ := ledger.OrderCreatorBalances(ctx, d.Pool, "o1")
	if pc["c1"][ledger.CreatorPending] != 1200 || pc["c2"][ledger.CreatorPending] != 357 {
		t.Fatalf("per creator %+v", pc)
	}
}

func TestAppendOnlyAndTamperDetection(t *testing.T) {
	d := testdb.New(t)
	seed(t, d)
	ctx := context.Background()
	post(t, d,
		ledger.Entry{Type: ledger.CommissionReserved, OrderID: "o1", From: ledger.MerchantFunding, To: ledger.OrderCommission, Amount: 1832, IdemKey: "a"},
		ledger.Entry{Type: ledger.ReturnToMerchant, OrderID: "o1", From: ledger.OrderCommission, To: ledger.MerchantReturned, Amount: 1832, IdemKey: "b"},
	)
	if _, err := d.Pool.Exec(ctx, `UPDATE ledger_entries SET amount_cents=1 WHERE idem_key='a'`); err == nil {
		t.Fatal("update must be rejected by the append-only trigger")
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("delete must be rejected")
	}
	// Simulate an attacker with superuser rights bypassing the trigger: Verify must notice.
	err := d.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `SET LOCAL sway.allow_reset = 'on'`); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `UPDATE ledger_entries SET amount_cents=1000, signed_cents=1000 WHERE idem_key='b'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := ledger.Verify(ctx, d.Pool)
	if v.OK || v.BrokenAtSeq == 0 {
		t.Fatalf("tampering not detected: %+v", v)
	}
}

func TestConcurrentPostsKeepChainLinear(t *testing.T) {
	d := testdb.New(t)
	seed(t, d)
	ctx := context.Background()
	post(t, d, ledger.Entry{Type: ledger.CommissionReserved, OrderID: "o1", From: ledger.MerchantFunding, To: ledger.OrderCommission, Amount: 100000, IdemKey: "fund"})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- d.Tx(ctx, func(q db.Q) error {
				// Half the goroutines race on the same key: exactly one may win.
				key := "race"
				if i%2 == 0 {
					key = "k" + string(rune('A'+i/2))
				}
				_, err := ledger.Post(ctx, q, ledger.Entry{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c1",
					From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 10, IdemKey: key})
				return err
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	v, _ := ledger.Verify(ctx, d.Pool)
	if !v.OK || v.Entries != 1+32+1 {
		t.Fatalf("verify %+v", v)
	}
}

func TestRejectsMalformed(t *testing.T) {
	d := testdb.New(t)
	seed(t, d)
	err := d.Tx(context.Background(), func(q db.Q) error {
		_, err := ledger.Post(context.Background(), q, ledger.Entry{Type: "X", From: "a", To: "a", Amount: 5, IdemKey: "x"})
		return err
	})
	if err == nil {
		t.Fatal("self-transfer must be rejected")
	}
	err = d.Tx(context.Background(), func(q db.Q) error {
		_, err := ledger.Post(context.Background(), q, ledger.Entry{Type: "X", From: "a", To: "b", Amount: -5, IdemKey: "y"})
		return err
	})
	if err == nil {
		t.Fatal("negative amount must be rejected")
	}
}

// Defense in depth: no entry may take a creator or commission account below zero, whatever the caller computed.
func TestOverdraftRefused(t *testing.T) {
	d := testdb.New(t)
	seed(t, d)
	post(t, d,
		ledger.Entry{Type: ledger.CommissionReserved, OrderID: "o1", From: ledger.MerchantFunding, To: ledger.OrderCommission, Amount: 100, IdemKey: "r"},
		ledger.Entry{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c1", From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 60, IdemKey: "a"},
	)
	try := func(e ledger.Entry) error {
		return d.Tx(context.Background(), func(q db.Q) error { _, err := ledger.Post(context.Background(), q, e); return err })
	}
	if err := try(ledger.Entry{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c2", From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 41, IdemKey: "b"}); err == nil {
		t.Fatal("over-distributing the commission must be refused")
	}
	if err := try(ledger.Entry{Type: ledger.Vest, OrderID: "o1", CreatorID: "c1", From: ledger.CreatorPending, To: ledger.CreatorVested, Amount: 61, IdemKey: "v"}); err == nil {
		t.Fatal("vesting more than pending must be refused")
	}
	if err := try(ledger.Entry{Type: ledger.AccruePending, OrderID: "o1", CreatorID: "c2", From: ledger.OrderCommission, To: ledger.CreatorPending, Amount: 40, IdemKey: "c"}); err != nil {
		t.Fatalf("exact distribution must pass: %v", err)
	}
	if v, _ := ledger.Verify(context.Background(), d.Pool); !v.OK || v.Entries != 3 {
		t.Fatalf("refused entries must leave no trace: %+v", v)
	}
}

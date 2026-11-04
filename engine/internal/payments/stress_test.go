package payments_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/testkit"
)

var requests = []string{
	"I need noise-cancelling headphones for long-haul flights, under $250.",
	"A carry-on suitcase for weekly business trips that survives rough handling, under $220.",
	"A standing desk that stays stable at full height for a tall person, under $450.",
	"Noise-cancelling headphones for my commute and long flights, budget $300",
}

// conservation: for every creator, lifetime earnings are fully accounted for:
// earned = pending + frozen + vested + paid - owed + forfeited + clawed_back, and no bucket is negative.
func conservation(t *testing.T, m *testkit.Money) {
	t.Helper()
	ctx := context.Background()
	rows, err := m.DB.Pool.Query(ctx, `SELECT id FROM creators`)
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
		b, err := payments.Balances(ctx, m.DB.Pool, id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending < 0 || b.Frozen < 0 || b.Vested < 0 || b.Paid < 0 || b.Owed < 0 || b.Forfeited < 0 || b.ClawedBack < 0 {
			t.Fatalf("%s negative bucket: %+v", id, b)
		}
		if got := b.Pending + b.Frozen + b.Vested + b.Paid - b.Owed + b.Forfeited + b.ClawedBack; got != b.Earned {
			t.Fatalf("%s conservation broken: accounted %d != earned %d (%+v)", id, got, b.Earned, b)
		}
	}
	checkBooks(t, m)
}

// TestChaos drives random lifecycles through the real services and checks every invariant after each round.
func TestChaos(t *testing.T) {
	rounds := 6
	if os.Getenv("SWAY_STRESS") != "" {
		rounds = 30
	}
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE creators SET paypal_email='maya.unclaimed@personal.example.com' WHERE id='c_maya'`)
	_, _ = m.DB.Pool.Exec(ctx, `UPDATE creators SET paypal_email='jonah.fail@personal.example.com' WHERE id='c_jonah'`)
	r := rand.New(rand.NewPCG(2026, 11))
	var orders []string
	for round := 0; round < rounds; round++ {
		// New purchases (each request is made unique so it is a fresh decision).
		for i := 0; i < 3; i++ {
			id, _ := m.Buy(t, fmt.Sprintf("%s #%d-%d", requests[r.IntN(len(requests))], round, i))
			orders = append(orders, id)
		}
		// Random events on random orders.
		for i := 0; i < 6; i++ {
			o := orders[r.IntN(len(orders))]
			switch r.IntN(8) {
			case 0:
				_, _ = m.Pay.Refund(ctx, o, 0, "full", "")
			case 1, 2:
				_, _ = m.Pay.Refund(ctx, o, int64(1+r.IntN(9000)), "partial", "")
			case 3:
				_ = m.Pay.TestDispute(ctx, o, payments.DisputeOpen)
			case 4:
				_ = m.Pay.TestDispute(ctx, o, payments.DisputeResolveSeller)
			case 5:
				_ = m.Pay.TestDispute(ctx, o, payments.DisputeResolveBuyer)
			case 6:
				_, _ = m.Pay.FastForward(ctx)
				_, _ = m.Pay.VestDue(ctx)
			case 7:
				if _, err := m.Pay.RunPayouts(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			m.Drain(t)
		}
		// Replay every webhook (duplicates must be no-ops).
		_, _ = m.DB.Pool.Exec(ctx, `UPDATE webhook_events SET processed_at=NULL WHERE random() < 0.5`)
		rows, _ := m.DB.Pool.Query(ctx, `SELECT paypal_event_id FROM webhook_events WHERE processed_at IS NULL`)
		var evs []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			evs = append(evs, id)
		}
		rows.Close()
		for _, e := range evs {
			if err := m.Pay.ProcessWebhook(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		m.Drain(t)
		conservation(t, m)
	}
	t.Logf("chaos: %d orders across %d rounds, invariants held", len(orders), rounds)
}

// TestConcurrentStorm hammers the same orders from many goroutines: captures, refunds, disputes and payouts race.
func TestConcurrentStorm(t *testing.T) {
	m := testkit.New(t).WithMoney(t)
	ctx := context.Background()
	var ppOrders, orders []string
	for i := 0; i < 6; i++ {
		dv, err := m.Shop.Decide(ctx, fmt.Sprintf("%s #storm-%d", requests[i%len(requests)], i))
		if err != nil {
			t.Fatal(err)
		}
		co, err := m.Pay.CreateOrder(ctx, dv.ID)
		if err != nil {
			t.Fatal(err)
		}
		ppOrders = append(ppOrders, co.PayPalOrderID)
		orders = append(orders, co.OrderID)
	}
	// 10 concurrent captures per order.
	var wg sync.WaitGroup
	for _, pp := range ppOrders {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.Pay.Capture(ctx, pp); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	m.Drain(t)
	// Concurrent refunds (some must be rejected as exceeding the remaining amount), disputes, vesting and payouts.
	_, _ = m.Pay.FastForward(ctx)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := orders[i%len(orders)]
			switch i % 5 {
			case 0, 1:
				_, _ = m.Pay.Refund(ctx, o, 3000, "storm", "")
			case 2:
				_ = m.Pay.TestDispute(ctx, o, payments.DisputeOpen)
			case 3:
				_, _ = m.Pay.VestDue(ctx)
			case 4:
				_, _ = m.Pay.RunPayouts(ctx, "")
			}
		}(i)
	}
	wg.Wait()
	m.Drain(t)
	var over int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE refunded_cents > amount_cents`).Scan(&over)
	if over != 0 {
		t.Fatal("an order was refunded more than its amount")
	}
	var reserves, captured int
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE type='COMMISSION_RESERVED'`).Scan(&reserves)
	_ = m.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE captured_at IS NOT NULL`).Scan(&captured)
	if reserves != captured || captured != len(orders) {
		t.Fatalf("reserves %d captured %d orders %d", reserves, captured, len(orders))
	}
	conservation(t, m)
}

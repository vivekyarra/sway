// Package history builds the pre-loaded order history and resets the marketplace.
//
// The order history runs through the real pipeline (agent decisions, exact Shapley, ledger, vesting,
// payouts, refunds, a clawback, a dispute) using Sway's built-in local engine and no PayPal calls.
// Every historical order is marked historical=true and is labelled as order history instead of linking to PayPal.
package history

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/attribution"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/marketplace"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/screening"
	"github.com/vivekyarra/sway/engine/internal/shop"
)

type historyOrder struct {
	request string
	daysAgo int
}

// Twelve past orders across all three categories.
var history = []historyOrder{
	{"Noise-cancelling headphones for long-haul flights to Tokyo, under $250", 13},
	{"A carry-on suitcase for weekly business trips that survives rough handling, under $220", 12},
	{"A standing desk that stays stable at full height for a tall person, under $450", 11},
	{"I need noise-cancelling headphones for long-haul flights, under $250. Comfort matters.", 10},
	{"Carry-on luggage that survives rough baggage handling on weekly trips, budget $220", 9},
	{"Standing desk for a tall programmer, must stay stable at full height, under $450", 8},
	{"Noise-cancelling headphones for long-haul flights and a red-eye, under $250", 7},
	{"The cheapest on-ear headphones you have", 6},
	{"Comfortable noise-cancelling headphones for long-haul travel, under $250", 5},
	{"A durable carry-on suitcase for weekly business trips, under $220, rough handling", 4},
	{"Noise-cancelling headphones for long-haul flights to Sydney, under $250", 2},
	{"A standing desk that stays stable at full height for a tall person, max $450", 1},
}

// Builder holds what history generation needs.
type Builder struct {
	DB    *db.DB
	Cfg   config.Config
	World *marketplace.World
	Pay   *payments.Service
	Log   *slog.Logger
}

// Installed reports whether the database already holds the catalog.
func Installed(ctx context.Context, d *db.DB) (bool, error) {
	var n int
	err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM merchants`).Scan(&n)
	return n > 0, err
}

// Install loads the catalog and builds the order history.
func (b *Builder) Install(ctx context.Context, cls screening.Classifier) error {
	w, err := marketplace.Install(ctx, b.DB, int(b.Cfg.VestingWindow.Seconds()), cls)
	if err != nil {
		return err
	}
	b.World = w
	for id, email := range b.Cfg.CreatorEmails {
		if _, err := b.DB.Pool.Exec(ctx, `UPDATE creators SET paypal_email=$2 WHERE id=$1`, id, email); err != nil {
			return err
		}
	}
	return b.History(ctx)
}

// History creates the 12 historical orders and drives them to a realistic mix of states.
func (b *Builder) History(ctx context.Context) error {
	ag := agent.New(b.DB, agent.Local{}, 8, b.Cfg.Attribution.Tau)
	sh := &shop.Service{DB: b.DB, Agent: ag, Categories: b.World.Categories, Samples: b.Cfg.Attribution.Samples, MaxSources: b.Cfg.Attribution.MaxSources}
	runner := &attribution.Runner{DB: b.DB, Agent: ag, Shop: sh, Cfg: b.Cfg.Attribution, AfterAllocate: b.Pay.Reconcile}
	var orders []string
	for i, h := range history {
		dv, err := sh.Decide(ctx, h.request)
		if err != nil {
			return fmt.Errorf("history decision %d: %w", i, err)
		}
		orderID := fmt.Sprintf("ord_seed_%02d", i+1)
		err = b.DB.Tx(ctx, func(q db.Q) error {
			var rate int
			if err := q.QueryRow(ctx, `SELECT commission_rate_bps FROM merchants WHERE id=$1`, dv.Chosen.MerchantID).Scan(&rate); err != nil {
				return err
			}
			// Historical orders use the production 30-day window so recent ones read as genuinely pending.
			if _, err := q.Exec(ctx, `INSERT INTO orders (id, decision_id, product_id, merchant_id, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode, status,
				captured_at, vests_at, buyer_email, historical, created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'historical','CAPTURED', now() - make_interval(days=>$8), now() - make_interval(days=>$8) + interval '30 days', 'historical-buyer@example.com', true,
				now() - make_interval(days=>$8))`,
				orderID, dv.ID, dv.Chosen.ID, dv.Chosen.MerchantID, dv.Chosen.PriceCents, rate, b.Cfg.SwayFeeBps, h.daysAgo); err != nil {
				return err
			}
			// The attribution run reserves the commission (same idempotency key a live capture uses).
			return nil
		})
		if err != nil {
			return err
		}
		if err := runner.Run(ctx, orderID); err != nil {
			return fmt.Errorf("history attribution %s: %w", orderID, err)
		}
		orders = append(orders, orderID)
	}
	// Orders 1-8 passed their return window; 9 was refunded before vesting.
	if _, err := b.Pay.Refund(ctx, orders[8], 0, "returned within the window", ""); err != nil {
		return err
	}
	if _, err := b.DB.Pool.Exec(ctx, `UPDATE orders SET vests_at = now() - interval '1 minute' WHERE id = ANY($1)`, orders[:8]); err != nil {
		return err
	}
	if _, err := b.Pay.VestDue(ctx); err != nil {
		return err
	}
	if _, err := b.Pay.RunPayouts(ctx, "historical"); err != nil {
		return err
	}
	// Order 3 (a standing desk) was returned after its commission was paid: a clawback the creator owes,
	// netted against their future earnings. It deliberately does not involve the headphone creators,
	// so the judge's live purchase pays Priya and Maya in full.
	if _, err := b.Pay.Refund(ctx, orders[2], 0, "returned after payout", ""); err != nil {
		return err
	}
	// Order 11 has an open (testMode) dispute: its commission is frozen.
	if err := b.Pay.HandleDispute(ctx, orders[10], payments.DisputeOpen, true); err != nil {
		return err
	}
	if b.Log != nil {
		b.Log.Info("order history ready", "orders", len(orders))
	}
	return nil
}

// Reset wipes all transactional data and rebuilds the catalog and history (test tools only).
// The LLM run cache is kept so the reset is fast and identical decisions stay identical.
func (b *Builder) Reset(ctx context.Context, cls screening.Classifier) error {
	err := b.DB.Tx(ctx, func(q db.Q) error {
		if _, err := q.Exec(ctx, `SET LOCAL sway.allow_reset = 'on'`); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `TRUNCATE ledger_entries, attributions, attribution_jobs, refunds, payout_items, payout_batches, webhook_events, jobs,
			orders, decisions, shopping_sessions, paypal_test_objects, idempotency_keys, source_products, sources, products, creators, merchants RESTART IDENTITY CASCADE`)
		return err
	})
	if err != nil {
		return err
	}
	return b.Install(ctx, cls)
}

// Package testkit wires a fully loaded database, the local agent and services for integration tests.
package testkit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/attribution"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/jobs"
	"github.com/vivekyarra/sway/engine/internal/marketplace"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/paypal"
	"github.com/vivekyarra/sway/engine/internal/shop"
	"github.com/vivekyarra/sway/engine/internal/testdb"
)

type Kit struct {
	DB    *db.DB
	World *marketplace.World
	Agent *agent.Agent
	Shop  *shop.Service
	Attr  *attribution.Runner
	Cfg   config.Config
}

// New returns a fully loaded environment on a fresh database with the deterministic agent.
func New(t testing.TB) *Kit {
	t.Helper()
	d := testdb.New(t)
	t.Setenv("DATABASE_URL", "postgres://unused")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, err := marketplace.Install(ctx, d, int(cfg.VestingWindow.Seconds()), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(d, agent.Local{}, cfg.LLM.Concurrency, cfg.Attribution.Tau)
	cats := w.Categories
	sh := &shop.Service{DB: d, Agent: ag, Categories: cats, Samples: cfg.Attribution.Samples, MaxSources: cfg.Attribution.MaxSources}
	at := &attribution.Runner{DB: d, Agent: ag, Shop: sh, Cfg: cfg.Attribution}
	return &Kit{DB: d, World: w, Agent: ag, Shop: sh, Attr: at, Cfg: cfg}
}

// InsertOrder creates a captured order row for a decision (payment wiring arrives with the PayPal layer).
func (k *Kit) InsertOrder(t testing.TB, id, decisionID, productID string, amount int64) {
	t.Helper()
	_, err := k.DB.Pool.Exec(context.Background(), `INSERT INTO orders (id, decision_id, product_id, merchant_id, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode, status, captured_at)
		VALUES ($1,$2,$3,'m_larkline',$4,800,1500,'test','CAPTURED',now())`, id, decisionID, productID, amount)
	if err != nil {
		t.Fatal(err)
	}
}

// Money wires the payment layer (PayPal test mode) and a job worker onto the kit.
type Money struct {
	*Kit
	Pay    *payments.Service
	Worker *jobs.Worker
}

func (k *Kit) WithMoney(t testing.TB) *Money {
	t.Helper()
	tm := &paypal.TestMode{DB: k.DB}
	pay := &payments.Service{DB: k.DB, PP: tm, Cfg: k.Cfg}
	k.Attr.AfterAllocate = pay.Reconcile
	handlers := pay.JobHandlers()
	handlers[jobs.TypeAttribution] = func(ctx context.Context, j jobs.Job) error {
		var p map[string]string
		_ = json.Unmarshal(j.Payload, &p)
		return k.Attr.Run(ctx, p["order_id"])
	}
	w := &jobs.Worker{DB: k.DB, Handlers: handlers, MaxAttempts: 3}
	return &Money{Kit: k, Pay: pay, Worker: w}
}

// Drain runs every ready job, including retries scheduled into the near future.
func (m *Money) Drain(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := m.Worker.Drain(ctx, 1000); err != nil {
			t.Fatal(err)
		}
		// Pull near-future retries (payout polling) forward so tests don't sleep.
		tag, err := m.DB.Pool.Exec(ctx, `UPDATE jobs SET run_after=now() WHERE status='QUEUED' AND run_after > now() AND run_after < now() + interval '10 seconds'`)
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() == 0 {
			return
		}
	}
}

// Buy runs the shopper flow end to end: decide, create the PayPal order, capture, attribute.
func (m *Money) Buy(t testing.TB, request string) (orderID, productID string) {
	t.Helper()
	ctx := context.Background()
	dv, err := m.Shop.Decide(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	co, err := m.Pay.CreateOrder(ctx, dv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pay.Capture(ctx, co.PayPalOrderID); err != nil {
		t.Fatal(err)
	}
	m.Drain(t)
	return co.OrderID, dv.Chosen.ID
}

// VestAndPay fast-forwards vesting and runs a payout cycle to completion.
func (m *Money) VestAndPay(t testing.TB) payments.PayoutPlan {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Pay.FastForward(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pay.VestDue(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := m.Pay.RunPayouts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m.Drain(t)
	return plan
}

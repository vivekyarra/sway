// Package testkit wires a seeded database, sim agent and services for integration tests.
package testkit

import (
	"context"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/attribution"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/seed"
	"github.com/vivekyarra/sway/engine/internal/shop"
	"github.com/vivekyarra/sway/engine/internal/testdb"
)

type Kit struct {
	DB    *db.DB
	World *seed.World
	Agent *agent.Agent
	Shop  *shop.Service
	Attr  *attribution.Runner
	Cfg   config.Config
}

// New returns a fully seeded environment on a fresh database with the deterministic agent.
func New(t testing.TB) *Kit {
	t.Helper()
	d := testdb.New(t)
	t.Setenv("DATABASE_URL", "postgres://unused")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, err := seed.Install(ctx, d, int(cfg.VestingWindow.Seconds()), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(d, agent.Sim{}, cfg.LLM.Concurrency, cfg.Attribution.Tau)
	cats := w.Categories
	sh := &shop.Service{DB: d, Agent: ag, Categories: cats, Samples: cfg.Attribution.Samples, MaxSources: cfg.Attribution.MaxSources}
	at := &attribution.Runner{DB: d, Agent: ag, Shop: sh, Cfg: cfg.Attribution}
	return &Kit{DB: d, World: w, Agent: ag, Shop: sh, Attr: at, Cfg: cfg}
}

// InsertOrder creates a captured order row for a decision (payment wiring arrives with the PayPal layer).
func (k *Kit) InsertOrder(t testing.TB, id, decisionID, productID string, amount int64) {
	t.Helper()
	_, err := k.DB.Pool.Exec(context.Background(), `INSERT INTO orders (id, decision_id, product_id, merchant_id, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode, status, captured_at)
		VALUES ($1,$2,$3,'m_larkline',$4,800,1500,'sim','CAPTURED',now())`, id, decisionID, productID, amount)
	if err != nil {
		t.Fatal(err)
	}
}

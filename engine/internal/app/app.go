// Package app wires configuration into running services: database, agent, payments, jobs, HTTP.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/api"
	"github.com/vivekyarra/sway/engine/internal/attribution"
	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/history"
	"github.com/vivekyarra/sway/engine/internal/jobs"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/paypal"
	"github.com/vivekyarra/sway/engine/internal/screening"
	"github.com/vivekyarra/sway/engine/internal/seed"
	"github.com/vivekyarra/sway/engine/internal/shop"
	"github.com/vivekyarra/sway/engine/internal/webui"
)

// Version is set at build time (-ldflags "-X .../app.Version=...").
var Version = "dev"

type App struct {
	Cfg        config.Config
	Log        *slog.Logger
	DB         *db.DB
	Agent      *agent.Agent
	Shop       *shop.Service
	Pay        *payments.Service
	Attr       *attribution.Runner
	History    *history.Builder
	Worker     *jobs.Worker
	Classifier screening.Classifier
}

// New builds every service. It migrates the schema and, on an empty database, installs the catalog and order history.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	d, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	a := &App{Cfg: cfg, Log: log, DB: d}

	var engine agent.Engine = agent.Local{}
	var explainer attribution.Explainer
	switch cfg.LLM.Mode {
	case "claude":
		c := agent.NewClaude(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.DecisionModel, cfg.LLM.ExplainModel, cfg.LLM.Timeout)
		engine, a.Classifier, explainer = c, c, c
	case "gemini", "openai":
		c := agent.NewOpenAICompat(cfg.LLM.Mode, cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.DecisionModel, cfg.LLM.ExplainModel, cfg.LLM.RPM, cfg.LLM.Timeout)
		engine, a.Classifier, explainer = c, c, c
	}
	a.Agent = agent.New(d, engine, cfg.LLM.Concurrency, cfg.Attribution.Tau)

	var provider paypal.Provider
	if cfg.PayPal.Mode == "sandbox" {
		provider = paypal.NewClient(cfg.PayPal.BaseURL, cfg.PayPal.ClientID, cfg.PayPal.ClientSecret, cfg.PayPal.WebhookID)
	} else {
		provider = &paypal.TestMode{DB: d, SettleAfter: 4 * time.Second}
	}

	w, err := seed.Load()
	if err != nil {
		return nil, err
	}
	a.Shop = &shop.Service{DB: d, Agent: a.Agent, Categories: w.Categories, Samples: cfg.Attribution.Samples, MaxSources: cfg.Attribution.MaxSources}
	if cfg.CatalogMode == "live" && cfg.Channel3APIKey != "" {
		a.Shop.Live = &catalog.Channel3{APIKey: cfg.Channel3APIKey, BaseURL: cfg.Channel3BaseURL}
	}
	a.Pay = &payments.Service{DB: d, PP: provider, Cfg: cfg, Log: log}
	a.Attr = &attribution.Runner{DB: d, Agent: a.Agent, Shop: a.Shop, Cfg: cfg.Attribution, Log: log, AfterAllocate: a.Pay.Reconcile}
	a.Attr.Explainer = explainer
	a.History = &history.Builder{DB: d, Cfg: cfg, World: w, Pay: a.Pay, Log: log}

	handlers := a.Pay.JobHandlers()
	handlers[jobs.TypeAttribution] = func(ctx context.Context, j jobs.Job) error {
		var p struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return err
		}
		return a.Attr.Run(ctx, p.OrderID)
	}
	a.Worker = &jobs.Worker{DB: d, Handlers: handlers, Concurrency: 4, Poll: cfg.WorkerPoll, Log: log}
	a.Pay.Wake = a.Worker.Wake

	if err := a.bootstrap(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// bootstrap installs the catalog and order history on an empty database. The web service and the worker may start at the
// same moment (Render), so a session advisory lock makes exactly one of them do it.
func (a *App) bootstrap(ctx context.Context) error {
	conn, err := a.DB.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(818181)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(818181)`) }()
	historical, err := history.Installed(ctx, a.DB)
	if err != nil || historical {
		return err
	}
	a.Log.Info("empty database: installing the catalog and order history")
	if err := a.History.Install(ctx, a.Classifier); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// HTTPHandler returns the API plus the embedded UI.
func (a *App) HTTPHandler() *api.Server {
	return &api.Server{
		Cfg: a.Cfg, DB: a.DB, Log: a.Log, Agent: a.Agent, Shop: a.Shop, Pay: a.Pay, History: a.History, Classifier: a.Classifier,
		Web: webui.Handler(a.Cfg.WebDir), TrustProxy: a.Cfg.TrustProxy, Version: Version,
	}
}

// RunBackground runs the job worker and periodic tasks until ctx ends, then waits for them to stop.
func (a *App) RunBackground(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.Worker.Run(ctx) }()
	every := func(name string, d time.Duration, fn func(context.Context) error) {
		if d <= 0 {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := fn(ctx); err != nil && ctx.Err() == nil {
						a.Log.Error("periodic task failed", "task", name, "err", err)
					}
				}
			}
		}()
	}
	every("vesting", a.Cfg.VestingTick, func(ctx context.Context) error {
		n, err := a.Pay.VestDue(ctx)
		if n > 0 {
			a.Log.Info("vested orders", "count", n)
		}
		return err
	})
	every("payouts", a.Cfg.AutoPayout, func(ctx context.Context) error {
		plan, err := a.Pay.RunPayouts(ctx, "")
		if plan.Items > 0 {
			a.Log.Info("payout batch created", "batch", plan.BatchID, "items", plan.Items, "total_cents", plan.TotalCents)
		}
		return err
	})
	every("housekeeping", time.Hour, func(ctx context.Context) error {
		_, err := a.DB.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - interval '24 hours'`)
		if err == nil {
			_, err = a.DB.Pool.Exec(ctx, `DELETE FROM jobs WHERE status='DONE' AND updated_at < now() - interval '7 days'`)
		}
		return err
	})
	wg.Wait()
}

// Close releases resources.
func (a *App) Close() { a.DB.Close() }

// Categories exposes catalog categories (used by tooling).
func (a *App) Categories() map[string]catalog.CategoryDef { return a.Shop.Categories }

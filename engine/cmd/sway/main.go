// Command sway runs the Sway engine.
//
//	sway serve    HTTP API + UI (and the background worker when WORKER_INLINE=true)
//	sway worker   background worker only (attribution, vesting, payouts, webhooks)
//	sway migrate  apply database migrations
//	sway install  install the catalog and order history if the database is empty
//	sway reset    wipe and rebuild all data (TEST_TOOLS=true only)
//	sway healthcheck  exit 0 if GET /api/health on $PORT succeeds (container health checks)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vivekyarra/sway/engine/internal/app"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	if err := run(cmd, log); err != nil {
		log.Error("fatal", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func run(cmd string, log *slog.Logger) error {
	if cmd == "healthcheck" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		c := http.Client{Timeout: 4 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + port + "/api/health")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "migrate":
		d, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Migrate(ctx)
	case "install":
		a, err := app.New(ctx, cfg, log)
		if err != nil {
			return err
		}
		a.Close()
		return nil
	case "reset":
		if !cfg.TestTools {
			return errors.New("reset is only available with DEMO_MODE=true")
		}
		a, err := app.New(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer a.Close()
		return a.History.Reset(ctx, a.Classifier)
	case "worker":
		a, err := app.New(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer a.Close()
		log.Info("worker started", "llm", a.Agent.Describe(), "paypal", a.Pay.PP.Mode())
		a.RunBackground(ctx)
		return nil
	case "serve":
		a, err := app.New(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer a.Close()
		srv := &http.Server{
			Addr: cfg.Addr, Handler: a.HTTPHandler().Handler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 16,
		}
		bgCtx, bgCancel := context.WithCancel(context.Background())
		bgDone := make(chan struct{})
		if cfg.WorkerInline {
			go func() { a.RunBackground(bgCtx); close(bgDone) }()
		} else {
			close(bgDone)
		}
		errc := make(chan error, 1)
		go func() {
			log.Info("listening", "addr", cfg.Addr, "llm", a.Agent.Describe(), "paypal", a.Pay.PP.Mode(), "test_tools", cfg.TestTools, "worker_inline", cfg.WorkerInline, "version", app.Version)
			errc <- srv.ListenAndServe()
		}()
		select {
		case err := <-errc:
			bgCancel()
			<-bgDone
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
		}
		log.Info("shutting down: draining requests and jobs")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		bgCancel()
		<-bgDone
		return nil
	}
	return fmt.Errorf("unknown command %q (serve, worker, migrate, install, reset, healthcheck)", cmd)
}

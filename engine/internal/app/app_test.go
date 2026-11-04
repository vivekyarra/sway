package app_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/app"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/testdb"
)

// Web service and worker booting at the same moment on an empty database must seed exactly once.
func TestConcurrentBootstrapSeedsOnce(t *testing.T) {
	d := testdb.New(t)
	url := d.Pool.Config().ConnString()
	t.Setenv("DATABASE_URL", url)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	apps := make(chan *app.App, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := app.New(context.Background(), cfg, log)
			errs <- err
			if err == nil {
				apps <- a
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(apps)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for a := range apps {
		a.Close()
	}
	var orders, merchants int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&orders)
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM merchants`).Scan(&merchants)
	if orders != 12 || merchants != 1 {
		t.Fatalf("expected one seeded history, got %d orders, %d merchants", orders, merchants)
	}
}

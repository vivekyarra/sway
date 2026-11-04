// Package testdb gives each test its own freshly migrated Postgres database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vivekyarra/sway/engine/internal/db"
)

// AdminURL is the maintenance connection used to create/drop per-test databases.
func AdminURL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://sway@localhost:5432/postgres?sslmode=disable"
}

// New creates database sway_t_<random>, migrates it and drops it when the test ends.
// If Postgres is unreachable the test is skipped, unless SWAY_REQUIRE_DB=1 (CI), where it fails.
func New(t testing.TB) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		if os.Getenv("SWAY_REQUIRE_DB") == "1" {
			t.Fatalf("postgres required but unreachable: %v", err)
		}
		t.Skipf("postgres unreachable (%v); set TEST_DATABASE_URL", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "sway_t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	_ = admin.Close(ctx)

	u, _ := url.Parse(AdminURL())
	u.Path = "/" + name
	d, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if a, err := pgx.Connect(c, AdminURL()); err == nil {
			_, _ = a.Exec(c, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
			_ = a.Close(c)
		}
	})
	return d
}

package attribution_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/testkit"
)

const req = "I need noise-cancelling headphones for long-haul flights, under $250."

type row struct {
	source, creator string
	phi             float64
	amount          int64
	role            string
	explanation     string
}

func rows(t *testing.T, k *testkit.Kit, orderID string) map[string]row {
	t.Helper()
	rs, err := k.DB.Pool.Query(context.Background(), `SELECT source_id, creator_id, phi, amount_cents, role, COALESCE(explanation,'') FROM attributions WHERE order_id=$1`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]row{}
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.source, &r.creator, &r.phi, &r.amount, &r.role, &r.explanation); err != nil {
			t.Fatal(err)
		}
		out[r.source] = r
	}
	return out
}

// Scenario E1 (attribution half) + E8 + E11: Priya and Maya paid, Dev and Tomás $0, blocked/duplicate excluded, books balance.
func TestCuratedAttribution(t *testing.T) {
	k := testkit.New(t)
	ctx := context.Background()
	dv, err := k.Shop.Decide(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if dv.Chosen.ID != "p_halvo_q7" {
		t.Fatalf("agent chose %s", dv.Chosen.ID)
	}
	for _, s := range dv.Sources {
		if s.ID == "s_dealbot_01" || s.ID == "s_copycat_01" {
			t.Fatalf("screened source %s reached the agent", s.ID)
		}
	}
	if len(dv.Ignored) != 2 {
		t.Fatalf("expected 2 ignored sources with reasons, got %+v", dv.Ignored)
	}
	// Remove Priya -> flips to Brisane.
	rv, err := k.Shop.Rerun(ctx, dv.ID, []string{"s_priya_01"})
	if err != nil || !rv.Flipped || rv.Chosen.ID != "p_brisane_anc2" {
		t.Fatalf("rerun %+v %v", rv, err)
	}
	loo, err := k.Shop.Influence(ctx, dv.ID)
	if err != nil || loo.Deltas["s_priya_01"] <= loo.Deltas["s_maya_01"] {
		t.Fatalf("influence %+v %v", loo, err)
	}

	k.InsertOrder(t, "ord_1", dv.ID, dv.Chosen.ID, dv.Chosen.PriceCents)
	if err := k.Attr.Run(ctx, "ord_1"); err != nil {
		t.Fatal(err)
	}
	// Running again is a no-op.
	if err := k.Attr.Run(ctx, "ord_1"); err != nil {
		t.Fatal(err)
	}
	rs := rows(t, k, "ord_1")
	t.Logf("attributions: %+v", rs)
	if rs["s_priya_01"].amount <= rs["s_maya_01"].amount || rs["s_maya_01"].amount <= 0 {
		t.Fatalf("priya should earn most, maya some: %+v", rs)
	}
	if rs["s_dev_01"].amount != 0 || rs["s_dev_01"].phi > 0 {
		t.Fatalf("dev argued against the purchase and must earn 0: %+v", rs["s_dev_01"])
	}
	if rs["s_tomas_01"].amount != 0 || math.Abs(rs["s_tomas_01"].phi) > 0.01 {
		t.Fatalf("tomas is neutral: %+v", rs["s_tomas_01"])
	}
	for _, r := range rs {
		if r.explanation == "" {
			t.Fatalf("missing explanation for %s", r.source)
		}
	}
	var share, gap float64
	var method, coal string
	if err := k.DB.Pool.QueryRow(ctx, `SELECT share, efficiency_gap, method, coalitions::text FROM attribution_jobs WHERE order_id='ord_1'`).Scan(&share, &gap, &method, &coal); err != nil {
		t.Fatal(err)
	}
	if method != "exact" || math.Abs(gap) > 1e-9 || share < 0.3 || share > 1 {
		t.Fatalf("method %s gap %v share %v", method, gap, share)
	}
	var c struct {
		Coalitions []any `json:"coalitions"`
	}
	_ = json.Unmarshal([]byte(coal), &c)
	if len(c.Coalitions) != 16 { // 4 legitimate sources -> 2^4 coalitions
		t.Fatalf("expected 16 coalitions, got %d", len(c.Coalitions))
	}
	s, err := ledger.SummarizeOrder(ctx, k.DB.Pool, "ord_1")
	if err != nil || !s.Balanced || s.Commission != 1832 {
		t.Fatalf("ledger %+v %v", s, err)
	}
	if v, _ := ledger.Verify(ctx, k.DB.Pool); !v.OK || !v.TrialZero {
		t.Fatalf("verify %+v", v)
	}
}

// Scenario E9: content didn't matter -> share 0, the whole commission returns to the merchant.
func TestContentDidNotMatterReturnsCommission(t *testing.T) {
	k := testkit.New(t)
	ctx := context.Background()
	dv, err := k.Shop.Decide(ctx, "The cheapest on-ear headphones you have.")
	if err != nil || dv.Chosen.ID != "p_orrin_air" {
		t.Fatalf("%+v %v", dv.Chosen, err)
	}
	k.InsertOrder(t, "ord_2", dv.ID, dv.Chosen.ID, dv.Chosen.PriceCents)
	if err := k.Attr.Run(ctx, "ord_2"); err != nil {
		t.Fatal(err)
	}
	s, _ := ledger.SummarizeOrder(ctx, k.DB.Pool, "ord_2")
	if s.CreatorPoolNet != 0 || s.SwayFee != 0 || s.ReturnedToMerchant != s.Commission || !s.Balanced {
		t.Fatalf("expected full return to merchant: %+v", s)
	}
}

// Budget guard: past MAX_ATTRIBUTION_CALLS the engine falls back to Monte Carlo and marks the result approximate.
func TestMonteCarloFallback(t *testing.T) {
	k := testkit.New(t)
	k.Attr.Cfg.MaxCalls = 10
	ctx := context.Background()
	dv, err := k.Shop.Decide(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	k.InsertOrder(t, "ord_3", dv.ID, dv.Chosen.ID, dv.Chosen.PriceCents)
	if err := k.Attr.Run(ctx, "ord_3"); err != nil {
		t.Fatal(err)
	}
	var approx bool
	var gap float64
	if err := k.DB.Pool.QueryRow(ctx, `SELECT approximate, efficiency_gap FROM attribution_jobs WHERE order_id='ord_3'`).Scan(&approx, &gap); err != nil || !approx {
		t.Fatalf("expected approximate, got %v %v", approx, err)
	}
	rs := rows(t, k, "ord_3")
	if rs["s_priya_01"].amount <= 0 {
		t.Fatalf("priya should still be credited: %+v", rs)
	}
	if s, _ := ledger.SummarizeOrder(ctx, k.DB.Pool, "ord_3"); !s.Balanced {
		t.Fatalf("unbalanced %+v", s)
	}
}

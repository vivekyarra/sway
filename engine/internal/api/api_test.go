package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/api"
	"github.com/vivekyarra/sway/engine/internal/history"
	"github.com/vivekyarra/sway/engine/internal/testkit"
)

func server(t *testing.T, demoMode bool) (*httptest.Server, *testkit.Money) {
	t.Helper()
	m := testkit.New(t).WithMoney(t)
	m.Cfg.TestTools = demoMode
	m.Pay.Cfg.TestTools = demoMode
	s := &api.Server{Cfg: m.Cfg, DB: m.DB, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Agent: m.Agent, Shop: m.Shop, Pay: m.Pay,
		History: &history.Builder{DB: m.DB, Cfg: m.Cfg, World: m.World, Pay: m.Pay}, Version: "test"}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, m
}

func call(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func TestHTTPFlowAndIdempotency(t *testing.T) {
	ts, m := server(t, true)
	resp, d := call(t, ts, "POST", "/api/agent/decide", `{"request":"I need noise-cancelling headphones for long-haul flights, under $250."}`, nil)
	if resp.StatusCode != 200 || d["chosen"].(map[string]any)["id"] != "p_halvo_q7" {
		t.Fatalf("decide %d %v", resp.StatusCode, d)
	}
	id := d["id"].(string)
	resp, r := call(t, ts, "POST", "/api/decisions/"+id+"/rerun", `{"exclude_source_ids":["s_priya_01"]}`, nil)
	if resp.StatusCode != 200 || r["flipped"] != true {
		t.Fatalf("rerun %d %v", resp.StatusCode, r)
	}
	h := map[string]string{"Idempotency-Key": "abc"}
	resp, o1 := call(t, ts, "POST", "/api/paypal/orders", `{"decision_id":"`+id+`"}`, h)
	if resp.StatusCode != 201 {
		t.Fatalf("create %d %v", resp.StatusCode, o1)
	}
	resp, o2 := call(t, ts, "POST", "/api/paypal/orders", `{"decision_id":"`+id+`"}`, h)
	if resp.Header.Get("Idempotent-Replayed") != "true" || o2["order_id"] != o1["order_id"] {
		t.Fatalf("replay not served: %v %v", resp.Header, o2)
	}
	resp, _ = call(t, ts, "POST", "/api/paypal/orders", `{"decision_id":"different"}`, h)
	if resp.StatusCode != 422 {
		t.Fatalf("key reuse with a different body must be 422, got %d", resp.StatusCode)
	}
	resp, cap := call(t, ts, "POST", "/api/paypal/orders/"+o1["paypal_order_id"].(string)+"/capture", ``, nil)
	if resp.StatusCode != 200 || cap["status"] != "CAPTURED" {
		t.Fatalf("capture %d %v", resp.StatusCode, cap)
	}
	m.Drain(t)
	resp, ord := call(t, ts, "GET", "/api/orders/"+o1["order_id"].(string), ``, nil)
	if resp.StatusCode != 200 || ord["attribution"].(map[string]any)["status"] != "DONE" || ord["split"].(map[string]any)["balanced"] != true {
		t.Fatalf("order %v", ord)
	}
	resp, ref := call(t, ts, "POST", "/api/merchant/orders/"+o1["order_id"].(string)+"/refund", `{"amount_cents":999999}`, nil)
	if resp.StatusCode != 422 {
		t.Fatalf("over-refund must be 422: %d %v", resp.StatusCode, ref)
	}
	resp, _ = call(t, ts, "GET", "/api/merchant/overview", ``, nil)
	if resp.StatusCode != 200 {
		t.Fatal("overview failed")
	}
	var v map[string]any
	resp, st := call(t, ts, "POST", "/api/merchant/settings", `{"commission_rate_bps":1000,"vesting_window_seconds":60}`, nil)
	if resp.StatusCode != 200 || st["commission_rate_bps"].(float64) != 1000 {
		t.Fatalf("settings %d %v", resp.StatusCode, st)
	}
	if resp, _ := call(t, ts, "POST", "/api/merchant/settings", `{"commission_rate_bps":9000}`, nil); resp.StatusCode != 422 {
		t.Fatalf("out-of-range commission must be 422, got %d", resp.StatusCode)
	}
	_, d2 := call(t, ts, "POST", "/api/agent/decide", `{"request":"A standing desk that stays stable at full height for a tall person, under $450."}`, nil)
	_, co := call(t, ts, "POST", "/api/paypal/orders", `{"decision_id":"`+d2["id"].(string)+`"}`, nil)
	call(t, ts, "POST", "/api/paypal/orders/"+co["paypal_order_id"].(string)+"/capture", ``, nil)
	m.Drain(t)
	_, o3 := call(t, ts, "GET", "/api/orders/"+co["order_id"].(string), ``, nil)
	if c := o3["split"].(map[string]any)["commission_cents"].(float64); c != 3990 { // 10% of $399
		t.Fatalf("new orders must use the new 10%% rate, commission %v", c)
	}
	resp, v = call(t, ts, "GET", "/api/ledger/verify", ``, nil)
	if resp.StatusCode != 200 || v["ok"] != true {
		t.Fatalf("verify %v", v)
	}
}

func TestHardening(t *testing.T) {
	ts, _ := server(t, false)
	resp, _ := call(t, ts, "GET", "/api/health", ``, nil)
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "X-Request-Id", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Fatalf("missing header %s", h)
		}
	}
	// Test-tool routes do not exist in production mode.
	for _, p := range []string{"/api/admin/reset", "/api/admin/fast-forward", "/api/merchant/orders/x/test-dispute"} {
		if resp, _ := call(t, ts, "POST", p, `{}`, nil); resp.StatusCode != 404 {
			t.Fatalf("%s should 404 without test tools, got %d", p, resp.StatusCode)
		}
	}
	// Test mode never accepts external webhooks; forged events are rejected.
	resp, _ = call(t, ts, "POST", "/api/webhooks/paypal", `{"id":"WH-1","event_type":"PAYMENT.CAPTURE.REFUNDED","resource":{}}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("forged webhook must be rejected, got %d", resp.StatusCode)
	}
	// Oversized bodies are refused.
	big := `{"request":"` + strings.Repeat("a", 70<<10) + `"}`
	if resp, _ := call(t, ts, "POST", "/api/agent/decide", big, nil); resp.StatusCode < 400 {
		t.Fatalf("oversized body accepted: %d", resp.StatusCode)
	}
	// Unknown API routes are JSON 404s.
	if resp, body := call(t, ts, "GET", "/api/nope", ``, nil); resp.StatusCode != 404 || body["error"] == nil {
		t.Fatalf("unknown route %d %v", resp.StatusCode, body)
	}
	// The agent endpoint is rate limited per client.
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		resp, _ := call(t, ts, "POST", "/api/agent/decide", `{"request":"a pony"}`, nil)
		if resp.StatusCode == 429 {
			limited = resp.Header.Get("Retry-After") != ""
		}
	}
	if !limited {
		t.Fatal("rate limit never triggered")
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(raw, []byte("sway_rate_limited_total")) {
		t.Fatal("metrics missing")
	}
}

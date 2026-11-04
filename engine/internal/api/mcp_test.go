package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func rpc(t *testing.T, url, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func result(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	r, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	return r
}

// A full agent session over MCP: initialize, list tools, shop, explain, counterfactual, verify.
func TestMCPAgentSession(t *testing.T) {
	ts, _ := server(t, true)
	code, out := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
	init := result(t, out)
	if code != 200 || init["protocolVersion"] != "2025-06-18" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize %d %v", code, out)
	}
	if code, _ := rpc(t, ts.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); code != 202 {
		t.Fatalf("notification should be 202, got %d", code)
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	tools := result(t, out)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(tools))
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shop_recommend","arguments":{"request":"I need noise-cancelling headphones for long-haul flights, under $250."}}}`, nil)
	rec := result(t, out)
	sc := rec["structuredContent"].(map[string]any)
	if rec["isError"] == true || sc["pick"].(map[string]any)["id"] != "p_halvo_q7" || !strings.Contains(sc["checkout_url"].(string), "/shop/?decision=dec_") {
		t.Fatalf("shop_recommend %v", rec)
	}
	if len(sc["screened_out"].([]any)) != 2 {
		t.Fatalf("screened sources missing: %v", sc["screened_out"])
	}
	dec := sc["decision_id"].(string)
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"explain_influence","arguments":{"decision_id":"`+dec+`"}}}`, nil)
	if text := result(t, out)["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "Priya Raman") || !strings.Contains(text, "pivotal") {
		t.Fatalf("explain_influence: %s", text)
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"counterfactual","arguments":{"decision_id":"`+dec+`","exclude_source_ids":["s_priya_01"]}}}`, nil)
	if cf := result(t, out)["structuredContent"].(map[string]any); cf["flipped"] != true {
		t.Fatalf("counterfactual %v", cf)
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"verify_ledger","arguments":{}}}`, nil)
	if v := result(t, out)["structuredContent"].(map[string]any); v["ok"] != true {
		t.Fatalf("verify %v", v)
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"order_attribution","arguments":{"order_id":"nope"}}}`, nil)
	if result(t, out)["isError"] != true {
		t.Fatal("unknown order should be a tool error")
	}
	_, out = rpc(t, ts.URL, `{"jsonrpc":"2.0","id":8,"method":"bogus"}`, nil)
	if out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method %v", out)
	}
	if code, _ := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":9,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("foreign origin should be 403, got %d", code)
	}
	resp, _ := http.Get(ts.URL + "/mcp")
	if resp.StatusCode != 405 {
		t.Fatalf("GET /mcp should be 405, got %d", resp.StatusCode)
	}
}

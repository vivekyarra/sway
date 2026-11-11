package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivekyarra/sway/engine/internal/agent"
)

func mockChat(t *testing.T, statuses []int, replies ...string) (*httptest.Server, *atomic.Int32, *[]map[string]any) {
	t.Helper()
	var n atomic.Int32
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		i := int(n.Add(1)) - 1
		if i < len(statuses) && statuses[i] != http.StatusOK {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":{"message":"rate limited"}}`, statuses[i])
			return
		}
		j := i - len(statuses)
		if j < 0 {
			j = 0
		}
		if j >= len(replies) {
			j = len(replies) - 1
		}
		content, _ := json.Marshal("```json\n" + replies[j] + "\n```")
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(content)+`},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &bodies
}

func TestOpenAICompatDecideRetriesRateLimit(t *testing.T) {
	ps, all := world(t)
	in := agent.Input{Request: headphonesReq, Candidates: pick(ps, "headphones"), Sources: srcs(all, "s_priya_01")}
	valid := `{"scores":[{"product_id":"p_brisane_anc2","score":70},{"product_id":"p_halvo_q7","score":85},{"product_id":"p_orrin_air","score":50},{"product_id":"p_tellum_pro","score":30}],"chosen_product_id":"p_halvo_q7","rationale":"Comfort on long flights.","relied_on_source_ids":["s_priya_01"]}`
	srv, calls, bodies := mockChat(t, []int{http.StatusTooManyRequests}, valid)
	c := agent.NewOpenAICompat("gemini", srv.URL, "k", "m", "", 0, 10*time.Second)
	out, err := c.Decide(context.Background(), in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || out.Chosen != "p_halvo_q7" || c.Fallbacks.Load() != 0 {
		t.Fatalf("calls=%d out=%+v fallbacks=%d", calls.Load(), out, c.Fallbacks.Load())
	}
	b := (*bodies)[1]
	rf, _ := b["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || b["model"] != "m" {
		t.Fatalf("unexpected request %v", b)
	}
	if strings.Contains(mustJSON(t, rf), "additionalProperties") {
		t.Fatal("schema must be portable (no additionalProperties)")
	}
}

func TestOpenAICompatFallsBackToLocal(t *testing.T) {
	ps, all := world(t)
	in := agent.Input{Request: headphonesReq, Candidates: pick(ps, "headphones"), Sources: srcs(all, "s_priya_01")}
	srv, _, _ := mockChat(t, nil, `{"scores":[],"chosen_product_id":"nope","rationale":"x","relied_on_source_ids":[]}`)
	c := agent.NewOpenAICompat("gemini", srv.URL, "k", "m", "", 0, 10*time.Second)
	out, err := c.Decide(context.Background(), in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Fallbacks.Load() != 1 || out.Chosen == "" {
		t.Fatalf("expected local fallback, got %+v fallbacks=%d", out, c.Fallbacks.Load())
	}
}

func TestOpenAICompatExplainStripsNumbers(t *testing.T) {
	srv, _, _ := mockChat(t, nil, `{"explanation":"Your comfort notes tipped it by 42%."}`)
	c := agent.NewOpenAICompat("gemini", srv.URL, "k", "m", "", 0, 10*time.Second)
	got, err := c.Explain(context.Background(), agent.ExplainInput{Request: "r", Purchased: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "42") {
		t.Fatalf("numbers must be stripped: %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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

func mockAnthropic(t *testing.T, replies ...string) (*httptest.Server, *atomic.Int32, *[]map[string]any) {
	t.Helper()
	var n atomic.Int32
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		i := int(n.Add(1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		text, _ := json.Marshal(replies[i])
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":`+string(text)+`}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &bodies
}

func TestClaudeRequestShapeAndRetry(t *testing.T) {
	ps, all := world(t)
	cands := pick(ps, "headphones")
	in := agent.Input{Request: headphonesReq, Candidates: cands, Sources: srcs(all, "s_priya_01")}
	valid := `{"scores":[{"product_id":"p_brisane_anc2","score":70},{"product_id":"p_halvo_q7","score":85},{"product_id":"p_orrin_air","score":50},{"product_id":"p_tellum_pro","score":30}],"chosen_product_id":"p_halvo_q7","rationale":"Comfort on long flights.","relied_on_source_ids":["s_priya_01","s_unknown"]}`
	srv, calls, bodies := mockAnthropic(t, `{"scores":[],"chosen_product_id":"nope","rationale":"x","relied_on_source_ids":[]}`, valid)
	c := agent.NewClaude("test-key", srv.URL, "claude-haiku-5-5", "claude-haiku-5-5", 10*time.Second)
	out, err := c.Decide(context.Background(), in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one retry after invalid output, got %d calls", calls.Load())
	}
	if out.Chosen != "p_halvo_q7" || len(out.ReliedOn) != 1 {
		t.Fatalf("unexpected output %+v (unknown relied-on ids must be dropped)", out)
	}
	b := (*bodies)[0]
	if _, ok := b["temperature"]; ok {
		t.Fatal("temperature must not be sent (rejected by current models)")
	}
	if _, ok := b["tool_choice"]; ok {
		t.Fatal("forced tool_choice must not be sent")
	}
	oc, _ := b["output_config"].(map[string]any)
	if oc == nil || oc["format"] == nil {
		t.Fatalf("structured output format missing: %v", b)
	}
	sys, _ := json.Marshal(b["system"])
	if !strings.Contains(string(sys), "neutral shopping agent") {
		t.Fatalf("system prompt missing: %s", sys)
	}
	msgs, _ := json.Marshal(b["messages"])
	if !strings.Contains(string(msgs), "s_priya_01") || !strings.Contains(string(msgs), "p_tellum_pro") {
		t.Fatal("user message must carry candidates and sources")
	}
}

func TestClaudeFailsAfterSecondInvalid(t *testing.T) {
	ps, _ := world(t)
	srv, _, _ := mockAnthropic(t, `not json`)
	c := agent.NewClaude("k", srv.URL, "m", "m", 5*time.Second)
	if _, err := c.Decide(context.Background(), agent.Input{Request: "x", Candidates: pick(ps, "headphones")}, 0); err == nil {
		t.Fatal("expected failure")
	}
}

func TestClaudeExplainStripsNumbers(t *testing.T) {
	srv, _, _ := mockAnthropic(t, `{"explanation":"Your review drove 62% of the decision and earned $6.74."}`)
	c := agent.NewClaude("k", srv.URL, "m", "m", 5*time.Second)
	got, err := c.Explain(context.Background(), agent.ExplainInput{Request: "r", Purchased: "Halvo Q7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "0123456789$%") {
		t.Fatalf("numbers must be stripped: %q", got)
	}
}

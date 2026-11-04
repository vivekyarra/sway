package agent_test

import (
	"testing"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/catalog"
)

var paraphrases = []string{
	"I need noise-cancelling headphones for long-haul flights, under $250.",
	"Noise cancelling headphones for long haul flights, budget $250",
	"Looking for ANC headphones for long-haul travel under $250",
	"Best noise-cancelling headphones for 12 hour flights under $250",
	"Headphones with noise cancelling for long flights, max $250",
	"I fly long-haul every month; need noise-cancelling headphones under $250",
	"Comfortable noise-cancelling headphones for long-haul flights below $250",
	"Noise-cancelling headphones for long-haul flights and travel, up to $250",
	"Need ANC headphones for long haul flights, less than $250",
	"Which noise-cancelling headphones should I get for long-haul flights? Under $250.",
}

// E2 robustness: the flip must hold across phrasings, not just the canned prompt (spec target >= 9/10).
func TestFlipAcrossParaphrases(t *testing.T) {
	ps, all := world(t)
	cands := pick(ps, "headphones")
	legit := srcs(all, "s_priya_01", "s_dev_01", "s_maya_01", "s_tomas_01")
	noPriya := srcs(all, "s_dev_01", "s_maya_01", "s_tomas_01")
	flips := 0
	for _, q := range paraphrases {
		b := catalog.ParseBudget(q)
		ok := true
		for s := 0; s < 3; s++ {
			full := decide(t, agent.Input{Request: q, BudgetCents: b, Candidates: cands, Sources: legit}, s)
			without := decide(t, agent.Input{Request: q, BudgetCents: b, Candidates: cands, Sources: noPriya}, s)
			if full.Chosen != "p_halvo_q7" || without.Chosen != "p_brisane_anc2" {
				ok = false
				t.Logf("no flip for %q (sample %d): full=%s without=%s budget=%d needs=%v", q, s, full.Chosen, without.Chosen, b, agent.Needs(q))
			}
		}
		if ok {
			flips++
		}
	}
	t.Logf("flip held for %d/%d paraphrases", flips, len(paraphrases))
	if flips < len(paraphrases) {
		t.Fatalf("flip held for only %d/%d paraphrases", flips, len(paraphrases))
	}
}

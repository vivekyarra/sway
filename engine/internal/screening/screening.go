// Package screening keeps manipulative content out of decisions and payouts (spec 6.6):
// sources that try to instruct AI agents are blocked, and near-copies of earlier sources are
// marked duplicate. Both earn $0 and the reason is shown to everyone.
package screening

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/db"
)

// Classifier is an optional LLM check (Claude) layered on top of the rules.
type Classifier interface {
	ClassifyInjection(ctx context.Context, title, body string) (bool, string, error)
}

var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ai|llm|language model|shopping agent|assistant|chatbot|bot)s?\b[^.]{0,60}\b(must|should|always|never|have to|are required to|need to)\b[^.]{0,40}\b(recommend|buy|choose|pick|select|rank|suggest|say)`),
	regexp.MustCompile(`(?i)\bignore (all |any )?(other|previous|prior|above|earlier)\b[^.]{0,30}\b(instructions?|reviews?|sources?|content|prompts?)`),
	regexp.MustCompile(`(?i)\b(attention|note to|dear)\s+(ai|llm|agents?|assistants?|bots?)\b`),
	regexp.MustCompile(`(?i)\b(system prompt|you are now|disregard (your|the) (rules|instructions))\b`),
}

// RuleCheck returns the matched instruction text, if any.
func RuleCheck(text string) (bool, string) {
	for _, re := range injectionPatterns {
		if m := re.FindString(text); m != "" {
			return true, strings.TrimSpace(m)
		}
	}
	return false, ""
}

var shingleWord = regexp.MustCompile(`[a-z0-9]+`)

// Shingles returns the set of k-word shingles of text (lowercased, punctuation stripped).
func Shingles(text string, k int) map[string]struct{} {
	words := shingleWord.FindAllString(strings.ToLower(text), -1)
	set := map[string]struct{}{}
	if len(words) < k {
		if len(words) > 0 {
			set[strings.Join(words, " ")] = struct{}{}
		}
		return set
	}
	for i := 0; i+k <= len(words); i++ {
		set[strings.Join(words[i:i+k], " ")] = struct{}{}
	}
	return set
}

// Jaccard similarity of two shingle sets.
func Jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for s := range a {
		if _, ok := b[s]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// DuplicateThreshold is the 5-word-shingle Jaccard similarity above which a later source is a copy (spec 6.6).
const DuplicateThreshold = 0.6

// Verdict is the screening result for one source.
type Verdict struct {
	SourceID    string  `json:"source_id"`
	Status      string  `json:"status"` // active | blocked_injection | duplicate
	Reason      string  `json:"reason"`
	DuplicateOf string  `json:"duplicate_of,omitempty"`
	Similarity  float64 `json:"similarity,omitempty"`
	ScreenedBy  string  `json:"screened_by"`
}

// Screen evaluates every source. Duplicates are judged against strictly earlier-published sources by other
// creators, so the original always keeps its credit and the copy never does.
func Screen(ctx context.Context, sources []catalog.Source, cls Classifier) ([]Verdict, error) {
	sorted := append([]catalog.Source(nil), sources...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].PublishedAt.Equal(sorted[j].PublishedAt) {
			return sorted[i].PublishedAt.Before(sorted[j].PublishedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	shingles := make([]map[string]struct{}, len(sorted))
	for i, s := range sorted {
		shingles[i] = Shingles(s.Body, 5)
	}
	var out []Verdict
	for i, s := range sorted {
		v := Verdict{SourceID: s.ID, Status: "active", ScreenedBy: "rules"}
		if hit, evidence := RuleCheck(s.Title + ". " + s.Body); hit {
			v.Status, v.Reason = "blocked_injection", fmt.Sprintf("Blocked: tries to instruct AI agents (%q). Earns $0.", evidence)
		} else if cls != nil {
			v.ScreenedBy = "rules+claude"
			flag, evidence, err := cls.ClassifyInjection(ctx, s.Title, s.Body)
			if err != nil {
				return nil, fmt.Errorf("screening %s: %w", s.ID, err)
			}
			if flag {
				v.Status, v.Reason = "blocked_injection", fmt.Sprintf("Blocked: tries to instruct AI agents (%s). Earns $0.", strings.TrimSpace(evidence))
			}
		}
		if v.Status == "active" {
			bestJ, bestIdx := 0.0, -1
			for j := 0; j < i; j++ {
				if sorted[j].CreatorID == s.CreatorID || !sorted[j].PublishedAt.Before(s.PublishedAt) {
					continue
				}
				if jac := Jaccard(shingles[i], shingles[j]); jac > bestJ {
					bestJ, bestIdx = jac, j
				}
			}
			if bestIdx >= 0 && bestJ >= DuplicateThreshold {
				orig := sorted[bestIdx]
				v.Status, v.DuplicateOf, v.Similarity = "duplicate", orig.ID, bestJ
				v.Reason = fmt.Sprintf("Duplicate of %s's earlier source (%.0f%% overlap). Earns $0.", orig.CreatorName, bestJ*100)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	return out, nil
}

// Apply stores verdicts on the sources table.
func Apply(ctx context.Context, q db.Q, verdicts []Verdict) error {
	for _, v := range verdicts {
		var sim any
		if v.Similarity > 0 {
			sim = v.Similarity
		}
		if _, err := q.Exec(ctx, `UPDATE sources SET status=$2, status_reason=$3, duplicate_of_id=NULLIF($4,''), similarity=$5, screened_by=$6 WHERE id=$1`,
			v.SourceID, v.Status, v.Reason, v.DuplicateOf, sim, v.ScreenedBy); err != nil {
			return err
		}
	}
	return nil
}

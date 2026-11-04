package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/vivekyarra/sway/engine/internal/catalog"
)

// Sim is a deterministic, offline shopping agent used when no Anthropic key is configured.
//
// It genuinely reads the sources: a lexical extractor finds (product, aspect, polarity) claims in
// each sentence, with negation ("never felt any pressure"), comparatives ("better than") and pronoun
// carry-over. Evidence saturates (tanh), so sources interact non-additively, which is exactly what
// makes Shapley attribution meaningful. A tiny hash-seeded jitter per sample mimics LLM sampling noise.
type Sim struct{}

func (Sim) Name() string  { return "sim" }
func (Sim) Model() string { return "sway-sim-v2" }

// Aspect lexicons. Request needs and source evidence share the aspect vocabulary.
// needLexicon maps aspect -> request term -> weight. A need's weight is the max of its matched terms.
var needLexicon = map[string]map[string]float64{
	"anc":         {"noise-cancel": 1, "noise cancel": 1, "anc": 1, "cancelling": 1, "quiet flight": 1},
	"comfort":     {"comfort": 1.5, "long-haul": 1.5, "long haul": 1.5, "long flight": 1.5, "hour": 1.5, "all day": 1, "all-day": 1, "red-eye": 1.5},
	"battery":     {"battery": 1, "long-haul": 0.5, "long haul": 0.5, "long flight": 0.5, "hour": 0.5},
	"portability": {"travel": 1, "flight": 1, "fly": 1, "flying": 1, "commute": 1, "compact": 1, "portable": 1, "trips": 1},
	"weight":      {"light": 1, "lightweight": 1},
	"durability":  {"survive": 1.5, "durable": 1.5, "rough": 1.5, "sturdy": 1, "tough": 1.5},
	"capacity":    {"pack": 1, "space": 1, "capacity": 1, "week": 1},
	"stability":   {"stable": 1.5, "wobble": 1.5, "sturdy": 1, "solid": 1, "full height": 1.5, "tall": 1},
	"height":      {"tall": 1.5, "height": 1},
	"noise":       {"quiet": 1, "silent": 1, "motor": 1},
	"price":       {"cheap": 1, "budget": 0.6, "affordable": 1, "value": 0.8, "inexpensive": 1},
}

var evidenceLexicon = map[string][]string{
	"comfort":     {"comfort", "comfortable", "uncomfortable", "cushion", "pressure", "clamp", "headache", "ache", "sore", "fatigue", "padding"},
	"anc":         {"noise cancelling", "noise-cancelling", "cancellation", "anc", "drone", "isolation"},
	"battery":     {"battery", "runtime", "charge lasts"},
	"portability": {"case", "folds", "fold flat", "compact", "bulky", "under the seat", "overhead bin"},
	"weight":      {"heavy", "lightweight", "to lift"},
	"durability":  {"wheel", "crack", "dent", "zipper", "broke", "held up", "survived", "rolls smoothly", "been solid"},
	"capacity":    {"expands", "fits a surprising amount", "space"},
	"stability":   {"wobble", "rock-solid", "shakes", "stable", "crossbar"},
	"noise":       {"motor", "quiet", "quieter", "quietest", "loud"},
}

var positiveWords = map[string]float64{
	"comfortable": 1, "great": 1, "excellent": 1, "best": 1, "love": 1, "better": 1, "stronger": 1, "superb": 1,
	"solid": 1, "rock-solid": 1.5, "quiet": 1, "quieter": 1, "quietest": 1, "smoothly": 1, "smooth": 1, "fits": 1, "fit": 1,
	"spare": 0.5, "held": 1, "survived": 0.5, "longer": 1, "expands": 1, "stable": 1, "flat": 0.5, "surprising": 0.5,
}

var negativeWords = map[string]float64{
	"hurt": 1, "hurts": 1, "headache": 1.5, "ache": 1, "aches": 1, "clamped": 1, "clamp": 1, "clamping": 1, "pressure": 1,
	"fatigue": 1, "bulky": 1, "wobble": 1, "wobbles": 1.5, "shakes": 1, "cracked": 1.5, "crack": 1, "dented": 1.5,
	"broke": 1.5, "heavy": 1, "loud": 1, "badly": 0.5, "worse": 1, "flimsy": 1, "disappointing": 1, "uncomfortable": 1.5,
}

var negators = map[string]bool{"not": true, "no": true, "never": true, "without": true, "dont": true, "doesnt": true, "isnt": true, "wasnt": true, "didnt": true}

var boosters = map[string]float64{"most": 1.3, "very": 1.2, "badly": 1.2, "extremely": 1.4, "so": 1.15, "far": 1.1}

var typeWeight = map[string]float64{"review": 1, "blog": 1, "transcript": 1, "forum": 0.85}

var sentenceSplit = regexp.MustCompile(`[.!?]+\s+|[.!?]+$`)
var simWord = regexp.MustCompile(`[a-z0-9]+(?:[-'][a-z0-9]+)*`)

// Claim is one piece of evidence a source gives about one product's aspect.
type Claim struct {
	SourceID  string  `json:"source_id"`
	ProductID string  `json:"product_id"`
	Aspect    string  `json:"aspect"`
	Strength  float64 `json:"strength"` // signed
	Sentence  string  `json:"sentence"`
}

// ExtractClaims reads a source and returns its claims about the candidates.
func ExtractClaims(src catalog.Source, cands []catalog.Product) []Claim {
	var claims []Claim
	var carry []string
	tw := typeWeight[src.Type]
	if tw == 0 {
		tw = 1
	}
	for _, sent := range sentenceSplit.Split(src.Body, -1) {
		sent = strings.TrimSpace(sent)
		if sent == "" {
			continue
		}
		low := strings.ToLower(sent)
		mentioned := mentions(low, cands)
		if len(mentioned) == 0 {
			mentioned = carry
		} else {
			carry = mentioned
		}
		if len(mentioned) == 0 {
			continue
		}
		aspects := aspectsIn(low)
		if len(aspects) == 0 {
			continue
		}
		pol := polarity(low)
		if pol == 0 {
			continue
		}
		pol = math.Max(-2, math.Min(2, pol)) * tw
		// Comparatives: "A ... better than B" -> A gets +pol, B gets -pol.
		if len(mentioned) >= 2 && strings.Contains(low, " than ") {
			idx := strings.Index(low, " than ")
			for _, pid := range mentioned {
				pos := mentionPos(low, pid, cands)
				s := pol
				if pos > idx {
					s = -pol
				}
				for _, a := range aspects {
					claims = append(claims, Claim{src.ID, pid, a, s, sent})
				}
			}
			continue
		}
		for _, pid := range mentioned {
			for _, a := range aspects {
				claims = append(claims, Claim{src.ID, pid, a, pol, sent})
			}
		}
	}
	return claims
}

func mentions(low string, cands []catalog.Product) []string {
	var out []string
	for _, p := range cands {
		if mentionPos(low, p.ID, cands) >= 0 {
			out = append(out, p.ID)
		}
	}
	return out
}

func mentionPos(low, pid string, cands []catalog.Product) int {
	for _, p := range cands {
		if p.ID != pid {
			continue
		}
		if i := strings.Index(low, strings.ToLower(p.Name)); i >= 0 {
			return i
		}
		if p.Brand != "" {
			if i := wordIndex(low, strings.ToLower(p.Brand)); i >= 0 {
				return i
			}
		}
	}
	return -1
}

func wordIndex(s, w string) int {
	for start := 0; ; {
		i := strings.Index(s[start:], w)
		if i < 0 {
			return -1
		}
		i += start
		before := i == 0 || !isAlnum(s[i-1])
		after := i+len(w) >= len(s) || !isAlnum(s[i+len(w)])
		if before && after {
			return i
		}
		start = i + 1
	}
}

func isAlnum(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }

func aspectsIn(low string) []string {
	var out []string
	for a, words := range evidenceLexicon {
		for _, w := range words {
			if containsTerm(low, w) {
				out = append(out, a)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func containsTerm(low, term string) bool {
	if strings.Contains(term, " ") || strings.Contains(term, "-") {
		return strings.Contains(low, term)
	}
	// prefix match on word boundaries ("crack" matches "cracked", "wheel" matches "wheels")
	for _, w := range simWord.FindAllString(low, -1) {
		if strings.HasPrefix(w, term) {
			return true
		}
	}
	return false
}

func polarity(low string) float64 {
	words := simWord.FindAllString(strings.ReplaceAll(low, "n't", "nt"), -1)
	total, boost := 0.0, 1.0
	for i, w := range words {
		if b, ok := boosters[w]; ok {
			boost = math.Max(boost, b)
		}
		v := 0.0
		if p, ok := positiveWords[w]; ok {
			v = p
		} else if n, ok := negativeWords[w]; ok {
			v = -n
		}
		if v == 0 {
			continue
		}
		for k := i - 1; k >= 0 && k >= i-3; k-- {
			if negators[words[k]] {
				v = -v
				break
			}
		}
		total += v
	}
	return total * boost
}

// Needs parses what the shopper cares about into aspect weights.
func Needs(request string) map[string]float64 {
	low := strings.ToLower(request)
	needs := map[string]float64{}
	for a, terms := range needLexicon {
		for t, w := range terms {
			if containsTerm(low, t) && w > needs[a] {
				needs[a] = w
			}
		}
	}
	if strings.Contains(low, "cheapest") || strings.Contains(low, "lowest price") {
		needs["price"] = 3
	} else if _, ok := needs["price"]; !ok {
		needs["price"] = 0.4
	}
	return needs
}

// simulation constants (tuned so the seeded flip is decisive, see agent_test.go)
const (
	evidenceGain   = 0.42 // how far strong evidence can move an aspect (on a 0..1 spec scale)
	evidenceScale  = 1.6  // saturation scale for tanh
	overBudgetCost = 25.0
	formMismatch   = 30.0
	jitterRange    = 1.0
)

type scored struct {
	id       string
	score    float64
	evidence map[string]map[string]float64 // aspect -> source -> strength
}

func (s Sim) score(in Input, sample int, key string) ([]scored, map[string]float64, []Claim) {
	needs := Needs(in.Request)
	// Only aspects that some candidate has a spec score for (or price) are meaningful in this category.
	present := map[string]bool{"price": true}
	for _, p := range in.Candidates {
		for a := range p.Scores {
			present[a] = true
		}
	}
	for a := range needs {
		if !present[a] {
			delete(needs, a)
		}
	}
	minP, maxP := int64(math.MaxInt64), int64(0)
	for _, p := range in.Candidates {
		minP, maxP = min(minP, p.PriceCents), max(maxP, p.PriceCents)
	}
	var claims []Claim
	for _, src := range in.Sources {
		claims = append(claims, ExtractClaims(src, in.Candidates)...)
	}
	low := strings.ToLower(in.Request)
	wantForm := ""
	for _, f := range []string{"on-ear", "over-ear", "in-ear", "soft-side", "hard-shell"} {
		if strings.Contains(low, f) {
			wantForm = f
		}
	}
	out := make([]scored, 0, len(in.Candidates))
	for _, p := range in.Candidates {
		sc := scored{id: p.ID, evidence: map[string]map[string]float64{}}
		for _, c := range claims {
			if c.ProductID != p.ID {
				continue
			}
			if sc.evidence[c.Aspect] == nil {
				sc.evidence[c.Aspect] = map[string]float64{}
			}
			sc.evidence[c.Aspect][c.SourceID] += c.Strength
		}
		num, den := 0.0, 0.0
		for a, w := range needs {
			base := 0.5
			if a == "price" {
				if maxP > minP {
					base = 1 - float64(p.PriceCents-minP)/float64(maxP-minP)
				}
			} else if v, ok := p.Scores[a]; ok {
				base = v
			}
			e := 0.0
			for _, v := range sc.evidence[a] {
				e += v
			}
			num += w * (base + evidenceGain*math.Tanh(e/evidenceScale))
			den += w
		}
		score := 50.0
		if den > 0 {
			score = 100 * num / den
		}
		if in.BudgetCents > 0 && p.PriceCents > in.BudgetCents {
			score -= overBudgetCost
		}
		if wantForm != "" {
			if f, _ := p.Specs["form"].(string); f != "" && f != wantForm {
				score -= formMismatch
			}
		}
		score += jitter(key, p.ID)
		sc.score = math.Max(0, math.Min(100, math.Round(score*10)/10))
		out = append(out, sc)
	}
	return out, needs, claims
}

func jitter(key, pid string) float64 {
	h := sha256.Sum256([]byte(key + "|" + pid))
	u := float64(binary.BigEndian.Uint64(h[:8])) / float64(math.MaxUint64)
	return (u*2 - 1) * jitterRange
}

// Decide scores every candidate deterministically for (input, sample).
func (s Sim) Decide(_ context.Context, in Input, sample int) (Output, error) {
	in = in.Normalize()
	key := CacheKey(s.Name(), s.Model(), in, sample)
	scs, needs, _ := s.score(in, sample, key)
	if len(scs) == 0 {
		return Output{}, fmt.Errorf("no candidates")
	}
	out := Output{}
	best := 0
	for i, sc := range scs {
		out.Scores = append(out.Scores, Score{ProductID: sc.id, Score: sc.score})
		if sc.score > scs[best].score || (sc.score == scs[best].score && sc.id < scs[best].id) {
			best = i
		}
	}
	out.Chosen = scs[best].id
	runner := -1
	for i := range scs {
		if i != best && (runner < 0 || scs[i].score > scs[runner].score) {
			runner = i
		}
	}
	out.ReliedOn, out.Rationale = rationale(in, scs, best, runner, needs)
	if err := Validate(in, &out); err != nil {
		return Output{}, err
	}
	return out, nil
}

var aspectPhrase = map[string][2]string{
	"comfort":     {"stays comfortable over long wear", "gets uncomfortable over long wear"},
	"anc":         {"cancels noise better", "cancels less noise"},
	"battery":     {"lasts longer on a charge", "needs charging sooner"},
	"portability": {"packs small enough for under-seat storage", "is bulky to carry"},
	"weight":      {"is light to carry", "is heavy to lift"},
	"durability":  {"holds up to rough handling", "breaks under rough handling"},
	"capacity":    {"fits more than its size suggests", "runs out of space"},
	"stability":   {"stays stable at full height", "wobbles at full height"},
	"noise":       {"runs quietly", "runs loudly"},
	"height":      {"reaches a tall user's height", "falls short for tall users"},
}

func rationale(in Input, scs []scored, best, runner int, needs map[string]float64) ([]string, string) {
	byID := map[string]catalog.Product{}
	for _, p := range in.Candidates {
		byID[p.ID] = p
	}
	srcByID := map[string]catalog.Source{}
	for _, s := range in.Sources {
		srcByID[s.ID] = s
	}
	chosen := byID[scs[best].id]
	type reason struct {
		src, aspect string
		impact      float64
		positive    bool
		about       string
	}
	var reasons []reason
	add := func(sc scored, sign float64) {
		for a, bySrc := range sc.evidence {
			w := needs[a]
			if w == 0 {
				continue
			}
			for sid, v := range bySrc {
				reasons = append(reasons, reason{sid, a, sign * v * w, v > 0, sc.id})
			}
		}
	}
	add(scs[best], 1)
	if runner >= 0 {
		add(scs[runner], -1)
	}
	sort.SliceStable(reasons, func(i, j int) bool {
		if reasons[i].impact != reasons[j].impact {
			return reasons[i].impact > reasons[j].impact
		}
		return reasons[i].src < reasons[j].src
	})
	relied := map[string]bool{}
	// Group the strongest reasons by (source, product) so one source reads as one clause:
	// "Dev Okafor (transcript) reports the Brisane ANC2 cancels noise better and lasts longer on a charge".
	type clause struct {
		src, about string
		phrases    []string
	}
	var clauses []*clause
	index := map[string]*clause{}
	for _, r := range reasons {
		if r.impact <= 0.3 {
			break
		}
		relied[r.src] = true
		p := aspectPhrase[r.aspect]
		phrase := p[0]
		if !r.positive {
			phrase = p[1]
		}
		k := r.src // one clause per source: its mirror-image claim about the runner-up adds nothing
		c, ok := index[k]
		if ok && c.about != r.about {
			continue
		}
		if !ok {
			if len(clauses) >= 2 {
				continue
			}
			c = &clause{src: r.src, about: r.about}
			index[k] = c
			clauses = append(clauses, c)
		}
		dup := false
		for _, x := range c.phrases {
			dup = dup || x == phrase
		}
		if !dup && len(c.phrases) < 2 {
			c.phrases = append(c.phrases, phrase)
		}
	}
	var parts []string
	for _, c := range clauses {
		parts = append(parts, fmt.Sprintf("%s (%s) reports the %s %s", srcByID[c.src].CreatorName, srcByID[c.src].Type, byID[c.about].Name, strings.Join(c.phrases, " and ")))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "I chose the %s at %s because it best fits what you asked for.", chosen.Name, catalog.FormatUSD(chosen.PriceCents))
	if len(parts) > 0 {
		b.WriteString(" " + strings.Join(parts, "; ") + ".")
	} else {
		var top []string
		for a, w := range needs {
			if a != "price" && w >= 1 {
				top = append(top, a)
			}
		}
		sort.Strings(top)
		if len(top) > 0 {
			fmt.Fprintf(&b, " On the spec sheets alone it offers the best balance of %s for the price.", strings.Join(top, ", "))
		} else {
			b.WriteString(" On the spec sheets alone it is the best value for your request.")
		}
	}
	if runner >= 0 {
		r := byID[scs[runner].id]
		fmt.Fprintf(&b, " The %s (%s) was the runner-up.", r.Name, catalog.FormatUSD(r.PriceCents))
	}
	ids := make([]string, 0, len(relied))
	for id := range relied {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, b.String()
}

// SimExplain writes a creator explanation from the extracted claims (no numbers, no invented facts).
func SimExplain(e ExplainInput) string {
	if !e.Positive {
		return "Your content argued for a different product, so it did not push the agent toward this purchase and earns nothing on it. That is how Sway keeps rewards tied to the decision itself."
	}
	if len(e.Aspects) == 0 {
		return fmt.Sprintf("Your content was part of the evidence the agent read before choosing the %s, and removing it made the choice less likely.", e.Purchased)
	}
	var phrases []string
	for _, a := range e.Aspects {
		if p, ok := aspectPhrase[a]; ok {
			phrases = append(phrases, strings.TrimPrefix(p[0], "is "))
		}
	}
	return fmt.Sprintf("Your content was the evidence that the %s %s, which is what the shopper asked for. %s",
		e.Purchased, strings.Join(phrases, " and "), e.RemovalEffect)
}

var digits = regexp.MustCompile(`\$?\d[\d,.]*%?`)

// StripNumbers removes any number a model may have written; code adds the real figures.
func StripNumbers(s string) string {
	s = digits.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

// Package shop is the shopper-facing agent flow: retrieve, decide, explain influence, re-run without a source.
package shop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/db"
)

type Service struct {
	DB         *db.DB
	Agent      *agent.Agent
	Categories map[string]catalog.CategoryDef
	Samples    int
	MaxSources int
	Live       LiveCatalog // optional (Channel3); nil in curated mode
}

// LiveCatalog imports real products for a request (Channel3 adapter).
type LiveCatalog interface {
	Search(ctx context.Context, q db.Q, request string, budget int64) (category string, err error)
}

// ErrUnsupported means the request matched no catalog category.
var ErrUnsupported = errors.New("unsupported request")

// UnsupportedError carries suggestions back to the shopper.
type UnsupportedError struct{ Suggestions []string }

func (e *UnsupportedError) Error() string {
	return "I can shop " + strings.Join(e.Suggestions, ", ") + " in the Sway catalog."
}
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// SourceView is a source as the shopper sees it.
type SourceView struct {
	ID          string        `json:"id"`
	CreatorID   string        `json:"creator_id"`
	CreatorName string        `json:"creator_name"`
	Type        string        `json:"type"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	Status      string        `json:"status"`
	Reason      string        `json:"reason,omitempty"`
	ProductIDs  []string      `json:"product_ids"`
	Claims      []agent.Claim `json:"claims,omitempty"`
}

// DecisionView is the full decision as returned by the API.
type DecisionView struct {
	ID            string             `json:"id"`
	SessionID     string             `json:"session_id"`
	Request       string             `json:"request"`
	BudgetCents   int64              `json:"budget_cents"`
	Category      string             `json:"category"`
	Candidates    []catalog.Product  `json:"candidates"`
	Sources       []SourceView       `json:"sources"`
	Ignored       []SourceView       `json:"ignored_sources"`
	Chosen        catalog.Product    `json:"chosen"`
	Rationale     string             `json:"rationale"`
	ReliedOn      []string           `json:"relied_on_source_ids"`
	Probabilities map[string]float64 `json:"probabilities"`
	Engine        string             `json:"engine"`
	Model         string             `json:"model"`
	PromptVersion string             `json:"prompt_version"`
	Status        string             `json:"status"`
	CreatedAt     time.Time          `json:"created_at"`
	Influence     *agent.LeaveOneOut `json:"influence,omitempty"`
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// Decide runs retrieval and the full-source decision for a shopping request.
func (s *Service) Decide(ctx context.Context, text string) (DecisionView, error) {
	text = strings.TrimSpace(text)
	if len(text) < 3 {
		return DecisionView{}, &UnsupportedError{Suggestions: s.suggestions()}
	}
	if len(text) > 500 {
		text = text[:500]
	}
	budget := catalog.ParseBudget(text)
	category := catalog.DetectCategory(text, s.Categories)
	if category == "" && s.Live != nil {
		c, err := s.Live.Search(ctx, s.DB.Pool, text, budget)
		if err != nil {
			return DecisionView{}, fmt.Errorf("live catalog: %w", err)
		}
		category = c
	}
	if category == "" {
		return DecisionView{}, &UnsupportedError{Suggestions: s.suggestions()}
	}
	products, err := catalog.ProductsByCategory(ctx, s.DB.Pool, category)
	if err != nil {
		return DecisionView{}, err
	}
	if len(products) == 0 {
		return DecisionView{}, &UnsupportedError{Suggestions: s.suggestions()}
	}
	cands := catalog.SelectCandidates(products, budget, 4)
	ids := make([]string, len(cands))
	for i, p := range cands {
		ids[i] = p.ID
	}
	linked, err := catalog.SourcesForProducts(ctx, s.DB.Pool, ids)
	if err != nil {
		return DecisionView{}, err
	}
	now := time.Now()
	sources := catalog.RankSources(text, linked, now, s.MaxSources)
	in := agent.Input{Request: text, BudgetCents: budget, Candidates: cands, Sources: sources}.Normalize()

	val, err := s.Agent.Evaluate(ctx, in, in.SourceIDs(), "", s.Samples, nil)
	if err != nil {
		return DecisionView{}, fmt.Errorf("agent: %w", err)
	}
	chosen := agent.Majority(val.Picks)
	rationale, relied := val.First.Rationale, val.First.ReliedOn
	for k := 0; k < s.Samples; k++ {
		if r, err := s.Agent.Run(ctx, in, k); err == nil && r.Chosen == chosen {
			rationale, relied = r.Rationale, r.ReliedOn
			break
		}
	}
	dv := DecisionView{
		ID: newID("dec_"), SessionID: newID("ses_"), Request: text, BudgetCents: budget, Category: category,
		Candidates: cands, Rationale: rationale, ReliedOn: relied, Probabilities: roundProbs(val.Probs),
		Engine: s.Agent.Engine.Name(), Model: s.Agent.Engine.Model(), PromptVersion: agent.PromptVersion, Status: "PROPOSED", CreatedAt: now.UTC(),
	}
	for _, p := range cands {
		if p.ID == chosen {
			dv.Chosen = p
		}
	}
	err = s.DB.Tx(ctx, func(q db.Q) error {
		var budgetArg any
		if budget > 0 {
			budgetArg = budget
		}
		if _, err := q.Exec(ctx, `INSERT INTO shopping_sessions (id, request_text, budget_cents, category) VALUES ($1,$2,$3,$4)`, dv.SessionID, text, budgetArg, category); err != nil {
			return err
		}
		candJSON, _ := json.Marshal(ids)
		srcJSON, _ := json.Marshal(in.SourceIDs())
		probs, _ := json.Marshal(dv.Probabilities)
		relJSON, _ := json.Marshal(relied)
		_, err := q.Exec(ctx, `INSERT INTO decisions (id, session_id, request_text, candidate_ids, source_ids, chosen_product_id, probabilities, rationale, relied_on_ids, model, prompt_version, engine, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			dv.ID, dv.SessionID, text, candJSON, srcJSON, chosen, probs, rationale, relJSON, dv.Model, dv.PromptVersion, dv.Engine, dv.CreatedAt)
		return err
	})
	if err != nil {
		return DecisionView{}, err
	}
	return s.Get(ctx, dv.ID)
}

func (s *Service) suggestions() []string {
	var out []string
	for _, c := range s.Categories {
		out = append(out, strings.ToLower(c.Label))
	}
	sort.Strings(out)
	return out
}

func roundProbs(p map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range p {
		out[k] = float64(int(v*10000+0.5)) / 10000
	}
	return out
}

// Load rebuilds the agent input for a stored decision (identical bytes, so every counterfactual hits the same cache).
func (s *Service) Load(ctx context.Context, q db.Q, decisionID string) (agent.Input, DecisionView, error) {
	var dv DecisionView
	var candJSON, srcJSON, probs, relied, influence []byte
	var budget *int64
	err := q.QueryRow(ctx, `SELECT d.id, d.session_id, d.request_text, COALESCE(ss.budget_cents, NULL), ss.category, d.candidate_ids, d.source_ids,
		d.chosen_product_id, d.probabilities, d.rationale, d.relied_on_ids, d.engine, d.model, d.prompt_version, d.status, d.created_at, d.influence
		FROM decisions d JOIN shopping_sessions ss ON ss.id=d.session_id WHERE d.id=$1`, decisionID).
		Scan(&dv.ID, &dv.SessionID, &dv.Request, &budget, &dv.Category, &candJSON, &srcJSON, &dv.Chosen.ID, &probs, &dv.Rationale, &relied,
			&dv.Engine, &dv.Model, &dv.PromptVersion, &dv.Status, &dv.CreatedAt, &influence)
	if err != nil {
		return agent.Input{}, dv, fmt.Errorf("decision %s: %w", decisionID, err)
	}
	if budget != nil {
		dv.BudgetCents = *budget
	}
	var candIDs, srcIDs []string
	_ = json.Unmarshal(candJSON, &candIDs)
	_ = json.Unmarshal(srcJSON, &srcIDs)
	_ = json.Unmarshal(probs, &dv.Probabilities)
	_ = json.Unmarshal(relied, &dv.ReliedOn)
	if len(influence) > 0 {
		var loo agent.LeaveOneOut
		if json.Unmarshal(influence, &loo) == nil {
			dv.Influence = &loo
		}
	}
	cands, err := catalog.ProductsByIDs(ctx, q, candIDs)
	if err != nil {
		return agent.Input{}, dv, err
	}
	srcs, err := catalog.SourcesByIDs(ctx, q, srcIDs)
	if err != nil {
		return agent.Input{}, dv, err
	}
	dv.Candidates = cands
	for _, p := range cands {
		if p.ID == dv.Chosen.ID {
			dv.Chosen = p
		}
	}
	in := agent.Input{Request: dv.Request, BudgetCents: dv.BudgetCents, Candidates: cands, Sources: srcs}.Normalize()
	for _, src := range srcs {
		dv.Sources = append(dv.Sources, view(src, agent.ExtractClaims(src, cands)))
	}
	// Sources linked to these products that screening excluded: shown with their reason, never given to the agent.
	linked, err := catalog.SourcesForProducts(ctx, q, candIDs)
	if err != nil {
		return agent.Input{}, dv, err
	}
	for _, src := range linked {
		if src.Status != "active" {
			dv.Ignored = append(dv.Ignored, view(src, nil))
		}
	}
	return in, dv, nil
}

func view(src catalog.Source, claims []agent.Claim) SourceView {
	return SourceView{ID: src.ID, CreatorID: src.CreatorID, CreatorName: src.CreatorName, Type: src.Type, Title: src.Title, Body: src.Body,
		PublishedAt: src.PublishedAt.Format("2006-01-02"), Status: src.Status, Reason: src.StatusReason, ProductIDs: src.ProductIDs, Claims: claims}
}

// Get returns a stored decision.
func (s *Service) Get(ctx context.Context, decisionID string) (DecisionView, error) {
	_, dv, err := s.Load(ctx, s.DB.Pool, decisionID)
	return dv, err
}

// RerunView is the answer to "Remove and re-run".
type RerunView struct {
	DecisionID    string             `json:"decision_id"`
	Excluded      []string           `json:"excluded_source_ids"`
	Chosen        catalog.Product    `json:"chosen"`
	Rationale     string             `json:"rationale"`
	Probabilities map[string]float64 `json:"probabilities"`
	Flipped       bool               `json:"flipped"`
	Agreement     string             `json:"agreement"` // e.g. "3/3 samples"
}

// Rerun re-decides with some sources removed.
func (s *Service) Rerun(ctx context.Context, decisionID string, exclude []string) (RerunView, error) {
	in, dv, err := s.Load(ctx, s.DB.Pool, decisionID)
	if err != nil {
		return RerunView{}, err
	}
	ex := map[string]bool{}
	for _, id := range exclude {
		ex[id] = true
	}
	var keep, excluded []string
	for _, id := range in.SourceIDs() {
		if ex[id] {
			excluded = append(excluded, id)
		} else {
			keep = append(keep, id)
		}
	}
	val, err := s.Agent.Evaluate(ctx, in, keep, dv.Chosen.ID, s.Samples, nil)
	if err != nil {
		return RerunView{}, err
	}
	pick := agent.Majority(val.Picks)
	rv := RerunView{DecisionID: decisionID, Excluded: excluded, Probabilities: roundProbs(val.Probs), Flipped: pick != dv.Chosen.ID,
		Agreement: fmt.Sprintf("%d/%d samples", val.Picks[pick], s.Samples)}
	for _, p := range in.Candidates {
		if p.ID == pick {
			rv.Chosen = p
		}
	}
	sub := in.WithSources(keep)
	for k := 0; k < s.Samples; k++ {
		if r, err := s.Agent.Run(ctx, sub, k); err == nil && r.Chosen == pick {
			rv.Rationale = r.Rationale
			break
		}
	}
	return rv, nil
}

// Influence computes (and stores) leave-one-out quick influence for a decision.
func (s *Service) Influence(ctx context.Context, decisionID string) (agent.LeaveOneOut, error) {
	in, dv, err := s.Load(ctx, s.DB.Pool, decisionID)
	if err != nil {
		return agent.LeaveOneOut{}, err
	}
	if dv.Influence != nil {
		return *dv.Influence, nil
	}
	loo, err := s.Agent.QuickInfluence(ctx, in, dv.Chosen.ID, s.Samples)
	if err != nil {
		return loo, err
	}
	b, _ := json.Marshal(loo)
	_, err = s.DB.Pool.Exec(ctx, `UPDATE decisions SET influence=$2 WHERE id=$1`, decisionID, b)
	return loo, err
}

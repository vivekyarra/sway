// Package catalog holds products, creators and sources, and the retrieval step that turns a
// shopping request into a candidate set plus the human-written evidence the agent will read.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/db"
)

type Product struct {
	ID         string             `json:"id"`
	MerchantID string             `json:"merchant_id"`
	Name       string             `json:"name"`
	Brand      string             `json:"brand"`
	Category   string             `json:"category"`
	PriceCents int64              `json:"price_cents"`
	Tagline    string             `json:"tagline"`
	ImageURL   string             `json:"image_url"`
	Specs      map[string]any     `json:"specs"`
	Scores     map[string]float64 `json:"scores,omitempty"` // normalized spec-sheet fit per aspect (local engine only)
	Source     string             `json:"source"`
}

type Creator struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	PayPalEmail string `json:"paypal_email"`
	Verified    bool   `json:"verified"`
	Bio         string `json:"bio"`
	AvatarHue   int    `json:"avatar_hue"`
}

type Source struct {
	ID            string    `json:"id"`
	CreatorID     string    `json:"creator_id"`
	CreatorName   string    `json:"creator_name"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	PublishedAt   time.Time `json:"published_at"`
	Status        string    `json:"status"`
	StatusReason  string    `json:"status_reason"`
	DuplicateOfID string    `json:"duplicate_of_id,omitempty"`
	ProductIDs    []string  `json:"product_ids"`
}

// Request is a parsed shopping request.
type Request struct {
	Text        string `json:"text"`
	Category    string `json:"category"`
	BudgetCents int64  `json:"budget_cents"` // 0 = no budget stated
}

var budgetRe = regexp.MustCompile(`(?i)(?:under|below|less than|max(?:imum)?|up to|budget(?: of)?|<)\s*\$?\s*(\d{2,6})(?:\.\d{1,2})?|\$\s*(\d{2,6})(?:\.\d{1,2})?`)

// ParseBudget extracts a dollar budget ("under $250", "$250", "budget of 300").
func ParseBudget(text string) int64 {
	m := budgetRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * 100
}

// CategoryDef comes from the historical catalog definition.
type CategoryDef struct {
	Label           string   `json:"label"`
	Keywords        []string `json:"keywords"`
	SuggestedPrompt string   `json:"suggested_prompt"`
}

// DetectCategory picks the category whose keywords best match the request (longest keyword wins ties).
func DetectCategory(text string, cats map[string]CategoryDef) string {
	t := " " + strings.ToLower(text) + " "
	best, bestScore := "", 0
	keys := make([]string, 0, len(cats))
	for k := range cats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		score := 0
		for _, kw := range cats[k].Keywords {
			if strings.Contains(t, strings.ToLower(kw)) {
				score += len(kw)
			}
		}
		if score > bestScore {
			best, bestScore = k, score
		}
	}
	return best
}

var ErrNoCategory = errors.New("catalog: no matching category")

// ProductsByCategory returns products of one category ordered by id.
func ProductsByCategory(ctx context.Context, q db.Q, category string) ([]Product, error) {
	return queryProducts(ctx, q, `WHERE category=$1 ORDER BY id`, category)
}

// ProductsByIDs returns the products in id order.
func ProductsByIDs(ctx context.Context, q db.Q, ids []string) ([]Product, error) {
	return queryProducts(ctx, q, `WHERE id = ANY($1) ORDER BY id`, ids)
}

func GetProduct(ctx context.Context, q db.Q, id string) (Product, error) {
	ps, err := queryProducts(ctx, q, `WHERE id=$1`, id)
	if err != nil {
		return Product{}, err
	}
	if len(ps) == 0 {
		return Product{}, fmt.Errorf("product %s not found", id)
	}
	return ps[0], nil
}

func queryProducts(ctx context.Context, q db.Q, where string, args ...any) ([]Product, error) {
	rows, err := q.Query(ctx, `SELECT id, merchant_id, name, brand, category, price_cents, tagline, image_url, specs, source FROM products `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Product
	for rows.Next() {
		var p Product
		var specs []byte
		if err := rows.Scan(&p.ID, &p.MerchantID, &p.Name, &p.Brand, &p.Category, &p.PriceCents, &p.Tagline, &p.ImageURL, &specs, &p.Source); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(specs, &p.Specs); err != nil {
			return nil, err
		}
		if s, ok := p.Specs["_scores"].(map[string]any); ok {
			p.Scores = map[string]float64{}
			for k, v := range s {
				if f, ok := v.(float64); ok {
					p.Scores[k] = f
				}
			}
			delete(p.Specs, "_scores")
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SourcesForProducts returns sources linked to any of the products, with their creator name and links.
func SourcesForProducts(ctx context.Context, q db.Q, productIDs []string) ([]Source, error) {
	return querySources(ctx, q, `WHERE s.id IN (SELECT source_id FROM source_products WHERE product_id = ANY($1))`, productIDs)
}

// SourcesByIDs returns sources in id order.
func SourcesByIDs(ctx context.Context, q db.Q, ids []string) ([]Source, error) {
	return querySources(ctx, q, `WHERE s.id = ANY($1)`, ids)
}

// AllSources returns every source (for screening and dashboards).
func AllSources(ctx context.Context, q db.Q) ([]Source, error) {
	return querySources(ctx, q, ``)
}

func querySources(ctx context.Context, q db.Q, where string, args ...any) ([]Source, error) {
	rows, err := q.Query(ctx, `SELECT s.id, s.creator_id, c.display_name, s.type, s.title, s.body, s.published_at, s.status, s.status_reason,
		COALESCE(s.duplicate_of_id,''), COALESCE((SELECT array_agg(product_id ORDER BY product_id) FROM source_products sp WHERE sp.source_id=s.id), '{}')
		FROM sources s JOIN creators c ON c.id=s.creator_id `+where+` ORDER BY s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.CreatorID, &s.CreatorName, &s.Type, &s.Title, &s.Body, &s.PublishedAt, &s.Status, &s.StatusReason, &s.DuplicateOfID, &s.ProductIDs); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

var wordRe = regexp.MustCompile(`[a-z0-9]+`)

var stop = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "for": true, "of": true, "to": true, "in": true, "on": true,
	"i": true, "my": true, "me": true, "is": true, "it": true, "with": true, "that": true, "this": true, "need": true, "want": true, "under": true, "at": true}

// Tokens lowercases and splits text into content words.
func Tokens(text string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		if !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

// RankSources keeps eligible sources (active, published before cutoff) ranked by keyword overlap with the
// request plus a small recency bonus, then returns the top n sorted by id (the agent always sees a fixed order).
func RankSources(req string, sources []Source, cutoff time.Time, n int) []Source {
	reqWords := map[string]bool{}
	for _, w := range Tokens(req) {
		reqWords[w] = true
	}
	type scored struct {
		s     Source
		score float64
	}
	var cands []scored
	for _, s := range sources {
		if s.Status != "active" || !s.PublishedAt.Before(cutoff) {
			continue
		}
		overlap := 0
		seen := map[string]bool{}
		for _, w := range Tokens(s.Title + " " + s.Body) {
			if reqWords[w] && !seen[w] {
				overlap++
				seen[w] = true
			}
		}
		ageDays := cutoff.Sub(s.PublishedAt).Hours() / 24
		recency := math.Exp(-ageDays / 365)
		cands = append(cands, scored{s, float64(overlap) + 0.5*recency})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].s.ID < cands[j].s.ID
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	out := make([]Source, len(cands))
	for i, c := range cands {
		out[i] = c.s
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SelectCandidates keeps products within budget first, then at most one over-budget "stretch" option,
// capped at max, sorted by id.
func SelectCandidates(products []Product, budget int64, max int) []Product {
	var in, over []Product
	for _, p := range products {
		if budget == 0 || p.PriceCents <= budget {
			in = append(in, p)
		} else {
			over = append(over, p)
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].PriceCents < in[j].PriceCents })
	sort.Slice(over, func(i, j int) bool { return over[i].PriceCents < over[j].PriceCents })
	out := in
	if len(out) > max {
		out = out[:max]
	}
	if len(out) < max && len(over) > 0 {
		out = append(out, over[0])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FormatUSD renders cents as "$229" or "$229.50".
func FormatUSD(c int64) string {
	if c%100 == 0 {
		return "$" + strconv.FormatInt(c/100, 10)
	}
	return fmt.Sprintf("$%d.%02d", c/100, c%100)
}

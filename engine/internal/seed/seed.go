// Package seed loads Sway's curated marketplace: one merchant, its products, the creators who review
// them and the sources they wrote. All brands, people and texts are original to Sway.
package seed

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/screening"
)

//go:embed data/catalog.json data/sources/*.md
var dataFS embed.FS

type ProductDef struct {
	catalog.Product
	Scores map[string]float64 `json:"scores"`
}

type MerchantDef struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	PayPalMerchantEmail string `json:"paypal_merchant_email"`
	CommissionRateBps   int    `json:"commission_rate_bps"`
}

type SourceDef struct {
	ID         string
	CreatorID  string
	Type       string
	Title      string
	Published  time.Time
	ProductIDs []string
	Body       string
}

type World struct {
	Merchant   MerchantDef                    `json:"merchant"`
	Categories map[string]catalog.CategoryDef `json:"categories"`
	Products   []ProductDef                   `json:"products"`
	Creators   []catalog.Creator              `json:"creators"`
	Sources    []SourceDef                    `json:"-"`
}

// Load parses the embedded catalog and source documents.
func Load() (*World, error) {
	raw, err := dataFS.ReadFile("data/catalog.json")
	if err != nil {
		return nil, err
	}
	var w World
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("seed: catalog.json: %w", err)
	}
	entries, err := fs.ReadDir(dataFS, "data/sources")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := dataFS.ReadFile("data/sources/" + e.Name())
		if err != nil {
			return nil, err
		}
		s, err := ParseSource(string(b))
		if err != nil {
			return nil, fmt.Errorf("seed: %s: %w", e.Name(), err)
		}
		w.Sources = append(w.Sources, s)
	}
	sort.Slice(w.Sources, func(i, j int) bool { return w.Sources[i].ID < w.Sources[j].ID })
	return &w, w.validate()
}

func (w *World) validate() error {
	products, creators := map[string]bool{}, map[string]bool{}
	for _, p := range w.Products {
		if _, ok := w.Categories[p.Category]; !ok {
			return fmt.Errorf("seed: product %s has unknown category %s", p.ID, p.Category)
		}
		products[p.ID] = true
	}
	for _, c := range w.Creators {
		creators[c.ID] = true
	}
	for _, s := range w.Sources {
		if !creators[s.CreatorID] {
			return fmt.Errorf("seed: source %s has unknown creator %s", s.ID, s.CreatorID)
		}
		for _, p := range s.ProductIDs {
			if !products[p] {
				return fmt.Errorf("seed: source %s links unknown product %s", s.ID, p)
			}
		}
	}
	return nil
}

// ParseSource reads a markdown document with a simple "key: value" front matter block.
func ParseSource(doc string) (SourceDef, error) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	if !strings.HasPrefix(doc, "---\n") {
		return SourceDef{}, fmt.Errorf("missing front matter")
	}
	head, body, ok := strings.Cut(doc[4:], "\n---\n")
	if !ok {
		return SourceDef{}, fmt.Errorf("unterminated front matter")
	}
	var s SourceDef
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "id":
			s.ID = v
		case "creator":
			s.CreatorID = v
		case "type":
			s.Type = v
		case "title":
			s.Title = v
		case "published":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return s, fmt.Errorf("bad published date: %w", err)
			}
			s.Published = t
		case "products":
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					s.ProductIDs = append(s.ProductIDs, p)
				}
			}
		}
	}
	s.Body = strings.TrimSpace(body)
	if s.ID == "" || s.CreatorID == "" || s.Type == "" || s.Body == "" || s.Published.IsZero() {
		return s, fmt.Errorf("incomplete source %q", s.ID)
	}
	return s, nil
}

// Apply upserts the world into the database. Re-running it refreshes catalog content without touching
// orders or the ledger. Sources start "active"; screening runs afterwards and may block them.
func Apply(ctx context.Context, q db.Q, w *World, vestingWindowSeconds int) error {
	m := w.Merchant
	if _, err := q.Exec(ctx, `INSERT INTO merchants (id,name,paypal_merchant_email,commission_rate_bps,vesting_window_seconds)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, paypal_merchant_email=EXCLUDED.paypal_merchant_email`,
		m.ID, m.Name, m.PayPalMerchantEmail, m.CommissionRateBps, vestingWindowSeconds); err != nil {
		return err
	}
	for _, p := range w.Products {
		specs := map[string]any{}
		for k, v := range p.Specs {
			specs[k] = v
		}
		specs["_scores"] = p.Scores
		specJSON, _ := json.Marshal(specs)
		if _, err := q.Exec(ctx, `INSERT INTO products (id,merchant_id,name,brand,category,price_cents,specs,tagline,image_url,source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'curated') ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, brand=EXCLUDED.brand,
			category=EXCLUDED.category, price_cents=EXCLUDED.price_cents, specs=EXCLUDED.specs, tagline=EXCLUDED.tagline`,
			p.ID, m.ID, p.Name, p.Brand, p.Category, p.PriceCents, specJSON, p.Tagline, "/products/"+p.ID+".svg"); err != nil {
			return err
		}
	}
	for _, c := range w.Creators {
		if _, err := q.Exec(ctx, `INSERT INTO creators (id,display_name,paypal_email,bio,avatar_hue) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (id) DO UPDATE SET display_name=EXCLUDED.display_name, bio=EXCLUDED.bio, avatar_hue=EXCLUDED.avatar_hue`,
			c.ID, c.DisplayName, c.PayPalEmail, c.Bio, c.AvatarHue); err != nil {
			return err
		}
	}
	for _, s := range w.Sources {
		if _, err := q.Exec(ctx, `INSERT INTO sources (id,creator_id,type,title,body,published_at) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (id) DO UPDATE SET title=EXCLUDED.title, body=EXCLUDED.body, published_at=EXCLUDED.published_at, type=EXCLUDED.type`,
			s.ID, s.CreatorID, s.Type, s.Title, s.Body, s.Published); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `DELETE FROM source_products WHERE source_id=$1`, s.ID); err != nil {
			return err
		}
		for _, p := range s.ProductIDs {
			if _, err := q.Exec(ctx, `INSERT INTO source_products (source_id, product_id) VALUES ($1,$2)`, s.ID, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// CatalogProducts returns the curated products as catalog values (with spec-sheet scores).
func (w *World) CatalogProducts() []catalog.Product {
	out := make([]catalog.Product, len(w.Products))
	for i, p := range w.Products {
		cp := p.Product
		cp.Scores = p.Scores
		cp.MerchantID = w.Merchant.ID
		cp.Source = "curated"
		out[i] = cp
	}
	return out
}

// CatalogSources returns the historical sources with creator names resolved (all active, unscreened).
func (w *World) CatalogSources() []catalog.Source {
	names := map[string]string{}
	for _, c := range w.Creators {
		names[c.ID] = c.DisplayName
	}
	out := make([]catalog.Source, len(w.Sources))
	for i, s := range w.Sources {
		out[i] = catalog.Source{ID: s.ID, CreatorID: s.CreatorID, CreatorName: names[s.CreatorID], Type: s.Type, Title: s.Title,
			Body: s.Body, PublishedAt: s.Published, Status: "active", ProductIDs: s.ProductIDs}
	}
	return out
}

// Install loads, applies and screens the curated catalog in one go.
func Install(ctx context.Context, d *db.DB, vestingWindowSeconds int, cls screening.Classifier) (*World, error) {
	w, err := Load()
	if err != nil {
		return nil, err
	}
	if err := d.Tx(ctx, func(q db.Q) error { return Apply(ctx, q, w, vestingWindowSeconds) }); err != nil {
		return nil, fmt.Errorf("seed apply: %w", err)
	}
	srcs, err := catalog.AllSources(ctx, d.Pool)
	if err != nil {
		return nil, err
	}
	verdicts, err := screening.Screen(ctx, srcs, cls)
	if err != nil {
		return nil, err
	}
	if err := d.Tx(ctx, func(q db.Q) error { return screening.Apply(ctx, q, verdicts) }); err != nil {
		return nil, err
	}
	return w, nil
}

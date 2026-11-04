package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/db"
)

// Channel3 imports real products for a shopping request (live catalog mode).
//
// Results are upserted as products in a request-specific category so the same agent, attribution and
// payment pipeline runs on them unchanged. Live products carry real brand names; Sway's own curated catalog
// uses original brands.
type Channel3 struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

const Channel3MerchantID = "m_channel3"

type c3Product struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	KeyFeatures []string `json:"key_features"`
	Brands      []struct {
		Name string `json:"name"`
	} `json:"brands"`
	Images []struct {
		URL  string `json:"url"`
		Main bool   `json:"is_main_image"`
	} `json:"images"`
	Offers []struct {
		URL          string `json:"url"`
		Domain       string `json:"domain"`
		Availability string `json:"availability"`
		Price        struct {
			Price    float64 `json:"price"`
			Currency string  `json:"currency"`
		} `json:"price"`
	} `json:"offers"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Search queries Channel3, stores up to four in-stock USD products and returns their category key.
func (c *Channel3) Search(ctx context.Context, q db.Q, request string, budget int64) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("CHANNEL3_API_KEY is not set")
	}
	body := map[string]any{"query": request, "limit": 12, "filters": map[string]any{"availability": []string{"InStock"}}}
	if budget > 0 {
		body["filters"].(map[string]any)["price"] = map[string]any{"max_price": math.Ceil(float64(budget) * 1.15 / 100)}
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/search", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel3: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("channel3: status %d: %s", resp.StatusCode, truncateStr(string(data), 200))
	}
	var out struct {
		Products []c3Product `json:"products"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("channel3: bad response: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(request))))
	category := "live_" + hex.EncodeToString(sum[:5])
	if _, err := q.Exec(ctx, `INSERT INTO merchants (id, name, commission_rate_bps, vesting_window_seconds) VALUES ($1,'Channel3 live catalog',800,120) ON CONFLICT (id) DO NOTHING`,
		Channel3MerchantID); err != nil {
		return "", err
	}
	stored := 0
	for _, p := range out.Products {
		if stored == 4 {
			break
		}
		price, offerURL, domain := int64(0), "", ""
		for _, o := range p.Offers {
			if o.Availability == "InStock" && strings.EqualFold(o.Price.Currency, "USD") && o.Price.Price > 0 {
				price, offerURL, domain = int64(math.Round(o.Price.Price*100)), o.URL, o.Domain
				break
			}
		}
		if price == 0 || p.ID == "" || p.Title == "" {
			continue
		}
		brand := ""
		if len(p.Brands) > 0 {
			brand = p.Brands[0].Name
		}
		image := ""
		for _, im := range p.Images {
			if image == "" || im.Main {
				image = im.URL
			}
		}
		specs := map[string]any{"merchant": domain, "offer_url": offerURL}
		for i, f := range p.KeyFeatures {
			if i == 4 {
				break
			}
			specs[fmt.Sprintf("feature_%d", i+1)] = truncateStr(f, 120)
		}
		specJSON, _ := json.Marshal(specs)
		tagline := ""
		if p.Description != nil {
			tagline = truncateStr(*p.Description, 90)
		}
		id := "p_c3_" + slugRe.ReplaceAllString(strings.ToLower(p.ID), "_")
		if _, err := q.Exec(ctx, `INSERT INTO products (id, merchant_id, name, brand, category, price_cents, specs, tagline, image_url, source, external_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'channel3',$10)
			ON CONFLICT (id) DO UPDATE SET price_cents=EXCLUDED.price_cents, specs=EXCLUDED.specs, category=EXCLUDED.category, image_url=EXCLUDED.image_url`,
			id, Channel3MerchantID, truncateStr(p.Title, 120), brand, category, price, specJSON, tagline, image, p.ID); err != nil {
			return "", err
		}
		stored++
	}
	if stored == 0 {
		return "", nil
	}
	return category, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

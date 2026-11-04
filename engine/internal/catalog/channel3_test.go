package catalog_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/testdb"
)

func TestChannel3SearchImportsInStockUSDProducts(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"products":[
		 {"id":"A1","title":"Trail Runner","brands":[{"id":"b","name":"Acme"}],"images":[{"url":"https://x/a.jpg","is_main_image":true}],"key_features":["Light"],
		  "offers":[{"url":"https://shop/a","domain":"shop.example","availability":"InStock","price":{"price":119.99,"currency":"USD"}}]},
		 {"id":"B2","title":"Sold out shoe","brands":[],"offers":[{"url":"u","domain":"d","availability":"OutOfStock","price":{"price":50,"currency":"USD"}}]},
		 {"id":"C3","title":"Euro shoe","brands":[],"offers":[{"url":"u","domain":"d","availability":"InStock","price":{"price":50,"currency":"EUR"}}]}
		],"next_page_token":null}`)
	}))
	defer srv.Close()
	d := testdb.New(t)
	c := &catalog.Channel3{APIKey: "k", BaseURL: srv.URL}
	cat, err := c.Search(context.Background(), d.Pool, "trail running shoes under $150", 15000)
	if err != nil || cat == "" {
		t.Fatalf("search: %q %v", cat, err)
	}
	ps, err := catalog.ProductsByCategory(context.Background(), d.Pool, cat)
	if err != nil || len(ps) != 1 {
		t.Fatalf("expected only the in-stock USD product, got %+v %v", ps, err)
	}
	if ps[0].PriceCents != 11999 || ps[0].Brand != "Acme" || ps[0].Source != "channel3" {
		t.Fatalf("product %+v", ps[0])
	}
	filters := got["filters"].(map[string]any)
	if filters["price"].(map[string]any)["max_price"].(float64) != 173 {
		t.Fatalf("budget filter %v", filters)
	}
	bad := &catalog.Channel3{APIKey: "wrong", BaseURL: srv.URL}
	if _, err := bad.Search(context.Background(), d.Pool, "x", 0); err == nil {
		t.Fatal("expected auth error")
	}
}

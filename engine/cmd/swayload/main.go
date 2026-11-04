// Command swayload drives realistic concurrent traffic through a running Sway and checks the books after.
//
// Each virtual shopper loops: ask the agent, run a counterfactual, create a PayPal order, capture it, and
// sometimes refund it (fully or partly). When the run ends it fast-forwards vesting, waits for every
// attribution, runs payouts, then asserts the ledger hash chain verifies, the trial balance is zero and every
// order balances to the cent. It exits non-zero on any server error or broken invariant.
//
//	go run ./cmd/swayload -url http://localhost:8080 -shoppers 32 -orders 400
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var requests = []string{
	"I need noise-cancelling headphones for long-haul flights, under $250.",
	"A carry-on suitcase for weekly business trips that survives rough handling, under $220.",
	"A standing desk that stays stable at full height for a tall person, under $450.",
	"The cheapest on-ear headphones you have.",
	"Comfortable ANC headphones for 12 hour flights, max $250",
	"Durable carry-on luggage for rough baggage handling, budget $220",
}

type stats struct {
	mu        sync.Mutex
	latencies map[string][]time.Duration
	errors    map[string]int
	limited   atomic.Int64
}

func (s *stats) add(ep string, d time.Duration, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies[ep] = append(s.latencies[ep], d)
	if status >= 500 || status == 0 {
		s.errors[ep]++
	}
}

var client = &http.Client{Timeout: 2 * time.Minute}

func call(base string, st *stats, ep, method, path string, body any, out any) int {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest(method, base+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Idempotency-Key", fmt.Sprintf("load-%d-%d", time.Now().UnixNano(), rand.Int64()))
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			st.add(ep, time.Since(start), 0)
			return 0
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			st.limited.Add(1)
			time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
			if body != nil {
				b, _ := json.Marshal(body)
				rdr = bytes.NewReader(b)
			}
			continue
		}
		st.add(ep, time.Since(start), resp.StatusCode)
		if out != nil && resp.StatusCode < 300 {
			_ = json.Unmarshal(raw, out)
		}
		return resp.StatusCode
	}
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func main() {
	base := flag.String("url", "http://localhost:8080", "Sway base URL")
	shoppers := flag.Int("shoppers", 16, "concurrent virtual shoppers")
	orders := flag.Int("orders", 200, "total purchases to make")
	refundRate := flag.Float64("refund-rate", 0.2, "fraction of orders refunded (half of them partially)")
	flag.Parse()
	b := strings.TrimRight(*base, "/")
	st := &stats{latencies: map[string][]time.Duration{}, errors: map[string]int{}}
	var made atomic.Int64
	var ids sync.Map
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *shoppers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 99))
			for made.Add(1) <= int64(*orders) {
				q := fmt.Sprintf("%s (shopper %d, %d)", requests[r.IntN(len(requests))], w, r.IntN(1_000_000))
				var d struct {
					ID      string `json:"id"`
					Sources []struct {
						ID string `json:"id"`
					} `json:"sources"`
				}
				if call(b, st, "decide", "POST", "/api/agent/decide", map[string]string{"request": q}, &d) != 200 {
					continue
				}
				if len(d.Sources) > 0 {
					call(b, st, "rerun", "POST", "/api/decisions/"+d.ID+"/rerun", map[string][]string{"exclude_source_ids": {d.Sources[r.IntN(len(d.Sources))].ID}}, nil)
				}
				var co struct {
					OrderID       string `json:"order_id"`
					PayPalOrderID string `json:"paypal_order_id"`
					Amount        int64  `json:"amount_cents"`
				}
				if call(b, st, "create_order", "POST", "/api/paypal/orders", map[string]string{"decision_id": d.ID}, &co) != 201 {
					continue
				}
				if call(b, st, "capture", "POST", "/api/paypal/orders/"+co.PayPalOrderID+"/capture", nil, nil) != 200 {
					continue
				}
				ids.Store(co.OrderID, true)
				if r.Float64() < *refundRate {
					amt := int64(0)
					if r.IntN(2) == 0 {
						amt = co.Amount / 2
					}
					call(b, st, "refund", "POST", "/api/merchant/orders/"+co.OrderID+"/refund", map[string]int64{"amount_cents": amt}, nil)
				}
				call(b, st, "get_order", "GET", "/api/orders/"+co.OrderID, nil, nil)
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Settle: wait for every attribution, vest, pay out.
	type overview struct {
		Orders []struct {
			ID                string `json:"id"`
			AttributionStatus string `json:"attribution_status"`
			Balanced          bool   `json:"balanced"`
		} `json:"orders"`
		Verification struct {
			OK        bool `json:"ok"`
			Entries   int  `json:"entries"`
			TrialZero bool `json:"trial_balance_zero"`
		} `json:"verification"`
	}
	var ov overview
	deadline := time.Now().Add(5 * time.Minute)
	for {
		call(b, st, "overview", "GET", "/api/merchant/overview", nil, &ov)
		pending := 0
		for _, o := range ov.Orders {
			if _, mine := ids.Load(o.ID); mine && o.AttributionStatus != "DONE" {
				pending++
			}
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	call(b, st, "fast_forward", "POST", "/api/admin/fast-forward", map[string]any{}, nil)
	call(b, st, "payouts", "POST", "/api/merchant/payouts/run", map[string]any{}, nil)
	time.Sleep(6 * time.Second) // let testMode payouts settle and be polled
	call(b, st, "overview", "GET", "/api/merchant/overview", nil, &ov)

	n := 0
	ids.Range(func(_, _ any) bool { n++; return true })
	fmt.Printf("\nSway load test: %d shoppers, %d purchases completed in %s (%.1f purchases/s)\n\n", *shoppers, n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	fmt.Printf("%-14s %7s %9s %9s %9s %7s\n", "endpoint", "calls", "p50", "p95", "p99", "5xx")
	eps := []string{"decide", "rerun", "create_order", "capture", "refund", "get_order", "overview"}
	failed := false
	for _, ep := range eps {
		ls := st.latencies[ep]
		fmt.Printf("%-14s %7d %9s %9s %9s %7d\n", ep, len(ls), pct(ls, .5).Round(time.Millisecond), pct(ls, .95).Round(time.Millisecond), pct(ls, .99).Round(time.Millisecond), st.errors[ep])
		if st.errors[ep] > 0 {
			failed = true
		}
	}
	fmt.Printf("\nrate-limited retries: %d\n", st.limited.Load())
	unbalanced, notDone := 0, 0
	for _, o := range ov.Orders {
		if _, mine := ids.Load(o.ID); !mine {
			continue
		}
		if !o.Balanced {
			unbalanced++
		}
		if o.AttributionStatus != "DONE" {
			notDone++
		}
	}
	fmt.Printf("orders balanced: %d/%d, attributions done: %d/%d\n", n-unbalanced, n, n-notDone, n)
	fmt.Printf("ledger: %d entries, hash chain %v, trial balance zero %v\n", ov.Verification.Entries, ok(ov.Verification.OK), ov.Verification.TrialZero)
	if failed || unbalanced > 0 || notDone > 0 || !ov.Verification.OK || !ov.Verification.TrialZero {
		fmt.Println("\nFAIL")
		os.Exit(1)
	}
	fmt.Println("\nPASS: no server errors, every order balances to the cent, ledger verified")
}

func ok(b bool) string {
	if b {
		return "intact"
	}
	return "BROKEN"
}

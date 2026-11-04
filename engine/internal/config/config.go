// Package config reads Sway's runtime configuration from the environment.
//
// Every value has a safe default so a fresh clone runs with zero setup:
// no PayPal keys -> the PayPal simulator; no Anthropic key -> the deterministic
// offline agent. The UI always shows which mode is live, so nothing is faked silently.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type PayPal struct {
	Mode          string // "sandbox" (real REST API) or "sim" (in-database simulator)
	Env           string // "sandbox" or "live"
	ClientID      string
	ClientSecret  string
	WebhookID     string
	BaseURL       string
	BuyerEmail    string // published sandbox buyer login for judges
	BuyerPassword string
}

type LLM struct {
	Mode          string // "claude" or "sim"
	APIKey        string
	BaseURL       string
	DecisionModel string
	ExplainModel  string
	Concurrency   int
	Timeout       time.Duration
}

type Attribution struct {
	Samples        int     // K samples per coalition
	Players        int     // exact-Shapley players (top by |leave-one-out|)
	MaxCalls       int     // budget guard before falling back to Monte Carlo
	MaxSources     int     // retrieval cap
	Tau            float64 // softmax temperature over 0..100 scores
	MinInfluence   float64 // below this v(N)-v(none), content "didn't matter"
	MCPermutations int
}

type Config struct {
	DatabaseURL       string
	Addr              string
	PublicBaseURL     string
	DemoMode          bool
	CatalogMode       string // "demo" or "live" (Channel3)
	Channel3APIKey    string
	Channel3BaseURL   string
	VestingWindow     time.Duration
	CommissionRateBps int
	SwayFeeBps        int
	MinPayoutCents    int64
	WorkerInline      bool
	WorkerPoll        time.Duration
	VestingTick       time.Duration
	AutoPayout        time.Duration // 0 disables automatic payout runs
	TrustProxy        bool
	RateLimitScale    int    // multiplies every rate limit (load tests and CI e2e); 1 in production
	WebDir            string // optional override for UI assets (otherwise embedded)
	AllowedOrigins    []string
	PayPal            PayPal
	LLM               LLM
	Attribution       Attribution
}

func Load() (Config, error) {
	var errs []string
	str := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s must be an integer (got %q)", k, v))
			return def
		}
		return n
	}
	boolean := func(k string, def bool) bool {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
		switch v {
		case "":
			return def
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		errs = append(errs, fmt.Sprintf("%s must be a boolean (got %q)", k, v))
		return def
	}

	ppID, ppSecret := str("PAYPAL_CLIENT_ID", ""), str("PAYPAL_CLIENT_SECRET", "")
	ppMode := "sim"
	if ppID != "" && ppSecret != "" {
		ppMode = "sandbox"
	}
	ppMode = str("PAYPAL_MODE", ppMode)
	ppEnv := str("PAYPAL_ENV", "sandbox")
	ppBase := "https://api-m.sandbox.paypal.com"
	if ppEnv == "live" {
		ppBase = "https://api-m.paypal.com"
	}

	key := str("ANTHROPIC_API_KEY", "")
	llmMode := "sim"
	if key != "" {
		llmMode = "claude"
	}
	llmMode = str("LLM_MODE", llmMode)

	port := str("PORT", "8080")
	c := Config{
		DatabaseURL:       str("DATABASE_URL", "postgres://sway@localhost:5432/sway?sslmode=disable"),
		Addr:              str("ADDR", ":"+port),
		PublicBaseURL:     strings.TrimRight(str("PUBLIC_BASE_URL", "http://localhost:"+port), "/"),
		DemoMode:          boolean("DEMO_MODE", true),
		CatalogMode:       str("CATALOG_MODE", "demo"),
		Channel3APIKey:    str("CHANNEL3_API_KEY", ""),
		Channel3BaseURL:   strings.TrimRight(str("CHANNEL3_BASE_URL", "https://api.trychannel3.com"), "/"),
		VestingWindow:     time.Duration(num("VESTING_WINDOW_SECONDS", 120)) * time.Second,
		CommissionRateBps: num("COMMISSION_RATE_BPS", 800),
		SwayFeeBps:        num("SWAY_FEE_BPS", 1500),
		MinPayoutCents:    int64(num("MIN_PAYOUT_CENTS", 50)),
		WorkerInline:      boolean("WORKER_INLINE", true),
		WorkerPoll:        time.Duration(num("WORKER_POLL_MS", 500)) * time.Millisecond,
		VestingTick:       time.Duration(num("VESTING_TICK_SECONDS", 15)) * time.Second,
		AutoPayout:        time.Duration(num("AUTO_PAYOUT_SECONDS", 30)) * time.Second,
		TrustProxy:        boolean("TRUST_PROXY", false),
		RateLimitScale:    clamp(num("RATE_LIMIT_SCALE", 1), 1, 1000),
		WebDir:            str("WEB_DIR", ""),
		AllowedOrigins:    splitList(str("ALLOWED_ORIGINS", "http://localhost:3000")),
		PayPal: PayPal{
			Mode: ppMode, Env: ppEnv, ClientID: ppID, ClientSecret: ppSecret,
			WebhookID: str("PAYPAL_WEBHOOK_ID", ""), BaseURL: str("PAYPAL_BASE_URL", ppBase),
			BuyerEmail: str("PAYPAL_SANDBOX_BUYER_EMAIL", ""), BuyerPassword: str("PAYPAL_SANDBOX_BUYER_PASSWORD", ""),
		},
		LLM: LLM{
			Mode: llmMode, APIKey: key, BaseURL: str("ANTHROPIC_API_BASE", ""),
			DecisionModel: str("DECISION_MODEL", "claude-haiku-5-5"),
			ExplainModel:  str("EXPLAIN_MODEL", "claude-haiku-5-5"),
			Concurrency:   clamp(num("LLM_CONCURRENCY", 8), 1, 64),
			Timeout:       time.Duration(num("LLM_TIMEOUT_SECONDS", 60)) * time.Second,
		},
		Attribution: Attribution{
			Samples:        clamp(num("ATTRIBUTION_SAMPLES", 3), 1, 9),
			Players:        clamp(num("ATTRIBUTION_PLAYERS", 6), 1, 10),
			MaxCalls:       clamp(num("MAX_ATTRIBUTION_CALLS", 250), 10, 100000),
			MaxSources:     10,
			Tau:            10,
			MinInfluence:   0.05,
			MCPermutations: 40,
		},
	}
	if c.PayPal.Mode != "sim" && c.PayPal.Mode != "sandbox" {
		errs = append(errs, "PAYPAL_MODE must be sim or sandbox")
	}
	if c.PayPal.Mode == "sandbox" && (ppID == "" || ppSecret == "") {
		errs = append(errs, "PAYPAL_MODE=sandbox needs PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET")
	}
	if c.LLM.Mode != "sim" && c.LLM.Mode != "claude" {
		errs = append(errs, "LLM_MODE must be sim or claude")
	}
	if c.LLM.Mode == "claude" && key == "" {
		errs = append(errs, "LLM_MODE=claude needs ANTHROPIC_API_KEY")
	}
	if c.CommissionRateBps < 0 || c.CommissionRateBps > 10000 || c.SwayFeeBps < 0 || c.SwayFeeBps > 10000 {
		errs = append(errs, "COMMISSION_RATE_BPS and SWAY_FEE_BPS must be within 0..10000")
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

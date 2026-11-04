// Package api is Sway's HTTP surface: the JSON API plus the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/history"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/mcp"
	"github.com/vivekyarra/sway/engine/internal/payments"
	"github.com/vivekyarra/sway/engine/internal/screening"
	"github.com/vivekyarra/sway/engine/internal/shop"
	"github.com/vivekyarra/sway/engine/internal/views"
)

// Pinger is implemented by providers that can check connectivity (the real PayPal client).
type Pinger interface {
	Ping(ctx context.Context) error
}

type Server struct {
	Cfg        config.Config
	DB         *db.DB
	Log        *slog.Logger
	Agent      *agent.Agent
	Shop       *shop.Service
	Pay        *payments.Service
	History    *history.Builder
	Classifier screening.Classifier
	Web        http.Handler // static UI (may be nil)
	TrustProxy bool
	Version    string
	Metrics    *Metrics

	agentLimit, writeLimit, readLimit *limiter
	resetMu                           chan struct{}
}

// Handler builds the full HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Metrics == nil {
		s.Metrics = newMetrics()
	}
	k := max(1, s.Cfg.RateLimitScale)
	s.agentLimit = newLimiter(40*k, 15*k)   // agent runs cost LLM calls
	s.writeLimit = newLimiter(120*k, 30*k)  // payments and admin actions
	s.readLimit = newLimiter(1200*k, 200*k) // dashboards poll
	s.resetMu = make(chan struct{}, 1)

	mux := http.NewServeMux()
	r := func(pattern string, l *limiter, h http.HandlerFunc) { mux.HandleFunc(pattern, s.rateLimit(l, h)) }
	post := func(h http.HandlerFunc) http.HandlerFunc { return limitBody(64<<10, s.idempotent(h)) }

	r("GET /api/health", s.readLimit, s.health)
	r("GET /api/config", s.readLimit, s.config)
	r("POST /api/agent/decide", s.agentLimit, post(s.decide))
	r("GET /api/decisions/{id}", s.readLimit, s.getDecision)
	r("GET /api/decisions/{id}/influence", s.agentLimit, s.influence)
	r("POST /api/decisions/{id}/rerun", s.agentLimit, post(s.rerun))
	r("POST /api/paypal/orders", s.writeLimit, post(s.createOrder))
	r("POST /api/paypal/orders/{id}/capture", s.writeLimit, post(s.capture))
	r("GET /api/orders/{id}", s.readLimit, s.getOrder)
	r("POST /api/webhooks/paypal", s.writeLimit, limitBody(256<<10, s.webhook))
	r("GET /api/creators", s.readLimit, s.listCreators)
	r("GET /api/creators/{id}", s.readLimit, s.getCreator)
	r("GET /api/merchant/overview", s.readLimit, s.merchant)
	r("POST /api/merchant/orders/{id}/refund", s.writeLimit, post(s.refund))
	r("POST /api/merchant/orders/{id}/test-dispute", s.writeLimit, post(s.testToolsOnly(s.testDispute)))
	r("POST /api/merchant/payouts/run", s.writeLimit, post(s.runPayouts))
	r("POST /api/merchant/settings", s.writeLimit, post(s.updateSettings))
	r("POST /api/admin/fast-forward", s.writeLimit, post(s.testToolsOnly(s.fastForward)))
	r("POST /api/admin/reset", s.writeLimit, post(s.testToolsOnly(s.reset)))
	r("GET /api/ledger/verify", s.readLimit, s.verifyLedger)
	mcpServer := &mcp.Server{Shop: s.Shop, DB: s.DB, PublicBaseURL: s.Cfg.PublicBaseURL, Version: s.Version, AllowedOrigins: s.Cfg.AllowedOrigins}
	mux.HandleFunc("POST /mcp", s.rateLimit(s.agentLimit, limitBody(64<<10, mcpServer.ServeHTTP)))
	mux.HandleFunc("GET /mcp", mcpServer.ServeHTTP)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such API route")
	})
	if s.Web != nil {
		mux.Handle("/", s.Web)
	}
	return s.withObservability(s.withSecurityHeaders(s.withCORS(mux)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

type apiError struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Suggestions []string `json:"suggestions,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// fail maps domain errors onto HTTP status codes without leaking internals.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var unsupported *shop.UnsupportedError
	switch {
	case errors.As(err, &unsupported):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]apiError{"error": {Code: "unsupported_request", Message: unsupported.Error(), Suggestions: unsupported.Suggestions}})
	case errors.Is(err, payments.ErrNotFound), errors.Is(err, views.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	case errors.Is(err, payments.ErrTestToolsOnly):
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	case errors.Is(err, payments.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", trimErr(err))
	case errors.Is(err, payments.ErrInvalid), errors.Is(err, payments.ErrNotRefundable):
		writeError(w, http.StatusUnprocessableEntity, "invalid", trimErr(err))
	case errors.Is(err, payments.ErrBadSignature):
		writeError(w, http.StatusBadRequest, "bad_signature", "Webhook signature verification failed")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "The request took too long. Please try again.")
	default:
		s.Log.Error("request failed", "request_id", RequestID(r.Context()), "route", r.Pattern, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong. Request id: "+RequestID(r.Context()))
	}
}

func trimErr(err error) string {
	msg := err.Error()
	for _, p := range []string{"invalid request: ", "conflict: ", "order is not refundable "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return msg
}

func (s *Server) testToolsOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Cfg.TestTools {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		h(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := map[string]any{"version": s.Version, "time": time.Now().UTC()}
	status := http.StatusOK
	if err := s.DB.Pool.Ping(ctx); err != nil {
		out["db"], status = "down", http.StatusServiceUnavailable
	} else {
		out["db"] = "ok"
	}
	out["paypal"] = map[string]string{"mode": s.Pay.PP.Mode(), "status": "ok"}
	if p, ok := s.Pay.PP.(Pinger); ok {
		if err := p.Ping(ctx); err != nil {
			out["paypal"] = map[string]string{"mode": s.Pay.PP.Mode(), "status": "unreachable"}
		}
	}
	out["llm"] = map[string]any{"mode": s.Agent.Engine.Name(), "model": s.Agent.Engine.Model(), "calls": s.Agent.Calls.Load(), "cache_hits": s.Agent.CacheHits.Load()}
	var queued, failed int
	_ = s.DB.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='QUEUED'), count(*) FILTER (WHERE status='FAILED') FROM jobs`).Scan(&queued, &failed)
	out["jobs"] = map[string]int{"queued": queued, "failed": failed}
	writeJSON(w, status, out)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	var cats []map[string]string
	if s.Shop != nil {
		keys := make([]string, 0, len(s.Shop.Categories))
		for k := range s.Shop.Categories {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := s.Shop.Categories[k]
			cats = append(cats, map[string]string{"key": k, "label": c.Label, "suggested_prompt": c.SuggestedPrompt})
		}
	}
	pp := map[string]any{"mode": s.Pay.PP.Mode()}
	if s.Pay.PP.Mode() == "sandbox" {
		pp["client_id"] = s.Cfg.PayPal.ClientID
		pp["buyer_email"] = s.Cfg.PayPal.BuyerEmail
		pp["buyer_password"] = s.Cfg.PayPal.BuyerPassword
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.Version, "test_tools": s.Cfg.TestTools, "catalog_mode": s.Cfg.CatalogMode,
		"paypal":                 pp,
		"llm":                    map[string]string{"mode": s.Agent.Engine.Name(), "model": s.Agent.Engine.Model(), "prompt_version": agent.PromptVersion},
		"vesting_window_seconds": int(s.Cfg.VestingWindow.Seconds()), "commission_rate_bps": s.Cfg.CommissionRateBps,
		"sway_fee_bps": s.Cfg.SwayFeeBps, "min_payout_cents": s.Cfg.MinPayoutCents,
		"attribution": map[string]any{"samples": s.Cfg.Attribution.Samples, "players": s.Cfg.Attribution.Players, "max_calls": s.Cfg.Attribution.MaxCalls, "tau": s.Cfg.Attribution.Tau},
		"categories":  cats,
	})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Request string `json:"request"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"request\": \"...\"}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	dv, err := s.Shop.Decide(ctx, body.Request)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dv)
}

func (s *Server) getDecision(w http.ResponseWriter, r *http.Request) {
	dv, err := s.Shop.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "not_found", "Decision not found")
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dv)
}

func (s *Server) influence(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	loo, err := s.Shop.Influence(ctx, r.PathValue("id"))
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "not_found", "Decision not found")
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"estimated": true, "influence": loo})
}

func (s *Server) rerun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Exclude []string `json:"exclude_source_ids"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"exclude_source_ids\": [...]}")
		return
	}
	if len(body.Exclude) > 20 {
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Too many sources to exclude")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	rv, err := s.Shop.Rerun(ctx, r.PathValue("id"), body.Exclude)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "not_found", "Decision not found")
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DecisionID string `json:"decision_id"`
	}
	if err := decode(r, &body); err != nil || body.DecisionID == "" {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"decision_id\": \"...\"}")
		return
	}
	co, err := s.Pay.CreateOrder(r.Context(), body.DecisionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, co)
}

func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	orderID, err := s.Pay.Capture(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o, err := views.GetOrder(r.Context(), s.DB.Pool, orderID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	o, err := views.GetOrder(r.Context(), s.DB.Pool, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Webhook body too large")
		return
	}
	dup, err := s.Pay.IngestWebhook(r.Context(), r.Header, body)
	if err != nil {
		if !errors.Is(err, payments.ErrBadSignature) && !errors.Is(err, payments.ErrInvalid) {
			s.fail(w, r, err) // 5xx: PayPal will redeliver
			return
		}
		s.Log.Warn("webhook rejected", "request_id", RequestID(r.Context()), "err", err)
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true, "duplicate": dup})
}

func (s *Server) listCreators(w http.ResponseWriter, r *http.Request) {
	cs, err := views.ListCreators(r.Context(), s.DB.Pool)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"creators": cs})
}

func (s *Server) getCreator(w http.ResponseWriter, r *http.Request) {
	c, err := views.GetCreator(r.Context(), s.DB.Pool, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) merchant(w http.ResponseWriter, r *http.Request) {
	m, err := views.GetMerchant(r.Context(), s.DB.Pool, s.Cfg.SwayFeeBps)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) refund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
	}
	if err := decode(r, &body); err != nil || body.AmountCents < 0 {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"amount_cents\": optional positive integer}")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	res, err := s.Pay.Refund(r.Context(), r.PathValue("id"), body.AmountCents, reason, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) testDispute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"action\": \"open|resolve_seller|resolve_buyer\"}")
		return
	}
	if err := s.Pay.TestDispute(r.Context(), r.PathValue("id"), body.Action); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "test_event": true, "action": body.Action})
}

func (s *Server) runPayouts(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Pay.VestDue(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	plan, err := s.Pay.RunPayouts(r.Context(), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// updateSettings changes the merchant's commission rate and vesting window. The Sway fee stays
// read-only. New orders use the new values; existing orders keep the terms they were bought under.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CommissionRateBps    *int `json:"commission_rate_bps"`
		VestingWindowSeconds *int `json:"vesting_window_seconds"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Body must be JSON: {\"commission_rate_bps\": 800, \"vesting_window_seconds\": 120}")
		return
	}
	if body.CommissionRateBps != nil && (*body.CommissionRateBps < 0 || *body.CommissionRateBps > 3000) {
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Commission must be between 0% and 30%")
		return
	}
	minVest := 10
	if !s.Cfg.TestTools {
		minVest = 86400
	}
	if body.VestingWindowSeconds != nil && (*body.VestingWindowSeconds < minVest || *body.VestingWindowSeconds > 90*86400) {
		writeError(w, http.StatusUnprocessableEntity, "invalid", fmt.Sprintf("Vesting window must be between %ds and 90 days", minVest))
		return
	}
	_, err := s.DB.Pool.Exec(r.Context(), `UPDATE merchants SET commission_rate_bps=COALESCE($1, commission_rate_bps), vesting_window_seconds=COALESCE($2, vesting_window_seconds)
		WHERE id=(SELECT id FROM merchants ORDER BY created_at LIMIT 1)`, body.CommissionRateBps, body.VestingWindowSeconds)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := views.GetMerchant(r.Context(), s.DB.Pool, s.Cfg.SwayFeeBps)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commission_rate_bps": m.CommissionRateBps, "vesting_window_seconds": m.VestingSeconds, "sway_fee_bps": m.SwayFeeBps})
}

func (s *Server) fastForward(w http.ResponseWriter, r *http.Request) {
	n, err := s.Pay.FastForward(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vested, err := s.Pay.VestDue(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"made_eligible": n, "vested": vested})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	select {
	case s.resetMu <- struct{}{}:
		defer func() { <-s.resetMu }()
	default:
		writeError(w, http.StatusConflict, "in_progress", "A reset is already running")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	if err := s.History.Reset(ctx, s.Classifier); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

func (s *Server) verifyLedger(w http.ResponseWriter, r *http.Request) {
	v, err := ledger.Verify(r.Context(), s.DB.Pool)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	extra := map[string]float64{
		"sway_llm_calls_total":      float64(s.Agent.Calls.Load()),
		"sway_llm_cache_hits_total": float64(s.Agent.CacheHits.Load()),
	}
	var queued, running, failed float64
	_ = s.DB.Pool.QueryRow(r.Context(), `SELECT count(*) FILTER (WHERE status='QUEUED'), count(*) FILTER (WHERE status='RUNNING'), count(*) FILTER (WHERE status='FAILED') FROM jobs`).
		Scan(&queued, &running, &failed)
	extra["sway_jobs_queued"], extra["sway_jobs_running"], extra["sway_jobs_failed"] = queued, running, failed
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.Metrics.write(w, extra)
}

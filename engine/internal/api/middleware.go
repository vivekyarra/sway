package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/db"
)

type ctxKey int

const requestIDKey ctxKey = 1

// statusRecorder captures the response for logging, metrics and idempotent replay.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	bytes   int
	capture *bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.capture != nil {
		r.capture.Write(b)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RequestID returns the request id for ctx.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withObservability adds a request id, recovers panics, records metrics and writes one structured log line per request.
func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("panic", "request_id", id, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if rec.status == 0 {
					writeError(rec, http.StatusInternalServerError, "internal", "Something went wrong. Request id: "+id)
				}
			}
			dur := time.Since(start)
			route := r.Pattern
			if route == "" {
				route = "static"
			}
			s.Metrics.observe(route, rec.status, dur)
			if strings.HasPrefix(r.URL.Path, "/api/") || rec.status >= 500 {
				lvl := slog.LevelInfo
				if rec.status >= 500 {
					lvl = slog.LevelError
				}
				s.Log.Log(r.Context(), lvl, "http", "request_id", id, "method", r.Method, "route", route, "status", rec.status, "ms", dur.Milliseconds(), "bytes", rec.bytes)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// withSecurityHeaders applies a strict CSP (PayPal's checkout domains allowed), HSTS on https and the usual hardening headers.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' https://www.paypal.com https://*.paypal.com https://*.paypalobjects.com",
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
		"font-src 'self' https://fonts.gstatic.com data:",
		"img-src 'self' data: https://*.paypal.com https://*.paypalobjects.com",
		"connect-src 'self' https://*.paypal.com https://*.paypalobjects.com",
		"frame-src https://*.paypal.com https://*.paypalobjects.com",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self' https://*.paypal.com",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(self \"https://www.paypal.com\")")
		h.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS allows the configured dev origins (the Next.js dev server) and nothing else.
func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.Cfg.AllowedOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && allowed[o] {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP uses the left-most X-Forwarded-For hop only when running behind a trusted proxy (Render).
func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter is a per-key token bucket.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newLimiter(perMinute, burst int) *limiter {
	return &limiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes a token for key; it returns false and the wait when the bucket is empty.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute { // evict idle buckets to bound memory
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens = minf(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// rateLimit wraps a handler with a token bucket keyed by client IP.
func (s *Server) rateLimit(l *limiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(s.clientIP(r), time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			s.Metrics.rateLimited.Add(1)
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Slow down and try again shortly.")
			return
		}
		h(w, r)
	}
}

// limitBody caps request bodies.
func limitBody(n int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		h(w, r)
	}
}

// idempotent makes a POST replay-safe with an Idempotency-Key header (the Stripe convention):
// the first response is stored, a retry with the same key and body gets the same response,
// the same key with a different body is rejected, and a concurrent duplicate gets 409.
func (s *Server) idempotent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || r.Method != http.MethodPost {
			h(w, r)
			return
		}
		if len(key) > 200 {
			writeError(w, http.StatusBadRequest, "bad_idempotency_key", "Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.URL.Path+"\x00"), body...))
		hash := hex.EncodeToString(sum[:])
		scope := r.URL.Path
		ctx := r.Context()
		var storedHash string
		var status int
		var resp []byte
		err = s.DB.Pool.QueryRow(ctx, `SELECT request_hash, status_code, response FROM idempotency_keys WHERE key=$1 AND scope=$2`, key, scope).Scan(&storedHash, &status, &resp)
		switch {
		case err == nil:
			if storedHash != hash {
				writeError(w, http.StatusUnprocessableEntity, "idempotency_mismatch", "This Idempotency-Key was used with a different request")
				return
			}
			if status == 0 {
				writeError(w, http.StatusConflict, "in_progress", "A request with this Idempotency-Key is still in progress")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		case !errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusInternalServerError, "internal", "idempotency store unavailable")
			return
		}
		if _, err := s.DB.Pool.Exec(ctx, `INSERT INTO idempotency_keys (key, scope, request_hash) VALUES ($1,$2,$3)`, key, scope, hash); err != nil {
			if db.IsUniqueViolation(err) {
				writeError(w, http.StatusConflict, "in_progress", "A request with this Idempotency-Key is still in progress")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "idempotency store unavailable")
			return
		}
		rec := &statusRecorder{ResponseWriter: w, capture: &bytes.Buffer{}}
		h(rec, r)
		bg := context.WithoutCancel(ctx)
		if rec.status >= 500 || rec.status == 0 {
			// Server errors are not cached: the client may retry with the same key.
			_, _ = s.DB.Pool.Exec(bg, `DELETE FROM idempotency_keys WHERE key=$1 AND scope=$2`, key, scope)
			return
		}
		_, _ = s.DB.Pool.Exec(bg, `UPDATE idempotency_keys SET status_code=$3, response=$4 WHERE key=$1 AND scope=$2`, key, scope, rec.status, rec.capture.Bytes())
	}
}

// Metrics is a tiny Prometheus-text exporter (no external deps).
type Metrics struct {
	mu          sync.Mutex
	requests    map[string]int64
	latencySum  map[string]float64
	rateLimited atomic.Int64
	started     time.Time
}

func newMetrics() *Metrics {
	return &Metrics{requests: map[string]int64{}, latencySum: map[string]float64{}, started: time.Now()}
}

func (m *Metrics) observe(route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fmt.Sprintf("route=%q,status=\"%d\"", route, status)
	m.requests[k]++
	m.latencySum[route] += d.Seconds()
}

func (m *Metrics) write(w io.Writer, extra map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(w, "# TYPE sway_http_requests_total counter")
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "sway_http_requests_total{%s} %d\n", k, m.requests[k])
	}
	fmt.Fprintln(w, "# TYPE sway_http_request_seconds_sum counter")
	routes := make([]string, 0, len(m.latencySum))
	for r := range m.latencySum {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		fmt.Fprintf(w, "sway_http_request_seconds_sum{route=%q} %.6f\n", r, m.latencySum[r])
	}
	fmt.Fprintf(w, "# TYPE sway_rate_limited_total counter\nsway_rate_limited_total %d\n", m.rateLimited.Load())
	fmt.Fprintf(w, "# TYPE sway_uptime_seconds gauge\nsway_uptime_seconds %.0f\n", time.Since(m.started).Seconds())
	names := make([]string, 0, len(extra))
	for k := range extra {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %g\n", k, k, extra[k])
	}
}

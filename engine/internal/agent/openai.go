package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenAICompat runs decisions on any OpenAI-compatible Chat Completions API with JSON-schema output.
// It powers the Google Gemini provider (Gemini's OpenAI-compatible endpoint) and works with Groq,
// OpenRouter, OpenAI or a local server. Free tiers are rate limited, so requests are paced to a
// requests-per-minute budget and 429/5xx responses are retried with backoff. If the model is still
// unavailable after retries, a decision falls back to Sway's built-in engine so a purchase never stalls;
// Fallbacks counts how often that happened (reported on /api/health).
type OpenAICompat struct {
	provider     string
	baseURL      string
	apiKey       string
	model        string
	explainModel string
	timeout      time.Duration
	http         *http.Client

	mu       sync.Mutex
	next     time.Time
	interval time.Duration

	Fallbacks atomic.Int64
	local     Local
}

// NewOpenAICompat builds a client. rpm <= 0 disables pacing.
func NewOpenAICompat(provider, baseURL, apiKey, model, explainModel string, rpm int, timeout time.Duration) *OpenAICompat {
	c := &OpenAICompat{
		provider: provider, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		model: model, explainModel: explainModel, timeout: timeout,
		http: &http.Client{Timeout: timeout},
	}
	if explainModel == "" {
		c.explainModel = model
	}
	if rpm > 0 {
		c.interval = time.Minute / time.Duration(rpm)
	}
	return c
}

func (c *OpenAICompat) Name() string  { return c.provider }
func (c *OpenAICompat) Model() string { return c.model }

// Decide runs one decision; an invalid output is retried once, then Sway's built-in engine answers.
func (c *OpenAICompat) Decide(ctx context.Context, in Input, sample int) (Output, error) {
	in = in.Normalize()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		text, err := c.structured(ctx, c.model, SystemPrompt, UserMessage(in), "decision", decisionSchema)
		if err != nil {
			lastErr = err
			break
		}
		var out Output
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			lastErr = fmt.Errorf("decision JSON: %w", err)
			continue
		}
		if err := Validate(in, &out); err != nil {
			lastErr = err
			continue
		}
		return out, nil
	}
	if ctx.Err() != nil {
		return Output{}, ctx.Err()
	}
	c.Fallbacks.Add(1)
	out, err := c.local.Decide(ctx, in, sample)
	if err != nil {
		return Output{}, fmt.Errorf("%s decision failed (%v) and fallback failed: %w", c.provider, lastErr, err)
	}
	return out, nil
}

// ClassifyInjection asks the model whether a source tries to command AI agents.
func (c *OpenAICompat) ClassifyInjection(ctx context.Context, title, body string) (bool, string, error) {
	text, err := c.structured(ctx, c.explainModel, injectionSystem, "<content>\n"+escapeSource(title)+"\n"+escapeSource(body)+"\n</content>", "screening", injectionSchema)
	if err != nil {
		return false, "", err
	}
	var r struct {
		Flag     bool   `json:"is_instruction_to_agent"`
		Evidence string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return false, "", err
	}
	return r.Flag, r.Evidence, nil
}

// Explain writes one or two sentences telling a creator why their source mattered (no numbers).
func (c *OpenAICompat) Explain(ctx context.Context, e ExplainInput) (string, error) {
	text, err := c.structured(ctx, c.explainModel, explainSystem, explainUser(e), "explanation", explainSchema)
	if err != nil {
		return "", err
	}
	var r struct {
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return "", err
	}
	return StripNumbers(r.Explanation), nil
}

func (c *OpenAICompat) wait(ctx context.Context) error {
	if c.interval == 0 {
		return nil
	}
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.interval)
	c.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *OpenAICompat) structured(ctx context.Context, model, system, user, name string, schema map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system + "\nRespond only with a JSON object matching the provided schema."},
			{"role": "user", "content": user},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": name, "schema": portableSchema(schema)},
		},
	})
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := c.wait(ctx); err != nil {
			return "", err
		}
		text, retryAfter, err := c.post(ctx, body)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if retryAfter < 0 {
			return "", err
		}
		if retryAfter == 0 {
			retryAfter = time.Duration(1<<attempt) * time.Second
		}
		if retryAfter > 30*time.Second {
			retryAfter = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryAfter):
		}
	}
	return "", lastErr
}

// post sends one request. retryAfter < 0 means the error is permanent.
func (c *OpenAICompat) post(ctx context.Context, body []byte) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%s request: %w", c.provider, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		ra := time.Duration(0)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ra = time.Duration(s) * time.Second
		}
		return "", ra, fmt.Errorf("%s %d: %s", c.provider, resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return "", -1, fmt.Errorf("%s %d: %s", c.provider, resp.StatusCode, snippet(raw))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", -1, fmt.Errorf("%s response: %w", c.provider, err)
	}
	if len(cr.Choices) == 0 {
		return "", 0, errors.New(c.provider + ": empty response")
	}
	ch := cr.Choices[0]
	if ch.Message.Refusal != "" {
		return "", -1, errors.New("model refused the request")
	}
	if ch.FinishReason == "length" {
		return "", 0, errors.New("model output truncated")
	}
	return stripFences(ch.Message.Content), 0, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// portableSchema drops keywords some OpenAI-compatible providers reject (additionalProperties, description).
func portableSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if k == "additionalProperties" || k == "description" {
				continue
			}
			out[k] = portableSchema(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = portableSchema(val)
		}
		return out
	default:
		return v
	}
}

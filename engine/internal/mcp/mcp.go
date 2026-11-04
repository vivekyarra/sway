// Package mcp exposes Sway to any AI agent over the Model Context Protocol (Streamable HTTP transport,
// JSON responses). An agent such as Claude Desktop can ask Sway's neutral shopper for a pick, inspect which
// human sources swayed it, run counterfactuals, hand the human a PayPal checkout link, and later read who was
// paid. That is the "attribution layer for any agent" from the Sway roadmap, live.
//
// The agent never touches money: checkout always happens in PayPal, initiated by the human from the link.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/catalog"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/shop"
	"github.com/vivekyarra/sway/engine/internal/views"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

var supportedVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true}

type Server struct {
	Shop           *shop.Service
	DB             *db.DB
	PublicBaseURL  string
	Version        string
	AllowedOrigins []string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser MCP clients send no Origin
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	if base, err := url.Parse(s.PublicBaseURL); err == nil && u.Host == base.Host {
		return true
	}
	for _, a := range s.AllowedOrigins {
		if a == o {
			return true
		}
	}
	return u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
}

// ServeHTTP handles POST /mcp (one JSON-RPC message per request).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP server returns JSON responses and does not open SSE streams", http.StatusMethodNotAllowed)
		return
	}
	if !s.originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "could not read body"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid JSON-RPC request (batches are not supported)"}})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted) // notification (e.g. notifications/initialized)
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if supportedVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "sway", "title": "Sway: attribution for agentic commerce", "version": s.Version},
			"instructions": "Use shop_recommend to get a neutral product pick backed by human-written sources. Show the user the checkout_url to pay with PayPal; " +
				"you never handle payment. Use explain_influence or counterfactual to show which sources swayed the pick. Creators are paid only if the buyer keeps the product.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		resp.Result = s.call(ctx, p.Name, p.Arguments)
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tools() []map[string]any {
	ro := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	return []map[string]any{
		{"name": "shop_recommend", "title": "Recommend a product",
			"description": "Ask Sway's neutral shopping agent for a pick. It reads human-written reviews, blogs, forum answers and video transcripts (screened for manipulation) and returns the pick, its rationale, the sources it read, and a PayPal checkout link for the human.",
			"inputSchema": obj(map[string]any{"request": str("What the shopper wants, e.g. 'noise-cancelling headphones for long-haul flights under $250'")}, "request"),
			"annotations": map[string]any{"readOnlyHint": false, "openWorldHint": false, "idempotentHint": false}},
		{"name": "explain_influence", "title": "Explain which sources swayed a pick",
			"description": "Leave-one-out influence for a decision: how much each source raised or lowered the chance of the pick, and what the agent would choose without it.",
			"inputSchema": obj(map[string]any{"decision_id": str("Decision id from shop_recommend")}, "decision_id"), "annotations": ro},
		{"name": "counterfactual", "title": "Re-decide without some sources",
			"description": "Re-run the same decision with the given sources removed (same prompt, model and candidates). Shows whether those sources swayed the sale.",
			"inputSchema": obj(map[string]any{"decision_id": str("Decision id"), "exclude_source_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "decision_id", "exclude_source_ids"),
			"annotations": ro},
		{"name": "order_attribution", "title": "Who gets paid for an order",
			"description": "Exact Shapley attribution for a purchase: each source's causal contribution, the commission split, and whether each creator's earnings are pending, vested, paid, forfeited or clawed back.",
			"inputSchema": obj(map[string]any{"order_id": str("Order id")}, "order_id"), "annotations": ro},
		{"name": "creator_earnings", "title": "A creator's earnings",
			"description": "Balances derived from Sway's ledger for one creator: pending, frozen, vested, paid, forfeited and owed.",
			"inputSchema": obj(map[string]any{"creator_id": str("Creator id, e.g. c_priya")}, "creator_id"), "annotations": ro},
		{"name": "verify_ledger", "title": "Verify the books",
			"description": "Re-walk Sway's SHA-256 hash-chained, double-entry ledger and confirm nothing was edited and the trial balance is zero.",
			"inputSchema": obj(map[string]any{}), "annotations": ro},
	}
}

type toolResult struct {
	Content           []map[string]string `json:"content"`
	StructuredContent any                 `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError"`
}

func ok(text string, data any) toolResult {
	return toolResult{Content: []map[string]string{{"type": "text", "text": text}}, StructuredContent: data}
}

func toolErr(msg string) toolResult {
	return toolResult{Content: []map[string]string{{"type": "text", "text": msg}}, IsError: true}
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) toolResult {
	var a struct {
		Request    string   `json:"request"`
		DecisionID string   `json:"decision_id"`
		Exclude    []string `json:"exclude_source_ids"`
		OrderID    string   `json:"order_id"`
		CreatorID  string   `json:"creator_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return toolErr("invalid arguments: " + err.Error())
		}
	}
	switch name {
	case "shop_recommend":
		dv, err := s.Shop.Decide(ctx, a.Request)
		var unsupported *shop.UnsupportedError
		if errors.As(err, &unsupported) {
			return toolErr(unsupported.Error())
		}
		if err != nil {
			return toolErr("the shopping agent failed: " + err.Error())
		}
		checkout := strings.TrimRight(s.PublicBaseURL, "/") + "/shop/?decision=" + url.QueryEscape(dv.ID)
		var srcs []map[string]string
		var lines []string
		for _, src := range dv.Sources {
			srcs = append(srcs, map[string]string{"id": src.ID, "creator": src.CreatorName, "type": src.Type, "title": src.Title})
			lines = append(lines, fmt.Sprintf("- %s (%s): %s [%s]", src.CreatorName, src.Type, src.Title, src.ID))
		}
		var screened []map[string]string
		for _, src := range dv.Ignored {
			screened = append(screened, map[string]string{"id": src.ID, "creator": src.CreatorName, "reason": src.Reason})
		}
		text := fmt.Sprintf("Pick: %s at %s (%.0f%% confident).\n%s\n\nSources read:\n%s\n\nCheckout (the human pays with PayPal): %s\nDecision id: %s",
			dv.Chosen.Name, catalog.FormatUSD(dv.Chosen.PriceCents), dv.Probabilities[dv.Chosen.ID]*100, dv.Rationale, strings.Join(lines, "\n"), checkout, dv.ID)
		return ok(text, map[string]any{"decision_id": dv.ID, "pick": dv.Chosen, "confidence": dv.Probabilities[dv.Chosen.ID], "rationale": dv.Rationale,
			"sources": srcs, "screened_out": screened, "checkout_url": checkout, "engine": dv.Engine, "model": dv.Model})

	case "explain_influence":
		loo, err := s.Shop.Influence(ctx, a.DecisionID)
		if err != nil {
			return toolErr("unknown decision or influence failed: " + err.Error())
		}
		_, dv, err := s.Shop.Load(ctx, s.DB.Pool, a.DecisionID)
		if err != nil {
			return toolErr(err.Error())
		}
		names := map[string]string{}
		for _, p := range dv.Candidates {
			names[p.ID] = p.Name
		}
		type row struct {
			ID, Creator, Without string
			Delta                float64
		}
		var rows []row
		for _, src := range dv.Sources {
			rows = append(rows, row{src.ID, src.CreatorName, names[loo.Picks[src.ID]], loo.Deltas[src.ID]})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Delta > rows[j].Delta })
		var lines []string
		for _, r := range rows {
			flip := ""
			if r.Without != dv.Chosen.Name {
				flip = fmt.Sprintf(" -> without it the agent picks %s (pivotal)", r.Without)
			}
			lines = append(lines, fmt.Sprintf("- %s: %+.1f pts%s", r.Creator, r.Delta*100, flip))
		}
		return ok(fmt.Sprintf("Influence on picking %s (leave-one-out, estimated):\n%s\nWith no sources the agent picks %s.", dv.Chosen.Name, strings.Join(lines, "\n"), names[loo.PickNo]),
			map[string]any{"decision_id": a.DecisionID, "pick": dv.Chosen.ID, "influence": loo})

	case "counterfactual":
		rv, err := s.Shop.Rerun(ctx, a.DecisionID, a.Exclude)
		if err != nil {
			return toolErr("unknown decision or re-run failed: " + err.Error())
		}
		verdict := "The pick did not change."
		if rv.Flipped {
			verdict = "The pick changed: those sources swayed the sale."
		}
		return ok(fmt.Sprintf("Without %s the agent picks %s (%s). %s", strings.Join(rv.Excluded, ", "), rv.Chosen.Name, rv.Agreement, verdict), rv)

	case "order_attribution":
		o, err := views.GetOrder(ctx, s.DB.Pool, a.OrderID)
		if err != nil {
			return toolErr("order not found")
		}
		o.Ledger = nil
		var lines []string
		for _, r := range o.Attributions {
			lines = append(lines, fmt.Sprintf("- %s: phi %+.1f pts, %s, %s", r.CreatorName, r.Phi*100, catalog.FormatUSD(r.AmountCents), r.Status))
		}
		status := "attribution pending"
		if o.Job != nil {
			status = "attribution " + strings.ToLower(o.Job.Status)
		}
		if o.Job != nil {
			o.Job.Coalitions = nil
		}
		return ok(fmt.Sprintf("%s, %s (%s). Commission %s: creators %s, Sway fee %s, back to merchant %s.\n%s", o.ProductName, catalog.FormatUSD(o.AmountCents), status,
			catalog.FormatUSD(o.Split.Commission), catalog.FormatUSD(o.Split.CreatorPoolNet), catalog.FormatUSD(o.Split.SwayFee), catalog.FormatUSD(o.Split.ReturnedToMerchant),
			strings.Join(lines, "\n")), o)

	case "creator_earnings":
		c, err := views.GetCreator(ctx, s.DB.Pool, a.CreatorID)
		if err != nil {
			return toolErr("creator not found")
		}
		b := c.Balances
		return ok(fmt.Sprintf("%s: %s Pending %s, vested %s, paid %s, forfeited %s, owed %s.", c.DisplayName, c.Headline, catalog.FormatUSD(b.Pending+b.Frozen),
			catalog.FormatUSD(b.Vested), catalog.FormatUSD(b.Paid), catalog.FormatUSD(b.Forfeited), catalog.FormatUSD(b.Owed)),
			map[string]any{"creator_id": c.ID, "name": c.DisplayName, "balances": b, "purchases_swayed": c.Swayed})

	case "verify_ledger":
		v, err := ledger.Verify(ctx, s.DB.Pool)
		if err != nil {
			return toolErr(err.Error())
		}
		text := fmt.Sprintf("Ledger verified: %d entries, hash chain intact, trial balance zero.", v.Entries)
		if !v.OK || !v.TrialZero {
			text = fmt.Sprintf("LEDGER PROBLEM at entry %d: %s", v.BrokenAtSeq, v.Problem)
		}
		return ok(text, v)
	}
	return toolErr("unknown tool: " + name)
}

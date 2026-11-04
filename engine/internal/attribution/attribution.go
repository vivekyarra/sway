// Package attribution turns a captured order into creator earnings (spec 6.5):
// screen sources by leave-one-out, run exact Shapley over the top players (Monte Carlo past the call
// budget), compute the influence share, and split the commission into the ledger in one transaction.
package attribution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vivekyarra/sway/engine/internal/agent"
	"github.com/vivekyarra/sway/engine/internal/config"
	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/ledger"
	"github.com/vivekyarra/sway/engine/internal/money"
	"github.com/vivekyarra/sway/engine/internal/shapley"
	"github.com/vivekyarra/sway/engine/internal/shop"
)

// MinPhi is the noise floor: influence at or below it (in probability points) is treated as zero.
const MinPhi = 0.01

// Explainer writes creator explanations (Claude in live mode; nil = deterministic templates).
type Explainer interface {
	Explain(ctx context.Context, e agent.ExplainInput) (string, error)
}

type Runner struct {
	DB        *db.DB
	Agent     *agent.Agent
	Shop      *shop.Service
	Cfg       config.Attribution
	Explainer Explainer
	Log       *slog.Logger
	// AfterAllocate runs after the allocation commits (lifecycle uses it to apply refunds/disputes that
	// arrived while attribution was running).
	AfterAllocate func(ctx context.Context, orderID string) error
}

// Player is one source competing for credit.
type Player struct {
	SourceID    string  `json:"source_id"`
	CreatorID   string  `json:"creator_id"`
	CreatorName string  `json:"creator_name"`
	Phi         float64 `json:"phi"`
	PhiStd      float64 `json:"phi_stddev"`
	LOO         float64 `json:"loo_delta"`
	PickWithout string  `json:"pick_without"`
	Role        string  `json:"role"`
}

// Coalition is one evaluated subset (for the coalition explorer).
type Coalition struct {
	Mask    uint32   `json:"mask"`
	Members []string `json:"members"`
	V       float64  `json:"v"`
	Std     float64  `json:"std"`
	Pick    string   `json:"pick"`
}

func newID(p string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return p + hex.EncodeToString(b[:])
}

type orderInfo struct {
	ID, DecisionID, ProductID string
	Amount                    int64
	CommissionBps, FeeBps     int
	PaymentMode               string
}

// Enqueue creates the attribution job row (idempotent per order).
func Enqueue(ctx context.Context, q db.Q, orderID string) error {
	_, err := q.Exec(ctx, `INSERT INTO attribution_jobs (id, order_id) VALUES ($1,$2) ON CONFLICT (order_id) DO NOTHING`, newID("att_"), orderID)
	return err
}

// Run attributes one order. Safe to call repeatedly: a finished job is a no-op.
func (r *Runner) Run(ctx context.Context, orderID string) error {
	if err := Enqueue(ctx, r.DB.Pool, orderID); err != nil {
		return err
	}
	var status string
	if err := r.DB.Pool.QueryRow(ctx, `SELECT status FROM attribution_jobs WHERE order_id=$1`, orderID).Scan(&status); err != nil {
		return err
	}
	if status == "DONE" {
		return nil
	}
	err := r.run(ctx, orderID)
	if err != nil {
		_, _ = r.DB.Pool.Exec(context.WithoutCancel(ctx), `UPDATE attribution_jobs SET status='FAILED', error=$2, phase='failed' WHERE order_id=$1 AND status<>'DONE'`, orderID, err.Error())
	}
	return err
}

func (r *Runner) setPhase(ctx context.Context, orderID, phase string, total int) {
	_, _ = r.DB.Pool.Exec(ctx, `UPDATE attribution_jobs SET status='RUNNING', phase=$2, total_runs=GREATEST(total_runs,$3), started_at=COALESCE(started_at, now()), error='' WHERE order_id=$1`, orderID, phase, total)
}

func (r *Runner) run(ctx context.Context, orderID string) error {
	var o orderInfo
	var decisionID *string
	err := r.DB.Pool.QueryRow(ctx, `SELECT id, decision_id, product_id, amount_cents, commission_rate_bps, sway_fee_bps, payment_mode FROM orders WHERE id=$1`, orderID).
		Scan(&o.ID, &decisionID, &o.ProductID, &o.Amount, &o.CommissionBps, &o.FeeBps, &o.PaymentMode)
	if err != nil {
		return fmt.Errorf("load order: %w", err)
	}
	if decisionID == nil {
		return errors.New("order has no agent decision to attribute")
	}
	o.DecisionID = *decisionID
	in, _, err := r.Shop.Load(ctx, r.DB.Pool, o.DecisionID)
	if err != nil {
		return err
	}
	k := r.Cfg.Samples
	target := o.ProductID
	var progress, hits atomic.Int64
	onRun := func(cached bool) {
		progress.Add(1)
		if cached {
			hits.Add(1)
		}
	}
	stop := r.flushProgress(ctx, orderID, &progress, &hits)
	defer stop()

	// 1. Screen: leave-one-out for every source, plus v(N) and v(∅).
	all := in.SourceIDs()
	r.setPhase(ctx, orderID, "screening", (len(all)+2)*k)
	type looRes struct {
		delta float64
		pick  string
	}
	loo := map[string]looRes{}
	var mu sync.Mutex
	var vFull, vNone agent.Value
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { vFull, err = r.Agent.Evaluate(gctx, in, all, target, k, onRun); return })
	g.Go(func() (err error) { vNone, err = r.Agent.Evaluate(gctx, in, nil, target, k, onRun); return })
	without := map[string]agent.Value{}
	for _, id := range all {
		g.Go(func() error {
			v, err := r.Agent.Evaluate(gctx, in, except(all, id), target, k, onRun)
			if err == nil {
				mu.Lock()
				without[id] = v
				mu.Unlock()
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for id, v := range without {
		loo[id] = looRes{vFull.Mean - v.Mean, agent.Majority(v.Picks)}
	}

	// 2. Players = top-P sources by |Δ|; the rest are background (always present, credited 0).
	ranked := append([]string(nil), all...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := math.Abs(loo[ranked[i]].delta), math.Abs(loo[ranked[j]].delta)
		if a != b {
			return a > b
		}
		return ranked[i] < ranked[j]
	})
	n := min(r.Cfg.Players, len(ranked))
	players := append([]string(nil), ranked[:n]...)
	sort.Strings(players)
	background := append([]string(nil), ranked[n:]...)
	sort.Strings(background)
	coalitionOf := func(mask uint32) []string {
		c := append([]string(nil), background...)
		for i, id := range players {
			if mask&(1<<i) != 0 {
				c = append(c, id)
			}
		}
		sort.Strings(c)
		return c
	}

	// 3. Budget guard: count uncached runs the exact method needs.
	masks := 1 << n
	needed := 0
	for m := 0; m < masks; m++ {
		cached, err := r.Agent.IsCached(ctx, in, coalitionOf(uint32(m)), k)
		if err != nil {
			return err
		}
		needed += k - cached
	}
	method := "exact"
	evalMasks := make([]uint32, 0, masks)
	if needed > r.Cfg.MaxCalls {
		method = "monte_carlo"
		seen := map[uint32]bool{}
		shapley.MonteCarlo(n, r.Cfg.MCPermutations, seedFor(orderID), func(m uint32) float64 {
			if !seen[m] {
				seen[m] = true
				evalMasks = append(evalMasks, m)
			}
			return 0
		})
	} else {
		for m := 0; m < masks; m++ {
			evalMasks = append(evalMasks, uint32(m))
		}
	}
	r.setPhase(ctx, orderID, "coalitions", int(progress.Load())+len(evalMasks)*k)

	// 4. Evaluate every needed coalition concurrently (the agent's semaphore caps real LLM concurrency).
	values := make(map[uint32]agent.Value, len(evalMasks))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(32)
	for _, m := range evalMasks {
		g.Go(func() error {
			v, err := r.Agent.Evaluate(gctx, in, coalitionOf(m), target, k, onRun)
			if err == nil {
				mu.Lock()
				values[m] = v
				mu.Unlock()
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// 5. Shapley values (+ uncertainty from per-coalition sample variance).
	var phi, phiStd []float64
	full := uint32(masks - 1)
	if method == "exact" {
		table := make(shapley.Values, masks)
		for m := range table {
			table[m] = values[uint32(m)].Mean
		}
		phi, err = shapley.Exact(n, table)
		if err != nil {
			return err
		}
		phiStd = exactStd(n, values, k)
	} else {
		phi, phiStd = shapley.MonteCarlo(n, r.Cfg.MCPermutations, seedFor(orderID), func(m uint32) float64 { return values[m].Mean })
	}
	vAll := vFull.Mean
	vBg := valueOr(values, 0, vNone.Mean)
	gap := shapley.EfficiencyGap(phi, valueOr(values, full, vAll), vBg)

	// 6. Influence share and allocation (spec 6.5 steps 3-4).
	share := 0.0
	if vAll > 0 && vAll-vNone.Mean >= r.Cfg.MinInfluence {
		share = math.Max(0, math.Min(1, (vAll-vNone.Mean)/vAll))
	}
	// Only statistically meaningful positive influence is paid: φ must clear a small noise floor.
	positive := 0.0
	for _, p := range phi {
		if p > MinPhi {
			positive += p
		}
	}
	if positive <= 0 {
		share = 0
	}
	split := money.ComputeSplit(o.Amount, o.CommissionBps, o.FeeBps, share)
	if err := split.Check(); err != nil {
		return err
	}
	weights := make([]float64, n)
	for i, p := range phi {
		if p > MinPhi {
			weights[i] = p / positive
		}
	}
	amounts := make([]int64, n)
	if split.CreatorPoolNet > 0 {
		if amounts, err = money.Allocate(split.CreatorPoolNet, weights); err != nil {
			return err
		}
	}

	srcByID := map[string]struct{ creator, name string }{}
	for _, s := range in.Sources {
		srcByID[s.ID] = struct{ creator, name string }{s.CreatorID, s.CreatorName}
	}
	var outPlayers []Player
	for i, id := range players {
		outPlayers = append(outPlayers, Player{SourceID: id, CreatorID: srcByID[id].creator, CreatorName: srcByID[id].name, Phi: phi[i], PhiStd: phiStd[i],
			LOO: loo[id].delta, PickWithout: loo[id].pick, Role: "player"})
	}
	for _, id := range background {
		outPlayers = append(outPlayers, Player{SourceID: id, CreatorID: srcByID[id].creator, CreatorName: srcByID[id].name, LOO: loo[id].delta, PickWithout: loo[id].pick, Role: "background"})
	}
	var coalitions []Coalition
	for _, m := range evalMasks {
		v := values[m]
		var members []string
		for i, id := range players {
			if m&(1<<i) != 0 {
				members = append(members, id)
			}
		}
		coalitions = append(coalitions, Coalition{Mask: m, Members: members, V: round4(v.Mean), Std: round4(math.Sqrt(v.Variance)), Pick: agent.Majority(v.Picks)})
	}
	sort.Slice(coalitions, func(i, j int) bool { return coalitions[i].Mask < coalitions[j].Mask })

	playersJSON, _ := json.Marshal(outPlayers)
	coalJSON, _ := json.Marshal(map[string]any{"players": players, "background": background, "coalitions": coalitions,
		"interactions": interactions(method, n, values), "v_all": round4(vAll), "v_none": round4(vNone.Mean), "v_background": round4(vBg)})
	simulated := o.PaymentMode != "sandbox"

	// 7. One transaction: attributions + ledger + job status. All or nothing.
	err = r.DB.Tx(ctx, func(q db.Q) error {
		if err := ledger.LockOrders(ctx, q, []string{orderID}); err != nil {
			return err
		}
		var done bool
		if err := q.QueryRow(ctx, `SELECT status='DONE' FROM attribution_jobs WHERE order_id=$1 FOR UPDATE`, orderID).Scan(&done); err != nil {
			return err
		}
		if done {
			return nil
		}
		entries := []ledger.Entry{{
			Type: ledger.CommissionReserved, OrderID: orderID, From: ledger.MerchantFunding, To: ledger.OrderCommission,
			Amount: split.Commission, IdemKey: ledger.Key("reserve", orderID), Memo: "commission reserved at capture", Simulated: simulated,
		}}
		for i, id := range players {
			role := "player"
			if _, err := q.Exec(ctx, `INSERT INTO attributions (id, order_id, source_id, creator_id, phi, phi_stddev, loo_delta, weight, amount_cents, role)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (order_id, source_id) DO NOTHING`,
				newID("atr_"), orderID, id, srcByID[id].creator, phi[i], phiStd[i], loo[id].delta, weights[i], amounts[i], role); err != nil {
				return err
			}
			entries = append(entries, ledger.Entry{
				Type: ledger.AccruePending, OrderID: orderID, CreatorID: srcByID[id].creator, From: ledger.OrderCommission, To: ledger.CreatorPending,
				Amount: amounts[i], IdemKey: ledger.Key("accrue", orderID, id), Ref: id, Memo: "influence share of creator pool", Simulated: simulated,
			})
		}
		for _, id := range background {
			if _, err := q.Exec(ctx, `INSERT INTO attributions (id, order_id, source_id, creator_id, phi, loo_delta, role)
				VALUES ($1,$2,$3,$4,0,$5,'background') ON CONFLICT (order_id, source_id) DO NOTHING`,
				newID("atr_"), orderID, id, srcByID[id].creator, loo[id].delta); err != nil {
				return err
			}
		}
		entries = append(entries,
			ledger.Entry{Type: ledger.SwayFeePending, OrderID: orderID, From: ledger.OrderCommission, To: ledger.SwayPending, Amount: split.SwayFee,
				IdemKey: ledger.Key("fee", orderID), Memo: "Sway fee on creator pool", Simulated: simulated},
			ledger.Entry{Type: ledger.ReturnToMerchant, OrderID: orderID, From: ledger.OrderCommission, To: ledger.MerchantReturned, Amount: split.ReturnedToMerchant,
				IdemKey: ledger.Key("return", orderID), Memo: "commission not earned by content", Simulated: simulated},
		)
		if _, err := ledger.Post(ctx, q, entries...); err != nil {
			return err
		}
		s, err := ledger.SummarizeOrder(ctx, q, orderID)
		if err != nil {
			return err
		}
		if !s.Balanced {
			return fmt.Errorf("%w: %+v", ledger.ErrUnbalanced, s)
		}
		_, err = q.Exec(ctx, `UPDATE attribution_jobs SET status='DONE', phase='done', progress=$2, cache_hits=$3, method=$4, approximate=$5,
			v_full=$6, v_none=$7, share=$8, efficiency_gap=$9, players=$10, coalitions=$11, finished_at=now(), error='' WHERE order_id=$1`,
			orderID, progress.Load(), hits.Load(), method, method != "exact", vAll, vNone.Mean, share, gap, playersJSON, coalJSON)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE decisions SET status='PURCHASED' WHERE id=$1`, o.DecisionID)
		return err
	})
	if err != nil {
		return err
	}
	if r.Log != nil {
		r.Log.Info("attribution done", "order", orderID, "method", method, "share", round4(share), "players", n, "runs", progress.Load(), "cache_hits", hits.Load(), "efficiency_gap", gap)
	}
	if r.AfterAllocate != nil {
		if err := r.AfterAllocate(ctx, orderID); err != nil {
			return fmt.Errorf("after allocate: %w", err)
		}
	}
	return r.explain(ctx, orderID, in, outPlayers)
}

func (r *Runner) flushProgress(ctx context.Context, orderID string, progress, hits *atomic.Int64) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		last := int64(-1)
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if p := progress.Load(); p != last {
					last = p
					_, _ = r.DB.Pool.Exec(ctx, `UPDATE attribution_jobs SET progress=$2, cache_hits=$3 WHERE order_id=$1 AND status='RUNNING'`, orderID, p, hits.Load())
				}
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

func (r *Runner) explain(ctx context.Context, orderID string, in agent.Input, players []Player) error {
	var purchased, rationale string
	if err := r.DB.Pool.QueryRow(ctx, `SELECT p.name, d.rationale FROM orders o JOIN products p ON p.id=o.product_id JOIN decisions d ON d.id=o.decision_id WHERE o.id=$1`, orderID).
		Scan(&purchased, &rationale); err != nil {
		return err
	}
	names := map[string]string{}
	for _, p := range in.Candidates {
		names[p.ID] = p.Name
	}
	srcs := map[string]int{}
	for i, s := range in.Sources {
		srcs[s.ID] = i
	}
	for _, p := range players {
		var has bool
		if err := r.DB.Pool.QueryRow(ctx, `SELECT explanation IS NOT NULL FROM attributions WHERE order_id=$1 AND source_id=$2`, orderID, p.SourceID).Scan(&has); err != nil || has {
			continue
		}
		src := in.Sources[srcs[p.SourceID]]
		effect := "Without it, the agent was less likely to choose this product."
		if p.PickWithout != "" && names[p.PickWithout] != purchased {
			effect = fmt.Sprintf("Without it, the agent would have picked the %s instead.", names[p.PickWithout])
		}
		var aspects []string
		seen := map[string]bool{}
		for _, c := range agent.ExtractClaims(src, in.Candidates) {
			if c.Strength > 0 && names[c.ProductID] == purchased && !seen[c.Aspect] {
				seen[c.Aspect] = true
				aspects = append(aspects, c.Aspect)
			}
		}
		sort.Strings(aspects)
		ei := agent.ExplainInput{Request: in.Request, Purchased: purchased, Rationale: rationale, SourceTitle: src.Title, SourceBody: src.Body,
			RemovalEffect: effect, CreatorName: p.CreatorName, Positive: p.Phi > 0, Aspects: aspects}
		var text string
		switch {
		case p.Role == "background":
			text = "Your content was available to the agent but had little effect on this decision compared with other sources, so it was not one of the sources credited."
		case math.Abs(p.Phi) <= MinPhi:
			text = "The agent read your content, but it did not move this decision either way, so it earns nothing on this order. Sources are paid only for influence that changes what gets bought."
		case p.Phi < 0:
			text = agent.SimExplain(ei)
		case r.Explainer != nil:
			t, err := r.Explainer.Explain(ctx, ei)
			if err != nil {
				if r.Log != nil {
					r.Log.Warn("explanation failed, using template", "err", err)
				}
				t = agent.SimExplain(ei)
			}
			text = t
		default:
			text = agent.SimExplain(ei)
		}
		if _, err := r.DB.Pool.Exec(ctx, `UPDATE attributions SET explanation=$3 WHERE order_id=$1 AND source_id=$2 AND explanation IS NULL`, orderID, p.SourceID, text); err != nil {
			return err
		}
	}
	return nil
}

// exactStd propagates per-coalition sampling variance into φ: Var(φ_i) ≈ Σ_S w² (Var v(S∪i) + Var v(S)) / K.
func exactStd(n int, values map[uint32]agent.Value, k int) []float64 {
	out := make([]float64, n)
	w := make([]float64, n)
	for s := 0; s < n; s++ {
		lg1, _ := math.Lgamma(float64(s + 1))
		lg2, _ := math.Lgamma(float64(n - s))
		lg3, _ := math.Lgamma(float64(n + 1))
		w[s] = math.Exp(lg1 + lg2 - lg3)
	}
	for i := 0; i < n; i++ {
		bit := uint32(1) << i
		v := 0.0
		for m := uint32(0); m < 1<<n; m++ {
			if m&bit != 0 {
				continue
			}
			ws := w[shapley.Popcount(m)]
			v += ws * ws * (values[m|bit].Variance + values[m].Variance) / float64(k)
		}
		out[i] = math.Sqrt(v)
	}
	return out
}

func interactions(method string, n int, values map[uint32]agent.Value) [][]float64 {
	if method != "exact" || n < 2 {
		return nil
	}
	t := make(shapley.Values, 1<<n)
	for m := range t {
		t[m] = values[uint32(m)].Mean
	}
	out := shapley.Interactions(n, t)
	for i := range out {
		for j := range out[i] {
			out[i][j] = round4(out[i][j])
		}
	}
	return out
}

func valueOr(m map[uint32]agent.Value, k uint32, def float64) float64 {
	if v, ok := m[k]; ok {
		return v.Mean
	}
	return def
}

func except(all []string, id string) []string {
	out := make([]string, 0, len(all))
	for _, x := range all {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func seedFor(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

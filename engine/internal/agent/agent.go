package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/vivekyarra/sway/engine/internal/db"
)

// Agent runs decisions through an Engine with a persistent cache, in-flight de-duplication and a
// concurrency limit shared by every caller in the process (shopper requests and attribution jobs).
type Agent struct {
	DB     *db.DB
	Engine Engine
	Tau    float64
	sem    chan struct{}
	group  singleflight.Group

	Calls     atomic.Int64 // real engine calls
	CacheHits atomic.Int64
}

func New(d *db.DB, e Engine, concurrency int, tau float64) *Agent {
	if concurrency < 1 {
		concurrency = 1
	}
	if tau <= 0 {
		tau = 10
	}
	return &Agent{DB: d, Engine: e, Tau: tau, sem: make(chan struct{}, concurrency)}
}

// RunResult is one (possibly cached) decision.
type RunResult struct {
	Output
	Cached    bool  `json:"cached"`
	LatencyMS int64 `json:"latency_ms"`
}

// Run executes (or recalls) one decision for the given input and sample index.
func (a *Agent) Run(ctx context.Context, in Input, sample int) (RunResult, error) {
	in = in.Normalize()
	key := CacheKey(a.Engine.Name(), a.Engine.Model(), in, sample)
	if r, ok, err := a.lookup(ctx, key); err != nil {
		return RunResult{}, err
	} else if ok {
		a.CacheHits.Add(1)
		return r, nil
	}
	v, err, _ := a.group.Do(key, func() (any, error) {
		if r, ok, err := a.lookup(ctx, key); err == nil && ok {
			a.CacheHits.Add(1)
			return r, nil
		}
		select {
		case a.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		start := time.Now()
		out, err := a.Engine.Decide(ctx, in, sample)
		<-a.sem
		if err != nil {
			return nil, err
		}
		a.Calls.Add(1)
		r := RunResult{Output: out, LatencyMS: time.Since(start).Milliseconds()}
		if err := a.store(ctx, key, in, sample, r); err != nil {
			return nil, err
		}
		return r, nil
	})
	if err != nil {
		return RunResult{}, err
	}
	return v.(RunResult), nil
}

func (a *Agent) lookup(ctx context.Context, key string) (RunResult, bool, error) {
	var scores, relied []byte
	var r RunResult
	err := a.DB.Pool.QueryRow(ctx, `SELECT scores, chosen_product_id, rationale, relied_on_ids, latency_ms FROM llm_runs WHERE cache_key=$1`, key).
		Scan(&scores, &r.Chosen, &r.Rationale, &relied, &r.LatencyMS)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, false, nil
		}
		return r, false, err
	}
	if err := json.Unmarshal(scores, &r.Scores); err != nil {
		return r, false, err
	}
	_ = json.Unmarshal(relied, &r.ReliedOn)
	r.Cached = true
	return r, true, nil
}

func (a *Agent) store(ctx context.Context, key string, in Input, sample int, r RunResult) error {
	scores, _ := json.Marshal(r.Scores)
	relied, _ := json.Marshal(r.ReliedOn)
	coalition, _ := json.Marshal(in.SourceIDs())
	_, err := a.DB.Pool.Exec(ctx, `INSERT INTO llm_runs (cache_key, model, prompt_version, coalition_source_ids, sample_index, scores, chosen_product_id, rationale, relied_on_ids, latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (cache_key) DO NOTHING`,
		key, a.Engine.Model(), PromptVersion, coalition, sample, scores, r.Chosen, r.Rationale, relied, r.LatencyMS)
	return err
}

// Value is v(S) for one coalition: the mean (over K samples) probability of picking target.
type Value struct {
	Mean     float64        `json:"mean"`
	Variance float64        `json:"variance"`
	Samples  []float64      `json:"samples"`
	Picks    map[string]int `json:"picks"` // chosen product counts across samples
	Probs    map[string]float64
	First    RunResult `json:"-"`
	Calls    int       `json:"calls"` // uncached engine calls made for this value
}

// Evaluate computes v(S) for the coalition of source ids against target.
func (a *Agent) Evaluate(ctx context.Context, in Input, coalition []string, target string, k int, onRun func(cached bool)) (Value, error) {
	sub := in.WithSources(coalition)
	results := make([]RunResult, k)
	g, gctx := errgroup.WithContext(ctx)
	for s := 0; s < k; s++ {
		g.Go(func() error {
			r, err := a.Run(gctx, sub, s)
			if err != nil {
				return err
			}
			results[s] = r
			if onRun != nil {
				onRun(r.Cached)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Value{}, err
	}
	v := Value{Picks: map[string]int{}, Probs: map[string]float64{}, First: results[0]}
	for _, r := range results {
		p := Probabilities(r.Scores, a.Tau)
		v.Samples = append(v.Samples, p[target])
		v.Picks[r.Chosen]++
		for id, x := range p {
			v.Probs[id] += x / float64(k)
		}
		if !r.Cached {
			v.Calls++
		}
	}
	for _, x := range v.Samples {
		v.Mean += x
	}
	v.Mean /= float64(k)
	if k > 1 {
		for _, x := range v.Samples {
			v.Variance += (x - v.Mean) * (x - v.Mean)
		}
		v.Variance /= float64(k - 1)
	}
	return v, nil
}

// IsCached reports whether all K samples of a coalition are already cached (used for budget planning).
func (a *Agent) IsCached(ctx context.Context, in Input, coalition []string, k int) (int, error) {
	sub := in.WithSources(coalition)
	keys := make([]string, k)
	for s := 0; s < k; s++ {
		keys[s] = CacheKey(a.Engine.Name(), a.Engine.Model(), sub, s)
	}
	var n int
	err := a.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM llm_runs WHERE cache_key = ANY($1)`, keys).Scan(&n)
	return n, err
}

// LeaveOneOut is the quick-influence estimate.
type LeaveOneOut struct {
	VFull  float64            `json:"v_full"`
	Deltas map[string]float64 `json:"deltas"`          // v(N) - v(N \ {i})
	Picks  map[string]string  `json:"picks_without"`   // what the agent picks without source i
	VNone  float64            `json:"v_none"`          // v(∅)
	PickNo string             `json:"pick_no_sources"` // what it picks with no sources
}

// QuickInfluence computes leave-one-out deltas for every source concurrently.
func (a *Agent) QuickInfluence(ctx context.Context, in Input, target string, k int) (LeaveOneOut, error) {
	all := in.SourceIDs()
	res := LeaveOneOut{Deltas: map[string]float64{}, Picks: map[string]string{}}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	var full Value
	g.Go(func() error {
		v, err := a.Evaluate(gctx, in, all, target, k, nil)
		full = v
		return err
	})
	g.Go(func() error {
		v, err := a.Evaluate(gctx, in, nil, target, k, nil)
		if err == nil {
			mu.Lock()
			res.VNone, res.PickNo = v.Mean, majority(v.Picks)
			mu.Unlock()
		}
		return err
	})
	without := map[string]Value{}
	for _, id := range all {
		g.Go(func() error {
			rest := make([]string, 0, len(all)-1)
			for _, x := range all {
				if x != id {
					rest = append(rest, x)
				}
			}
			v, err := a.Evaluate(gctx, in, rest, target, k, nil)
			if err != nil {
				return err
			}
			mu.Lock()
			without[id] = v
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return res, err
	}
	res.VFull = full.Mean
	for id, v := range without {
		res.Deltas[id] = round6(full.Mean - v.Mean)
		res.Picks[id] = majority(v.Picks)
	}
	return res, nil
}

func majority(picks map[string]int) string {
	best, n := "", -1
	for id, c := range picks {
		if c > n || (c == n && id < best) {
			best, n = id, c
		}
	}
	return best
}

// Majority is exported for callers that aggregate picks.
func Majority(picks map[string]int) string { return majority(picks) }

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// Describe is a short human label for logs.
func (a *Agent) Describe() string { return fmt.Sprintf("%s/%s", a.Engine.Name(), a.Engine.Model()) }

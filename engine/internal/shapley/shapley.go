// Package shapley computes Shapley values over coalitions represented as bitmasks.
//
// Players are indexed 0..n-1; a coalition is a uint32 mask. The value function is supplied
// by the caller (in Sway: the probability the agent picks the purchased product when it
// sees only that coalition of sources plus the always-present background sources).
package shapley

import (
	"errors"
	"math"
	"math/rand/v2"
)

// MaxExactPlayers bounds exact enumeration (2^n coalitions).
const MaxExactPlayers = 16

// Values maps every coalition mask (0 .. 2^n-1) to v(S).
type Values []float64

// Exact computes φ_i = Σ_{S ⊆ N\{i}} |S|!(n-|S|-1)!/n! · (v(S∪{i}) − v(S)).
func Exact(n int, v Values) ([]float64, error) {
	if n < 0 || n > MaxExactPlayers {
		return nil, errors.New("shapley: player count out of range")
	}
	if len(v) != 1<<n {
		return nil, errors.New("shapley: value table must have 2^n entries")
	}
	weights := make([]float64, n) // weight by |S|
	for s := 0; s < n; s++ {
		weights[s] = math.Exp(lgamma(s+1) + lgamma(n-s) - lgamma(n+1))
	}
	phi := make([]float64, n)
	for i := 0; i < n; i++ {
		bit := uint32(1) << i
		for mask := uint32(0); mask < uint32(1)<<n; mask++ {
			if mask&bit != 0 {
				continue
			}
			phi[i] += weights[popcount(mask)] * (v[mask|bit] - v[mask])
		}
	}
	return phi, nil
}

// MonteCarlo estimates φ by averaging marginal contributions over random permutations.
// value is called lazily, so callers can back it with a cache and count real evaluations.
// Returns the estimates and the per-player standard error.
func MonteCarlo(n, permutations int, key uint64, value func(mask uint32) float64) (phi, stderr []float64) {
	phi = make([]float64, n)
	sq := make([]float64, n)
	if n == 0 || permutations <= 0 {
		return phi, make([]float64, n)
	}
	r := rand.New(rand.NewPCG(key, key^0x9e3779b97f4a7c15))
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	for p := 0; p < permutations; p++ {
		// Antithetic pairs: every odd permutation is the reverse of the previous one (variance reduction).
		if p%2 == 0 {
			r.Shuffle(n, func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		} else {
			for a, b := 0, n-1; a < b; a, b = a+1, b-1 {
				perm[a], perm[b] = perm[b], perm[a]
			}
		}
		var mask uint32
		prev := value(0)
		for _, i := range perm {
			mask |= 1 << i
			cur := value(mask)
			d := cur - prev
			phi[i] += d
			sq[i] += d * d
			prev = cur
		}
	}
	stderr = make([]float64, n)
	k := float64(permutations)
	for i := range phi {
		mean := phi[i] / k
		variance := math.Max(0, sq[i]/k-mean*mean)
		phi[i] = mean
		if permutations > 1 {
			stderr[i] = math.Sqrt(variance / (k - 1))
		}
	}
	return phi, stderr
}

// EfficiencyGap returns Σφ − (v(N) − v(∅)); it is ~0 for exact Shapley values.
func EfficiencyGap(phi []float64, vFull, vEmpty float64) float64 {
	sum := 0.0
	for _, p := range phi {
		sum += p
	}
	return sum - (vFull - vEmpty)
}

// Interactions returns the pairwise Shapley interaction index I_ij for every pair (used by the coalition explorer
// to show which sources reinforce or cancel each other).
func Interactions(n int, v Values) [][]float64 {
	out := make([][]float64, n)
	for i := range out {
		out[i] = make([]float64, n)
	}
	if n < 2 {
		return out
	}
	weights := make([]float64, n-1)
	for s := 0; s <= n-2; s++ {
		weights[s] = math.Exp(lgamma(s+1) + lgamma(n-s-1) - lgamma(n))
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			bi, bj := uint32(1)<<i, uint32(1)<<j
			total := 0.0
			for mask := uint32(0); mask < uint32(1)<<n; mask++ {
				if mask&(bi|bj) != 0 {
					continue
				}
				total += weights[popcount(mask)] * (v[mask|bi|bj] - v[mask|bi] - v[mask|bj] + v[mask])
			}
			out[i][j], out[j][i] = total, total
		}
	}
	return out
}

func popcount(x uint32) int {
	c := 0
	for x != 0 {
		x &= x - 1
		c++
	}
	return c
}

// Popcount is exported for callers that build value tables.
func Popcount(x uint32) int { return popcount(x) }

func lgamma(x int) float64 {
	v, _ := math.Lgamma(float64(x))
	return v
}

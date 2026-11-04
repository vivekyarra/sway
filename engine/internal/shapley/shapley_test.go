package shapley

import (
	"math"
	"math/rand/v2"
	"testing"
)

const eps = 1e-9

func table(n int, f func(mask uint32) float64) Values {
	v := make(Values, 1<<n)
	for m := range v {
		v[m] = f(uint32(m))
	}
	return v
}

// Spec 13.1 #1: additive game -> φ_i equals its weight.
func TestAdditiveGame(t *testing.T) {
	w := []float64{0.1, 0.25, -0.05, 0, 0.4, 0.3}
	v := table(len(w), func(m uint32) float64 {
		s := 0.0
		for i := range w {
			if m&(1<<i) != 0 {
				s += w[i]
			}
		}
		return s
	})
	phi, err := Exact(len(w), v)
	if err != nil {
		t.Fatal(err)
	}
	for i := range w {
		if math.Abs(phi[i]-w[i]) > eps {
			t.Fatalf("φ_%d = %v want %v", i, phi[i], w[i])
		}
	}
}

func randomGame(r *rand.Rand, n int) Values {
	return table(n, func(m uint32) float64 {
		if m == 0 {
			return 0.2
		}
		return r.Float64()
	})
}

func TestEfficiencyRandomGames(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for trial := 0; trial < 200; trial++ {
		n := 1 + r.IntN(8)
		v := randomGame(r, n)
		phi, _ := Exact(n, v)
		if g := EfficiencyGap(phi, v[len(v)-1], v[0]); math.Abs(g) > 1e-9 {
			t.Fatalf("efficiency gap %v for n=%d", g, n)
		}
	}
}

func TestSymmetryAndNullPlayer(t *testing.T) {
	// Players 0 and 1 are interchangeable; player 2 never changes anything (null); 3 is a complement of 0/1.
	v := table(4, func(m uint32) float64 {
		a, b, d := m&1 != 0, m&2 != 0, m&8 != 0
		val := 0.1
		if a || b {
			val += 0.3
		}
		if (a || b) && d {
			val += 0.2
		}
		return val
	})
	phi, _ := Exact(4, v)
	if math.Abs(phi[0]-phi[1]) > eps {
		t.Fatalf("symmetry violated: %v vs %v", phi[0], phi[1])
	}
	if math.Abs(phi[2]) > eps {
		t.Fatalf("null player got %v", phi[2])
	}
	if g := EfficiencyGap(phi, v[15], v[0]); math.Abs(g) > eps {
		t.Fatalf("efficiency gap %v", g)
	}
}

func TestMonteCarloConvergesToExact(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	n := 6
	v := randomGame(r, n)
	exact, _ := Exact(n, v)
	calls := 0
	mc, se := MonteCarlo(n, 4000, 42, func(m uint32) float64 { calls++; return v[m] })
	for i := range exact {
		if math.Abs(mc[i]-exact[i]) > 0.03 {
			t.Fatalf("MC φ_%d=%v exact=%v", i, mc[i], exact[i])
		}
		if se[i] <= 0 {
			t.Fatalf("expected positive stderr")
		}
	}
	// Every permutation's marginal contributions telescope to v(N) - v(∅), so MC is efficient exactly.
	if g := EfficiencyGap(mc, v[len(v)-1], v[0]); math.Abs(g) > 1e-9 {
		t.Fatalf("MC efficiency gap %v", g)
	}
	if calls == 0 {
		t.Fatal("value function never called")
	}
}

func TestInteractionsDetectComplements(t *testing.T) {
	// v = 1 only when both players present: pure complementarity -> positive interaction.
	v := table(2, func(m uint32) float64 {
		if m == 3 {
			return 1
		}
		return 0
	})
	I := Interactions(2, v)
	if I[0][1] <= 0 || I[0][1] != I[1][0] {
		t.Fatalf("expected positive symmetric interaction, got %v", I)
	}
}

func TestValidation(t *testing.T) {
	if _, err := Exact(3, make(Values, 7)); err == nil {
		t.Fatal("expected size error")
	}
	if _, err := Exact(17, nil); err == nil {
		t.Fatal("expected range error")
	}
}

func BenchmarkExact10(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))
	v := randomGame(r, 10)
	for i := 0; i < b.N; i++ {
		_, _ = Exact(10, v)
	}
}

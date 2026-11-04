package money

import (
	"math/rand/v2"
	"testing"
)

func TestFormatParseRoundTrip(t *testing.T) {
	for _, c := range []Cents{0, 1, 9, 10, 99, 100, 22900, 1234567, -505} {
		got, err := Parse(Format(c))
		if err != nil || got != c {
			t.Fatalf("round trip %d -> %q -> %d (%v)", c, Format(c), got, err)
		}
	}
	for _, in := range []string{"229", "229.0", "229.00"} {
		if c, _ := Parse(in); c != 22900 {
			t.Fatalf("Parse(%q) = %d", in, c)
		}
	}
	for _, bad := range []string{"", "1.234", "abc", "1.x"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) should fail", bad)
		}
	}
}

func TestMulBpsRounding(t *testing.T) {
	cases := []struct {
		amt  Cents
		bps  int
		want Cents
	}{
		{22900, 800, 1832}, {19900, 800, 1592}, {1832, 1500, 275}, // 274.8 -> 275
		{1, 5000, 1}, {3, 5000, 2}, {0, 800, 0}, {100, 0, 0}, {100, 10000, 100},
	}
	for _, c := range cases {
		if got := MulBps(c.amt, c.bps); got != c.want {
			t.Fatalf("MulBps(%d,%d)=%d want %d", c.amt, c.bps, got, c.want)
		}
	}
}

// Spec 13.1 #2: 1,000 random splits; parts always sum exactly to the whole; no negative cents.
func TestAllocateRandomSumsExactly(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 1000; i++ {
		total := Cents(r.IntN(1_000_000))
		n := 1 + r.IntN(12)
		w := make([]float64, n)
		positive := false
		for j := range w {
			switch r.IntN(5) {
			case 0:
				w[j] = 0
			case 1:
				w[j] = -r.Float64()
			default:
				w[j] = r.Float64() * float64(1+r.IntN(1000))
				positive = true
			}
		}
		parts, err := Allocate(total, w)
		if !positive {
			if total > 0 && err == nil {
				t.Fatalf("expected error for no positive weights")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var sum Cents
		for j, p := range parts {
			if p < 0 {
				t.Fatalf("negative part %d", p)
			}
			if w[j] <= 0 && p != 0 {
				t.Fatalf("non-positive weight got %d", p)
			}
			sum += p
		}
		if sum != total {
			t.Fatalf("sum %d != total %d (weights %v)", sum, total, w)
		}
	}
}

func TestAllocateDeterministicTies(t *testing.T) {
	parts, _ := Allocate(10, []float64{1, 1, 1})
	if parts[0] != 4 || parts[1] != 3 || parts[2] != 3 {
		t.Fatalf("tie-break should favour lowest index, got %v", parts)
	}
}

func TestSplitInvariantExhaustive(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		total := Cents(1 + r.IntN(2_000_000))
		s := ComputeSplit(total, r.IntN(10001), r.IntN(10001), r.Float64()*1.2-0.1)
		if err := s.Check(); err != nil {
			t.Fatalf("%+v: %v", s, err)
		}
	}
	s := ComputeSplit(22900, 800, 1500, 1)
	if s.Commission != 1832 || s.SwayFee != 275 || s.CreatorPoolNet != 1557 || s.ReturnedToMerchant != 0 {
		t.Fatalf("unexpected split %+v", s)
	}
	s = ComputeSplit(22900, 800, 1500, 0)
	if s.ReturnedToMerchant != 1832 || s.CreatorPoolNet != 0 {
		t.Fatalf("zero share must return all commission: %+v", s)
	}
}

func FuzzAllocate(f *testing.F) {
	f.Add(int64(1000), 1.0, 2.0, 3.0)
	f.Fuzz(func(t *testing.T, total int64, a, b, c float64) {
		if total < 0 || total > 1<<40 {
			return
		}
		parts, err := Allocate(total, []float64{a, b, c})
		if err != nil {
			return
		}
		var sum int64
		for _, p := range parts {
			if p < 0 {
				t.Fatal("negative")
			}
			sum += p
		}
		if sum != total {
			t.Fatalf("sum %d != %d", sum, total)
		}
	})
}

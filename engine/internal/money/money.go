// Package money does all arithmetic on integer cents. Floats never touch an amount
// except as allocation weights, and every split sums exactly to its whole.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Cents is an amount in USD cents.
type Cents = int64

// Format renders cents as "12.34" (PayPal wire format, no currency symbol).
func Format(c Cents) string {
	sign := ""
	if c < 0 {
		sign = "-"
		c = -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// Parse reads a PayPal "12.34" value into cents exactly (no float rounding).
func Parse(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("money: empty amount")
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("money: too many decimals in %q", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad amount %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad amount %q", s)
	}
	c := w*100 + f
	if neg {
		c = -c
	}
	return c, nil
}

// MulBps returns round-half-up(amount * bps / 10000) using exact integer math.
func MulBps(amount Cents, bps int) Cents {
	return MulRatio(amount, int64(bps), 10000)
}

// MulRatio returns round-half-up(amount * num / den) exactly, for non-negative inputs.
func MulRatio(amount, num, den int64) Cents {
	if den <= 0 {
		panic("money: non-positive denominator")
	}
	if amount < 0 || num < 0 {
		panic("money: negative input to MulRatio")
	}
	p := new(big.Int).Mul(big.NewInt(amount), big.NewInt(num))
	p.Mul(p, big.NewInt(2))
	p.Add(p, big.NewInt(den))
	p.Quo(p, big.NewInt(2*den))
	return p.Int64()
}

// MulFraction returns round-half-up(amount * f) for f in [0,1].
// f is snapped to a 1e-9 grid first so tiny float noise can't move a cent.
func MulFraction(amount Cents, f float64) Cents {
	if math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f >= 1 {
		return amount
	}
	const grid = 1_000_000_000
	return MulRatio(amount, int64(math.Round(f*grid)), grid)
}

// Allocate splits total across weights with the largest remainder (Hamilton) method:
// parts are non-negative, sum exactly to total, and ties break by lowest index (deterministic).
// Zero or negative weights receive nothing. If every weight is zero, everything goes to nothing
// and an error is returned so callers decide what "unallocated" means.
func Allocate(total Cents, weights []float64) ([]Cents, error) {
	parts := make([]Cents, len(weights))
	if total < 0 {
		return nil, errors.New("money: negative total")
	}
	if total == 0 {
		return parts, nil
	}
	sum := 0.0
	for _, w := range weights {
		if w > 0 && !math.IsInf(w, 0) && !math.IsNaN(w) {
			sum += w
		}
	}
	if sum <= 0 {
		return nil, errors.New("money: no positive weights")
	}
	type rem struct {
		i int
		r float64
	}
	rems := make([]rem, 0, len(weights))
	var assigned Cents
	for i, w := range weights {
		if !(w > 0) || math.IsInf(w, 0) {
			continue
		}
		exact := float64(total) * (w / sum)
		floor := math.Floor(exact)
		parts[i] = Cents(floor)
		assigned += parts[i]
		rems = append(rems, rem{i, exact - floor})
	}
	sort.SliceStable(rems, func(a, b int) bool {
		if rems[a].r != rems[b].r {
			return rems[a].r > rems[b].r
		}
		return rems[a].i < rems[b].i
	})
	left := total - assigned
	// Float error can make left slightly out of [0, len(rems)]; correct defensively.
	for k := 0; left > 0; k = (k + 1) % len(rems) {
		parts[rems[k].i]++
		left--
	}
	for k := len(rems) - 1; left < 0; k-- {
		if k < 0 {
			k = len(rems) - 1
		}
		if parts[rems[k].i] > 0 {
			parts[rems[k].i]--
			left++
		}
	}
	return parts, nil
}

// Split is the per-order commission breakdown. Invariant: CreatorPoolNet + SwayFee + ReturnedToMerchant == Commission.
type Split struct {
	OrderTotal         Cents `json:"order_total_cents"`
	Commission         Cents `json:"commission_cents"`
	CreatorPoolGross   Cents `json:"creator_pool_gross_cents"`
	SwayFee            Cents `json:"sway_fee_cents"`
	CreatorPoolNet     Cents `json:"creator_pool_net_cents"`
	ReturnedToMerchant Cents `json:"returned_to_merchant_cents"`
}

// ComputeSplit applies Section 7.1 of the spec.
func ComputeSplit(orderTotal Cents, commissionBps, feeBps int, share float64) Split {
	s := Split{OrderTotal: orderTotal}
	s.Commission = MulBps(orderTotal, commissionBps)
	s.CreatorPoolGross = MulFraction(s.Commission, share)
	s.SwayFee = MulBps(s.CreatorPoolGross, feeBps)
	s.CreatorPoolNet = s.CreatorPoolGross - s.SwayFee
	s.ReturnedToMerchant = s.Commission - s.CreatorPoolGross
	return s
}

// Check verifies the split invariant.
func (s Split) Check() error {
	if s.CreatorPoolNet+s.SwayFee+s.ReturnedToMerchant != s.Commission {
		return fmt.Errorf("money: split does not balance: %d + %d + %d != %d", s.CreatorPoolNet, s.SwayFee, s.ReturnedToMerchant, s.Commission)
	}
	if s.CreatorPoolNet < 0 || s.SwayFee < 0 || s.ReturnedToMerchant < 0 {
		return errors.New("money: negative component in split")
	}
	return nil
}

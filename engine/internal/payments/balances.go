package payments

import (
	"context"

	"github.com/vivekyarra/sway/engine/internal/db"
	"github.com/vivekyarra/sway/engine/internal/ledger"
)

// CreatorBalances are the five big numbers on a creator dashboard, all derived from the ledger.
type CreatorBalances struct {
	Pending    int64 `json:"pending_cents"`
	Frozen     int64 `json:"frozen_cents"`
	Vested     int64 `json:"vested_cents"`
	Paid       int64 `json:"paid_cents"`
	Forfeited  int64 `json:"forfeited_cents"`
	ClawedBack int64 `json:"clawed_back_cents"`
	Owed       int64 `json:"owed_cents"`
	Earned     int64 `json:"earned_cents"` // lifetime accruals
}

// Balances derives a creator's balances.
func Balances(ctx context.Context, q db.Q, creatorID string) (CreatorBalances, error) {
	b, err := ledger.BalancesFor(ctx, q, ledger.Scope{CreatorID: creatorID})
	if err != nil {
		return CreatorBalances{}, err
	}
	out := CreatorBalances{
		Pending: b[ledger.CreatorPending], Frozen: b[ledger.CreatorFrozen], Vested: b[ledger.CreatorVested], Paid: b[ledger.CreatorPaid],
	}
	if o := b[ledger.CreatorOwed]; o < 0 {
		out.Owed = -o
	}
	err = q.QueryRow(ctx, `SELECT
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$2),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$3),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$4),0)::bigint
		FROM ledger_entries WHERE creator_id=$1`, creatorID, ledger.Forfeit, ledger.Clawback, ledger.AccruePending).Scan(&out.Forfeited, &out.ClawedBack, &out.Earned)
	return out, err
}

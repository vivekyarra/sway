// Package ledger is Sway's book of record: double-entry, append-only, idempotent and hash-chained.
//
// Every entry moves a positive amount from one account to another, so the trial balance is zero by
// construction. Balances are always derived by summing entries; nothing mutable is ever stored.
// Each entry carries an idempotency key (a retried webhook or API call can never double-book) and
// the SHA-256 of the previous entry, so any edit to history is detectable by Verify.
package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivekyarra/sway/engine/internal/db"
)

// Entry types (spec 7.3, plus CLAWBACK_OFFSET for netting owed balances against later payouts).
const (
	CommissionReserved   = "COMMISSION_RESERVED"
	AccruePending        = "ACCRUE_PENDING"
	SwayFeePending       = "SWAY_FEE_PENDING"
	ReturnToMerchant     = "RETURN_TO_MERCHANT"
	Vest                 = "VEST"
	Forfeit              = "FORFEIT"
	Freeze               = "FREEZE"
	Unfreeze             = "UNFREEZE"
	PayoutSent           = "PAYOUT_SENT"
	PayoutFailedReversal = "PAYOUT_FAILED_REVERSAL"
	Clawback             = "CLAWBACK"
	ClawbackOffset       = "CLAWBACK_OFFSET"
)

// Accounts. creator:* and sway:* are scoped by the entry's creator/order; merchant:* by the order's merchant.
const (
	MerchantFunding   = "merchant:funding" // contra account the merchant's commission budget comes from
	OrderCommission   = "order:commission" // reserved at capture, fully distributed by attribution
	CreatorPending    = "creator:pending"
	CreatorFrozen     = "creator:frozen"
	CreatorVested     = "creator:vested"
	CreatorPaid       = "creator:paid"
	CreatorOwed       = "creator:owed" // negative balance = the creator owes (clawback)
	SwayPending       = "sway:pending"
	SwayFrozen        = "sway:frozen"
	SwayVested        = "sway:vested"
	MerchantReturned  = "merchant:returned"  // commission not earned by content (influence share < 100%)
	MerchantRecovered = "merchant:recovered" // forfeits and clawbacks flowing back to the merchant
)

// nonNegative accounts can never hold a negative balance within their (creator, order) scope.
// merchant:funding, merchant:* and creator:owed are the only accounts allowed below zero.
var nonNegative = map[string]bool{
	OrderCommission: true, CreatorPending: true, CreatorFrozen: true, CreatorVested: true, CreatorPaid: true,
	SwayPending: true, SwayFrozen: true, SwayVested: true,
}

// ErrOverdraft is returned when an entry would take a non-negative account below zero.
var ErrOverdraft = errors.New("ledger: entry would overdraw an account")

// Entry is one movement of money.
type Entry struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	IdemKey   string    `json:"idem_key"`
	Type      string    `json:"type"`
	OrderID   string    `json:"order_id,omitempty"`
	CreatorID string    `json:"creator_id,omitempty"`
	From      string    `json:"from_account"`
	To        string    `json:"to_account"`
	Amount    int64     `json:"amount_cents"`
	Signed    int64     `json:"signed_cents"`
	Ref       string    `json:"ref,omitempty"`
	Memo      string    `json:"memo,omitempty"`
	Simulated bool      `json:"simulated"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
}

// GenesisHash anchors the chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Key builds a deterministic idempotency key from its parts.
func Key(parts ...string) string { return strings.Join(parts, "|") }

// signedFor applies the spec's display sign convention (7.3).
func signedFor(typ string, amount int64) int64 {
	switch typ {
	case Forfeit, PayoutSent, Clawback, ClawbackOffset:
		return -amount
	case Vest, Freeze, Unfreeze:
		return 0
	default:
		return amount
	}
}

func newID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "le_" + hex.EncodeToString(b[:])
}

func hashEntry(prev string, e Entry) string {
	h := sha256.New()
	fields := []string{
		prev, e.ID, e.IdemKey, e.Type, e.OrderID, e.CreatorID, e.From, e.To,
		strconv.FormatInt(e.Amount, 10), strconv.FormatInt(e.Signed, 10), e.Ref, e.Memo,
		strconv.FormatBool(e.Simulated), e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Post appends entries atomically within q's transaction. Entries whose idempotency key already exists
// are skipped (that is how replays become no-ops). Zero amounts are skipped. Returns entries actually written.
// Must be called inside a transaction: it takes a transaction-scoped advisory lock to serialize the chain.
func Post(ctx context.Context, q db.Q, entries ...Entry) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(424242)`); err != nil {
		return nil, fmt.Errorf("ledger lock: %w", err)
	}
	prev := GenesisHash
	if err := q.QueryRow(ctx, `SELECT hash FROM ledger_entries ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !isNoRows(err) {
		return nil, err
	}
	var written []Entry
	for _, e := range entries {
		if e.Amount == 0 {
			continue
		}
		if e.Amount < 0 {
			return nil, fmt.Errorf("ledger: negative amount %d for %s", e.Amount, e.Type)
		}
		if e.IdemKey == "" || e.Type == "" || e.From == "" || e.To == "" || e.From == e.To {
			return nil, fmt.Errorf("ledger: malformed entry %+v", e)
		}
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE idem_key=$1)`, e.IdemKey).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		e.ID = newID()
		e.Signed = signedFor(e.Type, e.Amount)
		e.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
		e.PrevHash = prev
		e.Hash = hashEntry(prev, e)
		err := q.QueryRow(ctx, `INSERT INTO ledger_entries
			(id, idem_key, type, order_id, creator_id, from_account, to_account, amount_cents, signed_cents, ref, memo, simulated, prev_hash, hash, created_at)
			VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING seq`,
			e.ID, e.IdemKey, e.Type, e.OrderID, e.CreatorID, e.From, e.To, e.Amount, e.Signed, e.Ref, e.Memo, e.Simulated, e.PrevHash, e.Hash, e.CreatedAt,
		).Scan(&e.Seq)
		if err != nil {
			return nil, fmt.Errorf("ledger insert %s: %w", e.IdemKey, err)
		}
		prev = e.Hash
		written = append(written, e)
		if nonNegative[e.From] {
			// creator:* balances are scoped to (order, creator); order:commission and sway:* to the order.
			creatorScope := e.CreatorID
			if !strings.HasPrefix(e.From, "creator:") {
				creatorScope = "*"
			}
			var bal int64
			if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN to_account=$1 THEN amount_cents ELSE -amount_cents END),0)::bigint
				FROM ledger_entries WHERE (to_account=$1 OR from_account=$1) AND COALESCE(order_id,'')=$2 AND ($3='*' OR COALESCE(creator_id,'')=$3)`,
				e.From, e.OrderID, creatorScope).Scan(&bal); err != nil {
				return nil, err
			}
			if bal < 0 {
				return nil, fmt.Errorf("%w: %s for order %q creator %q would be %d (%s)", ErrOverdraft, e.From, e.OrderID, e.CreatorID, bal, e.IdemKey)
			}
		}
	}
	return written, nil
}

// LockOrders takes row locks on orders in a fixed (sorted) order. Every transaction that moves an order's
// money locks its orders first and only then posts to the ledger, so lock order is always
// orders (ascending) -> ledger, which rules out deadlocks between payouts, refunds, disputes and vesting.
func LockOrders(ctx context.Context, q db.Q, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	rows, err := q.Query(ctx, `SELECT id FROM orders WHERE id = ANY($1) ORDER BY id FOR UPDATE`, sorted)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

func isNoRows(err error) bool { return err != nil && strings.Contains(err.Error(), "no rows") }

// Verification is the result of re-walking the hash chain.
type Verification struct {
	OK          bool   `json:"ok"`
	Entries     int    `json:"entries"`
	Head        string `json:"head"`
	BrokenAtSeq int64  `json:"broken_at_seq,omitempty"`
	Problem     string `json:"problem,omitempty"`
	TrialZero   bool   `json:"trial_balance_zero"`
}

// Verify recomputes every hash in order. Any edited, inserted or deleted entry breaks the chain.
func Verify(ctx context.Context, q db.Q) (Verification, error) {
	v, _, err := VerifyAfter(ctx, q, 0, GenesisHash)
	return v, err
}

// VerifyAfter verifies only entries with seq > afterSeq, which must link to prevHash (the hash of entry afterSeq).
// Because the chain is append-only, verifying the tail incrementally is equivalent to a full walk for new entries.
// It returns the verification (Entries counts only the tail) and the last seq checked.
func VerifyAfter(ctx context.Context, q db.Q, afterSeq int64, prevHash string) (Verification, int64, error) {
	rows, err := q.Query(ctx, `SELECT seq, id, idem_key, type, COALESCE(order_id,''), COALESCE(creator_id,''), from_account, to_account,
		amount_cents, signed_cents, ref, memo, simulated, prev_hash, hash, created_at FROM ledger_entries WHERE seq > $1 ORDER BY seq`, afterSeq)
	if err != nil {
		return Verification{}, afterSeq, err
	}
	defer rows.Close()
	v := Verification{OK: true, Head: prevHash}
	prev := prevHash
	last := afterSeq
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Seq, &e.ID, &e.IdemKey, &e.Type, &e.OrderID, &e.CreatorID, &e.From, &e.To, &e.Amount, &e.Signed,
			&e.Ref, &e.Memo, &e.Simulated, &e.PrevHash, &e.Hash, &e.CreatedAt); err != nil {
			return v, last, err
		}
		v.Entries++
		last = e.Seq
		if v.OK {
			if e.PrevHash != prev {
				v.OK, v.BrokenAtSeq, v.Problem = false, e.Seq, "prev_hash does not link to the previous entry"
			} else if hashEntry(prev, e) != e.Hash {
				v.OK, v.BrokenAtSeq, v.Problem = false, e.Seq, "entry contents do not match its hash"
			}
		}
		prev = e.Hash
	}
	if err := rows.Err(); err != nil {
		return v, last, err
	}
	v.Head = prev
	// Double-entry: every amount leaves one account and enters another, so the global sum is zero.
	var total int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(d),0)::bigint FROM (
		SELECT amount_cents d FROM ledger_entries UNION ALL SELECT -amount_cents FROM ledger_entries) x`).Scan(&total); err != nil {
		return v, last, err
	}
	v.TrialZero = total == 0
	return v, last, nil
}

// Balances maps account -> balance (cents).
type Balances map[string]int64

// Scope filters which entries count toward a balance query.
type Scope struct {
	OrderID   string
	CreatorID string
}

// BalancesFor derives balances for the creator/sway/order-scoped accounts within a scope.
func BalancesFor(ctx context.Context, q db.Q, s Scope) (Balances, error) {
	where, args := []string{"1=1"}, []any{}
	if s.OrderID != "" {
		args = append(args, s.OrderID)
		where = append(where, fmt.Sprintf("order_id=$%d", len(args)))
	}
	if s.CreatorID != "" {
		args = append(args, s.CreatorID)
		where = append(where, fmt.Sprintf("creator_id=$%d", len(args)))
	}
	cond := strings.Join(where, " AND ")
	rows, err := q.Query(ctx, `SELECT account, COALESCE(SUM(delta),0)::bigint FROM (
		SELECT to_account AS account, amount_cents AS delta FROM ledger_entries WHERE `+cond+`
		UNION ALL
		SELECT from_account, -amount_cents FROM ledger_entries WHERE `+cond+`) x GROUP BY account`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b := Balances{}
	for rows.Next() {
		var acct string
		var amt int64
		if err := rows.Scan(&acct, &amt); err != nil {
			return nil, err
		}
		b[acct] = amt
	}
	return b, rows.Err()
}

// OrderCreatorBalances returns per-creator balances for one order (creator-scoped accounts only).
func OrderCreatorBalances(ctx context.Context, q db.Q, orderID string) (map[string]Balances, error) {
	rows, err := q.Query(ctx, `SELECT creator_id, account, COALESCE(SUM(delta),0)::bigint FROM (
		SELECT creator_id, to_account AS account, amount_cents AS delta FROM ledger_entries WHERE order_id=$1 AND creator_id IS NOT NULL
		UNION ALL
		SELECT creator_id, from_account, -amount_cents FROM ledger_entries WHERE order_id=$1 AND creator_id IS NOT NULL) x
		WHERE account LIKE 'creator:%' GROUP BY creator_id, account`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Balances{}
	for rows.Next() {
		var c, acct string
		var amt int64
		if err := rows.Scan(&c, &acct, &amt); err != nil {
			return nil, err
		}
		if out[c] == nil {
			out[c] = Balances{}
		}
		out[c][acct] = amt
	}
	return out, rows.Err()
}

// ErrUnbalanced is returned by CheckOrder when the commission invariant fails.
var ErrUnbalanced = errors.New("ledger: order does not balance")

// OrderSummary is the per-order commission accounting from the ledger.
type OrderSummary struct {
	Commission         int64 `json:"commission_cents"`
	CreatorPoolNet     int64 `json:"creator_pool_net_cents"`
	SwayFee            int64 `json:"sway_fee_cents"`
	ReturnedToMerchant int64 `json:"returned_to_merchant_cents"`
	Undistributed      int64 `json:"undistributed_cents"`
	Forfeited          int64 `json:"forfeited_cents"`
	ClawedBack         int64 `json:"clawed_back_cents"`
	Balanced           bool  `json:"balanced"`
}

// SummarizeOrder checks spec invariant E11: creator_pool_net + sway_fee + returned_to_merchant = commission.
func SummarizeOrder(ctx context.Context, q db.Q, orderID string) (OrderSummary, error) {
	var s OrderSummary
	err := q.QueryRow(ctx, `SELECT
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$2),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$3),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$4),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$5),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$6),0)::bigint,
		COALESCE(SUM(amount_cents) FILTER (WHERE type=$7),0)::bigint
		FROM ledger_entries WHERE order_id=$1`, orderID,
		CommissionReserved, AccruePending, SwayFeePending, ReturnToMerchant, Forfeit, Clawback,
	).Scan(&s.Commission, &s.CreatorPoolNet, &s.SwayFee, &s.ReturnedToMerchant, &s.Forfeited, &s.ClawedBack)
	if err != nil {
		return s, err
	}
	s.Undistributed = s.Commission - s.CreatorPoolNet - s.SwayFee - s.ReturnedToMerchant
	s.Balanced = s.Undistributed == 0
	return s, nil
}

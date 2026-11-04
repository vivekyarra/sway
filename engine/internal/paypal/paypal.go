// Package paypal talks to PayPal's REST APIs (Orders v2, Payments refunds, Payouts, Webhooks)
// behind a Provider interface, with an in-database simulator for keyless local runs.
package paypal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Provider is everything Sway needs from a payment processor.
type Provider interface {
	Mode() string // "sandbox" or "sim"
	CreateOrder(ctx context.Context, r CreateOrderRequest) (Order, error)
	CaptureOrder(ctx context.Context, paypalOrderID, requestID string) (Capture, error)
	RefundCapture(ctx context.Context, captureID string, amountCents int64, requestID, note string) (Refund, error)
	CreatePayoutBatch(ctx context.Context, r PayoutBatchRequest) (PayoutBatch, error)
	GetPayoutBatch(ctx context.Context, batchID string) (PayoutBatch, error)
	CancelPayoutItem(ctx context.Context, itemID string) (PayoutItem, error)
	VerifyWebhook(ctx context.Context, h http.Header, body []byte) (bool, error)
}

type CreateOrderRequest struct {
	RequestID   string // PayPal-Request-Id (our order id): replays return the same PayPal order
	ReferenceID string // our order id
	CustomID    string // decision id
	Description string
	AmountCents int64
}

type Order struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	ApproveURL string `json:"approve_url,omitempty"`
}

type Capture struct {
	OrderID     string `json:"order_id"`
	OrderStatus string `json:"order_status"`
	CaptureID   string `json:"capture_id"`
	Status      string `json:"status"` // COMPLETED, PENDING, DECLINED...
	AmountCents int64  `json:"amount_cents"`
	PayerEmail  string `json:"payer_email"`
	ReferenceID string `json:"reference_id"`
}

type Refund struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AmountCents int64  `json:"amount_cents"`
	CaptureID   string `json:"capture_id"`
}

type PayoutItemRequest struct {
	SenderItemID string
	Receiver     string
	AmountCents  int64
	Note         string
}

type PayoutBatchRequest struct {
	SenderBatchID string // unique: PayPal rejects a reused id, which is what blocks duplicate batches
	Subject       string
	Message       string
	Items         []PayoutItemRequest
}

type PayoutItem struct {
	ItemID       string `json:"payout_item_id"`
	SenderItemID string `json:"sender_item_id"`
	Status       string `json:"transaction_status"`
	Error        string `json:"error,omitempty"`
}

type PayoutBatch struct {
	BatchID string       `json:"payout_batch_id"`
	Status  string       `json:"batch_status"`
	Items   []PayoutItem `json:"items"`
}

// Error is a PayPal API error with its debug id (quote it to PayPal support).
type Error struct {
	Status  int
	Name    string
	Message string
	Issue   string
	DebugID string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("paypal %d %s", e.Status, e.Name)
	if e.Issue != "" {
		s += " (" + e.Issue + ")"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.DebugID != "" {
		s += " [debug_id " + e.DebugID + "]"
	}
	return s
}

// IsIssue reports whether err is a PayPal error with the given issue or name.
func IsIssue(err error, issue string) bool {
	var pe *Error
	return errors.As(err, &pe) && (pe.Issue == issue || pe.Name == issue)
}

// FinalItemStatus reports whether a payout item status is terminal.
func FinalItemStatus(s string) bool {
	switch s {
	case "SUCCESS", "FAILED", "RETURNED", "BLOCKED", "REFUNDED", "REVERSED", "DENIED":
		return true
	}
	return false
}

// FailedItemStatus reports whether money did not reach (or came back from) the creator.
func FailedItemStatus(s string) bool {
	switch s {
	case "FAILED", "RETURNED", "BLOCKED", "REFUNDED", "REVERSED", "DENIED":
		return true
	}
	return false
}

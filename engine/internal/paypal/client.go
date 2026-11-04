package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vivekyarra/sway/engine/internal/money"
)

// Client is the real PayPal REST client (sandbox or live).
type Client struct {
	baseURL, clientID, secret, webhookID string
	http                                 *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewClient(baseURL, clientID, secret, webhookID string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, secret: secret, webhookID: webhookID,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 25 * time.Second,
			DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		}},
	}
}

func (c *Client) Mode() string { return "sandbox" }

// accessToken returns a cached OAuth2 client-credentials token, refreshed a minute before expiry.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal oauth: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", parseError(resp, body)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("paypal oauth: bad token response")
	}
	c.token = t.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return c.token, nil
}

// Ping verifies credentials (used by /api/health).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.accessToken(ctx)
	return err
}

// do sends a JSON request. Writes carry PayPal-Request-Id, so retrying a timed-out or 5xx write is safe:
// PayPal returns the original result instead of executing twice.
func (c *Client) do(ctx context.Context, method, path, requestID string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	retryable := method == http.MethodGet || requestID != ""
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(200*(1<<attempt))*time.Millisecond + time.Duration(rand.IntN(250))*time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Prefer", "return=representation")
		if requestID != "" {
			req.Header.Set("PayPal-Request-Id", requestID)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if retryable && ctx.Err() == nil {
				continue
			}
			return err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(respBody) > 0 {
				return json.Unmarshal(respBody, out)
			}
			return nil
		}
		lastErr = parseError(resp, respBody)
		if retryable && (resp.StatusCode == 429 || resp.StatusCode >= 500 || resp.StatusCode == 401) {
			continue
		}
		return lastErr
	}
	return lastErr
}

func parseError(resp *http.Response, body []byte) error {
	var e struct {
		Name             string `json:"name"`
		Message          string `json:"message"`
		DebugID          string `json:"debug_id"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Details          []struct {
			Issue       string `json:"issue"`
			Description string `json:"description"`
		} `json:"details"`
	}
	_ = json.Unmarshal(body, &e)
	pe := &Error{Status: resp.StatusCode, Name: e.Name, Message: e.Message, DebugID: e.DebugID}
	if pe.DebugID == "" {
		pe.DebugID = resp.Header.Get("Paypal-Debug-Id")
	}
	if e.Error != "" {
		pe.Name, pe.Message = e.Error, e.ErrorDescription
	}
	if len(e.Details) > 0 {
		pe.Issue = e.Details[0].Issue
		if e.Details[0].Description != "" {
			pe.Message = e.Details[0].Description
		}
	}
	if pe.Name == "" {
		pe.Name = http.StatusText(resp.StatusCode)
	}
	return pe
}

type amount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

func (c *Client) CreateOrder(ctx context.Context, r CreateOrderRequest) (Order, error) {
	body := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{{
			"reference_id": r.ReferenceID,
			"custom_id":    r.CustomID,
			"description":  truncate(r.Description, 127),
			"amount":       amount{"USD", money.Format(r.AmountCents)},
		}},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": map[string]any{
			"shipping_preference": "NO_SHIPPING", "user_action": "PAY_NOW", "brand_name": "Sway demo store",
		}}},
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/checkout/orders", r.RequestID, body, &resp); err != nil {
		return Order{}, err
	}
	o := Order{ID: resp.ID, Status: resp.Status}
	for _, l := range resp.Links {
		if l.Rel == "approve" || l.Rel == "payer-action" {
			o.ApproveURL = l.Href
		}
	}
	return o, nil
}

type orderResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Payer  struct {
		Email string `json:"email_address"`
	} `json:"payer"`
	PurchaseUnits []struct {
		ReferenceID string `json:"reference_id"`
		Payments    struct {
			Captures []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Amount amount `json:"amount"`
			} `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

func (o orderResponse) capture() (Capture, error) {
	c := Capture{OrderID: o.ID, OrderStatus: o.Status, PayerEmail: o.Payer.Email}
	if len(o.PurchaseUnits) == 0 {
		return c, errors.New("paypal: order has no purchase units")
	}
	pu := o.PurchaseUnits[0]
	c.ReferenceID = pu.ReferenceID
	if len(pu.Payments.Captures) == 0 {
		return c, errors.New("paypal: order has no capture")
	}
	cp := pu.Payments.Captures[0]
	c.CaptureID, c.Status = cp.ID, cp.Status
	amt, err := money.Parse(cp.Amount.Value)
	if err != nil {
		return c, err
	}
	c.AmountCents = amt
	return c, nil
}

func (c *Client) CaptureOrder(ctx context.Context, paypalOrderID, requestID string) (Capture, error) {
	var resp orderResponse
	err := c.do(ctx, http.MethodPost, "/v2/checkout/orders/"+paypalOrderID+"/capture", requestID, map[string]any{}, &resp)
	if IsIssue(err, "ORDER_ALREADY_CAPTURED") {
		// Captured by an earlier attempt: read it back so the caller gets the same capture.
		if err = c.do(ctx, http.MethodGet, "/v2/checkout/orders/"+paypalOrderID, "", nil, &resp); err != nil {
			return Capture{}, err
		}
	} else if err != nil {
		return Capture{}, err
	}
	return resp.capture()
}

func (c *Client) RefundCapture(ctx context.Context, captureID string, amountCents int64, requestID, note string) (Refund, error) {
	body := map[string]any{}
	if amountCents > 0 {
		body["amount"] = amount{"USD", money.Format(amountCents)}
	}
	if note != "" {
		body["note_to_payer"] = truncate(note, 255)
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Amount amount `json:"amount"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/payments/captures/"+captureID+"/refund", requestID, body, &resp); err != nil {
		return Refund{}, err
	}
	amt, err := money.Parse(resp.Amount.Value)
	if err != nil {
		return Refund{}, err
	}
	return Refund{ID: resp.ID, Status: resp.Status, AmountCents: amt, CaptureID: captureID}, nil
}

type payoutBatchResponse struct {
	BatchHeader struct {
		PayoutBatchID string `json:"payout_batch_id"`
		BatchStatus   string `json:"batch_status"`
	} `json:"batch_header"`
	Items []struct {
		PayoutItemID      string `json:"payout_item_id"`
		TransactionStatus string `json:"transaction_status"`
		PayoutItem        struct {
			SenderItemID string `json:"sender_item_id"`
		} `json:"payout_item"`
		Errors struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"items"`
}

func (r payoutBatchResponse) batch() PayoutBatch {
	b := PayoutBatch{BatchID: r.BatchHeader.PayoutBatchID, Status: r.BatchHeader.BatchStatus}
	for _, it := range r.Items {
		b.Items = append(b.Items, PayoutItem{ItemID: it.PayoutItemID, SenderItemID: it.PayoutItem.SenderItemID, Status: it.TransactionStatus,
			Error: strings.TrimSpace(it.Errors.Name + " " + it.Errors.Message)})
	}
	return b
}

func (c *Client) CreatePayoutBatch(ctx context.Context, r PayoutBatchRequest) (PayoutBatch, error) {
	items := make([]map[string]any, len(r.Items))
	for i, it := range r.Items {
		items[i] = map[string]any{
			"recipient_type": "EMAIL",
			"amount":         map[string]string{"value": money.Format(it.AmountCents), "currency": "USD"},
			"receiver":       it.Receiver,
			"note":           truncate(it.Note, 4000),
			"sender_item_id": it.SenderItemID,
		}
	}
	body := map[string]any{
		"sender_batch_header": map[string]string{"sender_batch_id": r.SenderBatchID, "email_subject": r.Subject, "email_message": r.Message},
		"items":               items,
	}
	var resp payoutBatchResponse
	err := c.do(ctx, http.MethodPost, "/v1/payments/payouts", r.SenderBatchID, body, &resp)
	if IsIssue(err, "DUPLICATE_REQUEST_ID") || IsIssue(err, "SENDER_BATCH_ID_DUPLICATE") || IsIssue(err, "USER_BUSINESS_ERROR") && strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		return PayoutBatch{}, fmt.Errorf("paypal: batch %s already submitted: %w", r.SenderBatchID, err)
	}
	if err != nil {
		return PayoutBatch{}, err
	}
	return resp.batch(), nil
}

func (c *Client) GetPayoutBatch(ctx context.Context, batchID string) (PayoutBatch, error) {
	var resp payoutBatchResponse
	if err := c.do(ctx, http.MethodGet, "/v1/payments/payouts/"+batchID+"?fields=batch_header,items&page_size=1000", "", nil, &resp); err != nil {
		return PayoutBatch{}, err
	}
	return resp.batch(), nil
}

func (c *Client) CancelPayoutItem(ctx context.Context, itemID string) (PayoutItem, error) {
	var resp struct {
		PayoutItemID      string `json:"payout_item_id"`
		TransactionStatus string `json:"transaction_status"`
		PayoutItem        struct {
			SenderItemID string `json:"sender_item_id"`
		} `json:"payout_item"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/payments/payouts-item/"+itemID+"/cancel", "cancel-"+itemID, nil, &resp); err != nil {
		return PayoutItem{}, err
	}
	return PayoutItem{ItemID: resp.PayoutItemID, SenderItemID: resp.PayoutItem.SenderItemID, Status: resp.TransactionStatus}, nil
}

// VerifyWebhook asks PayPal to verify the transmission signature (spec 8.5). The event is passed
// as raw JSON so the bytes PayPal signed are exactly the bytes it verifies.
func (c *Client) VerifyWebhook(ctx context.Context, h http.Header, body []byte) (bool, error) {
	if c.webhookID == "" {
		return false, errors.New("PAYPAL_WEBHOOK_ID is not configured")
	}
	if !json.Valid(body) {
		return false, nil
	}
	req := map[string]any{
		"auth_algo":         h.Get("Paypal-Auth-Algo"),
		"cert_url":          h.Get("Paypal-Cert-Url"),
		"transmission_id":   h.Get("Paypal-Transmission-Id"),
		"transmission_sig":  h.Get("Paypal-Transmission-Sig"),
		"transmission_time": h.Get("Paypal-Transmission-Time"),
		"webhook_id":        c.webhookID,
		"webhook_event":     json.RawMessage(body),
	}
	for _, k := range []string{"auth_algo", "cert_url", "transmission_id", "transmission_sig", "transmission_time"} {
		if req[k] == "" {
			return false, nil
		}
	}
	var resp struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", "", req, &resp); err != nil {
		return false, err
	}
	return resp.VerificationStatus == "SUCCESS", nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

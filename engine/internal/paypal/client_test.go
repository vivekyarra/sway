package paypal_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vivekyarra/sway/engine/internal/paypal"
)

type stub struct {
	tokens, creates, captures, failNext atomic.Int32
	requestIDs                          []string
	lastVerify                          map[string]json.RawMessage
}

func (m *stub) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/v1/oauth2/token":
			if u, p, ok := r.BasicAuth(); !ok || u != "id" || p != "secret" {
				w.WriteHeader(401)
				return
			}
			m.tokens.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(401)
		case r.URL.Path == "/v2/checkout/orders" && r.Method == "POST":
			m.creates.Add(1)
			m.requestIDs = append(m.requestIDs, r.Header.Get("PayPal-Request-Id"))
			if m.failNext.Load() > 0 {
				m.failNext.Add(-1)
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"name":"SERVICE_UNAVAILABLE"}`)
				return
			}
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			pu := req["purchase_units"].([]any)[0].(map[string]any)
			if pu["amount"].(map[string]any)["value"] != "229.00" {
				t.Errorf("amount not formatted: %v", pu["amount"])
			}
			if r.Header.Get("PayPal-Request-Id") == "ord_1" && (req["intent"] != "CAPTURE" || req["payment_source"] != nil || pu["reference_id"] != "ord_1" || pu["custom_id"] != "dec_1") {
				t.Errorf("order body must match the standard JS SDK integration: %v", req)
			}
			_, _ = io.WriteString(w, `{"id":"5O190127TN364715T","status":"CREATED","links":[{"rel":"payer-action","href":"https://sandbox.paypal.com/checkoutnow?token=5O1"}]}`)
		case strings.HasSuffix(r.URL.Path, "/capture"):
			m.captures.Add(1)
			w.WriteHeader(422)
			_, _ = io.WriteString(w, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"ORDER_ALREADY_CAPTURED"}],"debug_id":"abc"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/checkout/orders/") && r.Method == "GET":
			_, _ = io.WriteString(w, `{"id":"5O1","status":"COMPLETED","payer":{"email_address":"buyer@example.com"},"purchase_units":[{"reference_id":"ord_1","payments":{"captures":[{"id":"CAP1","status":"COMPLETED","amount":{"currency_code":"USD","value":"229.00"}}]}}]}`)
		case r.URL.Path == "/v1/notifications/verify-webhook-signature":
			_ = json.Unmarshal(body, &m.lastVerify)
			_, _ = io.WriteString(w, `{"verification_status":"SUCCESS"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientCreateRetriesWithSameRequestID(t *testing.T) {
	m := &stub{}
	c := paypal.NewClient(m.server(t).URL, "id", "secret", "WH-1")
	m.failNext.Store(1)
	o, err := c.CreateOrder(context.Background(), paypal.CreateOrderRequest{RequestID: "ord_1", ReferenceID: "ord_1", CustomID: "dec_1", AmountCents: 22900, Description: "Halvo Q7"})
	if err != nil {
		t.Fatal(err)
	}
	if o.ID == "" || !strings.Contains(o.ApproveURL, "checkoutnow") {
		t.Fatalf("order %+v", o)
	}
	if m.creates.Load() != 2 || m.requestIDs[0] != "ord_1" || m.requestIDs[1] != "ord_1" {
		t.Fatalf("expected a retry with the same PayPal-Request-Id, got %v", m.requestIDs)
	}
	if _, err := c.CreateOrder(context.Background(), paypal.CreateOrderRequest{RequestID: "ord_2", AmountCents: 22900}); err != nil {
		t.Fatal(err)
	}
	if m.tokens.Load() != 1 {
		t.Fatalf("token should be cached, fetched %d times", m.tokens.Load())
	}
}

func TestClientCaptureAlreadyCapturedReadsBack(t *testing.T) {
	m := &stub{}
	c := paypal.NewClient(m.server(t).URL, "id", "secret", "WH-1")
	cp, err := c.CaptureOrder(context.Background(), "5O1", "capture-ord_1")
	if err != nil {
		t.Fatal(err)
	}
	if cp.CaptureID != "CAP1" || cp.AmountCents != 22900 || cp.PayerEmail != "buyer@example.com" {
		t.Fatalf("capture %+v", cp)
	}
}

func TestClientVerifyWebhookSendsRawEvent(t *testing.T) {
	m := &stub{}
	c := paypal.NewClient(m.server(t).URL, "id", "secret", "WH-1")
	raw := []byte(`{"id":"WH-EVT-1","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"CAP1","amount":{"value":"229.00"}}}`)
	h := http.Header{}
	for k, v := range map[string]string{"Paypal-Auth-Algo": "SHA256withRSA", "Paypal-Cert-Url": "https://api.sandbox.paypal.com/cert", "Paypal-Transmission-Id": "t1",
		"Paypal-Transmission-Sig": "sig", "Paypal-Transmission-Time": "2026-11-04T00:00:00Z"} {
		h.Set(k, v)
	}
	ok, err := c.VerifyWebhook(context.Background(), h, raw)
	if err != nil || !ok {
		t.Fatalf("verify %v %v", ok, err)
	}
	if string(m.lastVerify["webhook_event"]) != string(raw) {
		t.Fatalf("event bytes changed: %s", m.lastVerify["webhook_event"])
	}
	if string(m.lastVerify["webhook_id"]) != `"WH-1"` {
		t.Fatalf("webhook id %s", m.lastVerify["webhook_id"])
	}
	// Missing transmission headers never reach PayPal and never verify.
	if ok, _ := c.VerifyWebhook(context.Background(), http.Header{}, raw); ok {
		t.Fatal("unsigned webhook verified")
	}
}

func TestClientBadCredentials(t *testing.T) {
	m := &stub{}
	c := paypal.NewClient(m.server(t).URL, "id", "wrong", "")
	if _, err := c.CreateOrder(context.Background(), paypal.CreateOrderRequest{RequestID: "x", AmountCents: 100}); err == nil {
		t.Fatal("expected auth failure")
	}
}

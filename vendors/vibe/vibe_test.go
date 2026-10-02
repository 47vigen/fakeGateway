package vibe

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/47vigen/fakeGateway/core"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const testKey = "test-key"

func newTestApp() (http.Handler, *core.Core) {
	return core.Handler(&Vendor{apiKey: testKey}, "http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual, EnforceAmounts: true})
}

func do(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func createBody(orderID, callback string) map[string]any {
	return map[string]any{
		"order_id":     orderID,
		"cart_amount":  12_500_000,
		"callback_url": callback,
		"data": map[string]any{
			"tax":          0,
			"goods_amount": 12_500_000,
			"items": []map[string]any{
				{"id": "p1", "name": "Item", "price": 5_000_000, "quantity": 2, "discount": 0},
				{"id": "p2", "name": "Other", "price": 2_500_000, "quantity": 1, "discount": 0},
			},
		},
	}
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func verify(t *testing.T, h http.Handler, refID string) map[string]any {
	t.Helper()
	w := do(t, h, "POST", "/api/v1/orders/verify", testKey, map[string]any{"ref_id": refID})
	if w.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", w.Code)
	}
	return decode(t, w)
}

func TestCreateSettleVerify(t *testing.T) {
	h, _ := newTestApp()
	const id = "0198f3c1-1111-2222-3333-444455556666"

	w := do(t, h, "POST", "/api/v1/orders/", testKey, createBody(id, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := decode(t, w)["payment_url"]; got != "http://gw.test/pay/"+id {
		t.Fatalf("payment_url = %v", got)
	}

	// Created but not settled: verify must not claim payment.
	if got := verify(t, h, id)["status"]; got != false {
		t.Fatalf("status before settle = %v, want false", got)
	}

	if w := do(t, h, "POST", "/api/v1/test/orders/"+id+"/settle", "", nil); w.Code != http.StatusOK {
		t.Fatalf("settle status = %d: %s", w.Code, w.Body)
	}
	if got := verify(t, h, id)["status"]; got != true {
		t.Fatalf("status after settle = %v, want true", got)
	}

	// The full create request is readable back for assertions.
	got := decode(t, do(t, h, "GET", "/api/v1/test/orders/"+id, "", nil))
	req, ok := got["request"].(map[string]any)
	if !ok || req["cart_amount"] != float64(12_500_000) {
		t.Fatalf("stored request = %v", got["request"])
	}
}

func TestVerifyUnknownRefIsOKWithStatusFalse(t *testing.T) {
	h, _ := newTestApp()
	// The real provider answers 200 with status:false for an order it cannot
	// find. It does not 404, and a 200 is not proof of payment.
	if got := verify(t, h, "no-such-order")["status"]; got != false {
		t.Fatalf("status = %v, want false", got)
	}
}

func TestAPIKeyIsChecked(t *testing.T) {
	h, _ := newTestApp()
	for _, tc := range []struct{ name, path string }{
		{"create", "/api/v1/orders/"},
		{"verify", "/api/v1/orders/verify"},
	} {
		for _, key := range []string{"", "wrong-key"} {
			w := do(t, h, "POST", tc.path, key, createBody("id-1", ""))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s with key %q = %d, want 401", tc.name, key, w.Code)
			}
		}
	}
}

func TestAmountMismatchIsRejectedUnlessDisabled(t *testing.T) {
	h, s := newTestApp()
	body := createBody("mismatch-1", "")
	body["cart_amount"] = 12_500_001 // one Rial off goods_amount + tax

	if w := do(t, h, "POST", "/api/v1/orders/", testKey, body); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
	}

	b := s.Behaviour()
	b.EnforceAmounts = false
	s.SetBehaviour(b)
	if w := do(t, h, "POST", "/api/v1/orders/", testKey, body); w.Code != http.StatusOK {
		t.Fatalf("status with the check off = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestPayPageAndCallback(t *testing.T) {
	var seen *url.URL
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL
		http.Redirect(w, r, "http://front.test/after-payment", http.StatusTemporaryRedirect)
	}))
	defer merchant.Close()

	h, _ := newTestApp()
	const id = "callback-1"
	if w := do(t, h, "POST", "/api/v1/orders/", testKey, createBody(id, merchant.URL+"/v1/gateway/callback/"+id)); w.Code != http.StatusOK {
		t.Fatalf("create: %s", w.Body)
	}

	// The page renders and offers both outcomes.
	page := do(t, h, "GET", "/pay/"+id, "", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "12,500,000") {
		t.Fatalf("pay page = %d, body missing the formatted amount", page.Code)
	}

	// Clicking "pay" settles the order and sends the browser to the callback.
	click := httptest.NewRequest("POST", "/pay/"+id, strings.NewReader("outcome=pay"))
	click.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, click)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("pay submit = %d, want 303", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Query().Get("result") != "OK" || loc.Query().Get("ref_id") != id {
		t.Fatalf("redirect query = %q", loc.RawQuery)
	}
	if got := verify(t, h, id)["status"]; got != true {
		t.Fatalf("status after the pay click = %v, want true", got)
	}

	// The headless path: the control API calls the merchant back itself.
	got := decode(t, do(t, h, "POST", "/api/v1/test/orders/"+id+"/callback", "", nil))
	cb, _ := got["callback"].(map[string]any)
	if cb["status"] != float64(http.StatusTemporaryRedirect) || cb["location"] != "http://front.test/after-payment" {
		t.Fatalf("callback result = %v", got["callback"])
	}
	if seen == nil || seen.Query().Get("result") != "OK" || seen.Query().Get("ref_id") != id {
		t.Fatalf("the merchant saw %v", seen)
	}
}

// The legacy knob names still force a status out of the provider routes, and
// zero clears them.
func TestForcedStatus(t *testing.T) {
	h, _ := newTestApp()
	if w := do(t, h, "POST", "/api/v1/test/config", "", map[string]any{"create_status": 500}); w.Code != http.StatusOK {
		t.Fatalf("config = %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/v1/orders/", testKey, createBody("forced-1", "")); w.Code != http.StatusInternalServerError {
		t.Fatalf("create with create_status 500 = %d", w.Code)
	}
	do(t, h, "POST", "/api/v1/test/config", "", map[string]any{"create_status": 0})
	if w := do(t, h, "POST", "/api/v1/orders/", testKey, createBody("forced-1", "")); w.Code != http.StatusOK {
		t.Fatalf("create after clearing = %d: %s", w.Code, w.Body)
	}
}

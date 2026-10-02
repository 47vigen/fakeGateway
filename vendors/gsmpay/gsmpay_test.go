package gsmpay

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

const merchant = "test-merchant"

func newTestApp() (http.Handler, *core.Core) {
	return core.Handler(&Vendor{merchantCode: merchant}, "http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
}

func post(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body, err)
		}
	}
	return w.Code, out
}

// The payload the merchant's gsmpay driver builds.
func createBody(ref, callback string) map[string]any {
	return map[string]any{
		"merchant_code":       merchant,
		"callback_url":        callback,
		"invoice_reference":   ref,
		"invoice_amount":      12_500_000,
		"invoice_date":        "2026-10-02T10:00:00.000Z",
		"payer_mobile":        "09120000000",
		"payer_first_name":    "وحید",
		"payer_last_name":     "رضایی",
		"payer_national_code": "0000000000",
		"description":         "Order " + ref,
		"items": []map[string]any{{
			"is_product": true, "name": "Item", "quantity": 1, "reference": "p1",
			"unit_price": "12500000", "unit_discount": "0", "unit_tax_amount": "0",
		}},
	}
}

func create(t *testing.T, h http.Handler, ref, callback string) string {
	t.Helper()
	code, out := post(t, h, "/v1/cpg/payments", createBody(ref, callback))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	token := data["token"].(string)
	if data["redirect_url"] != "http://gw.test/pay/"+token {
		t.Fatalf("redirect_url = %v", data["redirect_url"])
	}
	return token
}

func verify(t *testing.T, h http.Handler, token, ref string, amount int) (int, map[string]any) {
	t.Helper()
	return post(t, h, "/v1/cpg/payments/verify", map[string]any{
		"merchant_code": merchant, "token": token, "invoice_reference": ref, "invoice_amount": amount,
	})
}

func reverse(t *testing.T, h http.Handler, token string, amount int) (int, map[string]any) {
	t.Helper()
	return post(t, h, "/v1/cpg/payments/reverse", map[string]any{
		"merchant_code": merchant, "token": token, "reverse_amount": amount,
	})
}

func TestMerchantCodeIsChecked(t *testing.T) {
	h, _ := newTestApp()
	for _, path := range []string{"/v1/cpg/payments", "/v1/cpg/payments/verify", "/v1/cpg/payments/reverse"} {
		for _, mc := range []string{"", "wrong"} {
			body := createBody("1", "")
			body["merchant_code"] = mc
			code, out := post(t, h, path, body)
			if code != http.StatusUnprocessableEntity || out["type"] != "merchant_code_error" {
				t.Errorf("%s with merchant %q = %d %v", path, mc, code, out)
			}
		}
	}
}

func TestCreateValidationAndDuplicate(t *testing.T) {
	h, _ := newTestApp()
	create(t, h, "1001", "")
	for name, mutate := range map[string]func(map[string]any){
		"duplicate reference": func(map[string]any) {},
		"no reference":        func(b map[string]any) { b["invoice_reference"] = "" },
		"zero amount":         func(b map[string]any) { b["invoice_amount"] = 0 },
	} {
		body := createBody("1001", "")
		mutate(body)
		code, out := post(t, h, "/v1/cpg/payments", body)
		if code != http.StatusUnprocessableEntity || out["type"] != "validation_error" || out["errors"] == nil {
			t.Errorf("%s = %d %v", name, code, out)
		}
	}
}

func TestVerifyFollowsTheOrder(t *testing.T) {
	h, _ := newTestApp()
	token := create(t, h, "1001", "")

	for _, tc := range []struct {
		name   string
		token  string
		ref    string
		amount int
		status int
		want   string // is_paid, or the error type
	}{
		{"before settle", token, "1001", 12_500_000, 200, "false"},
		{"unknown token", "nope", "1001", 12_500_000, 422, "payment_token_error"},
		{"other invoice", token, "9999", 12_500_000, 422, "payment_token_error"},
		{"other amount", token, "1001", 1, 422, "invoice_amount_error"},
	} {
		code, out := verify(t, h, tc.token, tc.ref, tc.amount)
		if code != tc.status || got(out) != tc.want {
			t.Errorf("%s = %d %v, want %d %s", tc.name, code, out, tc.status, tc.want)
		}
	}

	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)
	for range 2 { // verify is idempotent: GSMPay documents no "already verified"
		code, out := verify(t, h, token, "1001", 12_500_000)
		data, _ := out["data"].(map[string]any)
		if code != 200 || data["is_paid"] != true || data["invoice_amount"] != float64(12_500_000) || data["token"] != token {
			t.Fatalf("after settle = %d %v", code, out)
		}
	}
}

func got(out map[string]any) string {
	if data, ok := out["data"].(map[string]any); ok {
		if data["is_paid"] == true {
			return "true"
		}
		return "false"
	}
	s, _ := out["type"].(string)
	return s
}

func TestReverse(t *testing.T) {
	h, _ := newTestApp()
	token := create(t, h, "1001", "")

	if code, out := reverse(t, h, token, 12_500_000); code != 422 || out["type"] != "invalid_order_status" {
		t.Fatalf("reverse unpaid = %d %v", code, out)
	}
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)

	code, out := reverse(t, h, token, 12_500_000)
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["token"] != token || data["reversed_amount"] != float64(12_500_000) {
		t.Fatalf("reverse = %d %v", code, out)
	}
	if code, out := reverse(t, h, token, 12_500_000); code != 422 || out["type"] != "reverse_amount_exceeded" {
		t.Fatalf("reverse twice = %d %v", code, out)
	}
	if code, out := reverse(t, h, "nope", 1); code != 422 || out["type"] != "payment_token_error" {
		t.Fatalf("reverse unknown = %d %v", code, out)
	}
	// A reversed payment no longer verifies as paid.
	if _, out := verify(t, h, token, "1001", 12_500_000); got(out) != "false" {
		t.Fatalf("verify after reverse = %v", out)
	}
}

func TestPartialReverseAddsUp(t *testing.T) {
	h, _ := newTestApp()
	token := create(t, h, "1001", "")
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)
	reverse(t, h, token, 2_500_000)
	_, out := reverse(t, h, token, 10_000_000)
	if out["data"].(map[string]any)["reversed_amount"] != float64(12_500_000) {
		t.Fatalf("second partial = %v", out)
	}
}

func TestPayPageAndCallback(t *testing.T) {
	var seen *url.URL
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL
		http.Redirect(w, r, "http://front.test/after-payment", http.StatusFound)
	}))
	defer merchant.Close()

	h, _ := newTestApp()
	token := create(t, h, "1001", merchant.URL+"/v1/gateway/callback/hash-1")

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/pay/"+token, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "12,500,000") {
		t.Fatalf("pay page = %d", page.Code)
	}

	for _, tc := range []struct{ outcome, status string }{{"cancel", "failed"}, {"pay", "success"}} {
		click := httptest.NewRequest("POST", "/pay/"+token, strings.NewReader("outcome="+tc.outcome))
		click.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, click)
		loc, _ := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusSeeOther || loc.Path != "/v1/gateway/callback/hash-1" ||
			loc.Query().Get("token") != token || loc.Query().Get("status") != tc.status {
			t.Fatalf("%s = %d %s", tc.outcome, w.Code, loc)
		}
	}

	req := httptest.NewRequest("POST", "/api/v1/test/orders/"+token+"/callback", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if seen == nil || seen.Query().Get("status") != "success" || seen.Query().Get("token") != token {
		t.Fatalf("the merchant saw %v", seen)
	}

	view := httptest.NewRecorder()
	h.ServeHTTP(view, httptest.NewRequest("GET", "/api/v1/test/orders/"+token, nil))
	var v map[string]any
	json.Unmarshal(view.Body.Bytes(), &v)
	calls := v["calls"].(map[string]any)
	if calls["create"] != float64(1) || calls["callback"] != float64(3) || v["request"].(map[string]any)["invoice_reference"] != "1001" {
		t.Fatalf("view = %v", v)
	}
}

func TestForced(t *testing.T) {
	h, _ := newTestApp()
	token := create(t, h, "1001", "")
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)
	for _, tc := range []struct {
		force  map[string]string
		status int
		typ    string
	}{
		{map[string]string{"verify": "invoice_amount_error"}, 422, "invoice_amount_error"},
		{map[string]string{"verify": "503"}, 503, "server_error"},
		{map[string]string{"refund": "authorization_error"}, 403, "authorization_error"},
		{map[string]string{"create": "ip_address_error"}, 422, "ip_address_error"},
	} {
		post(t, h, "/api/v1/test/config", map[string]any{"force": tc.force})
		var code int
		var out map[string]any
		switch {
		case tc.force["verify"] != "":
			code, out = verify(t, h, token, "1001", 12_500_000)
		case tc.force["refund"] != "":
			code, out = reverse(t, h, token, 1)
		default:
			code, out = post(t, h, "/v1/cpg/payments", createBody("2002", ""))
		}
		if code != tc.status || out["type"] != tc.typ {
			t.Errorf("force %v = %d %v", tc.force, code, out)
		}
		post(t, h, "/api/v1/test/config", map[string]any{})
	}
}

package kamanlend

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

const (
	terminal = "test-terminal"
	secret   = "test-secret"
	nid      = "0012345678"
)

func newTestApp() http.Handler {
	h, _ := core.Handler(&Vendor{terminalCode: terminal, terminalSecret: secret, creditLimit: 50_000_000},
		"http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
	return h
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

// The payload the merchant's kamanlend driver builds for initiate and preCheck.
func register(code, redirect string, amount int) map[string]any {
	return map[string]any{
		"terminalCode":         terminal,
		"terminalSecret":       secret,
		"redirectionUrl":       redirect,
		"shoppingCardCode":     code,
		"customerNationalCode": nid,
		"saleItems": []map[string]any{
			{"code": "p1", "title": "Item", "quantity": 1, "totalAmountRial": amount},
		},
	}
}

func create(t *testing.T, h http.Handler, code, redirect string) string {
	t.Helper()
	status, out := post(t, h, "/api/Gateway/RegisterPayment", register(code, redirect, 12_500_000))
	res, _ := out["result"].(map[string]any)
	if status != 200 || out["success"] != true || res["token"] == nil {
		t.Fatalf("register = %d %v", status, out)
	}
	token := res["token"].(string)
	if res["gatewayUrl"] != "http://gw.test/pay/"+token {
		t.Fatalf("gatewayUrl = %v", res["gatewayUrl"])
	}
	return token
}

func state(t *testing.T, h http.Handler, token string) (int, map[string]any) {
	t.Helper()
	return post(t, h, "/api/Gateway/GetPaymentState", map[string]any{
		"terminalCode": terminal, "terminalSecret": secret, "customerNationalCode": nid, "token": token,
	})
}

func refused(out map[string]any) string {
	msgs, _ := out["messages"].([]any)
	if out["success"] != false || len(msgs) == 0 {
		return ""
	}
	m := msgs[0].(map[string]any)
	if m["messageKind"] != "Error" {
		return ""
	}
	return m["message"].(string)
}

func TestTerminalIsChecked(t *testing.T) {
	h := newTestApp()
	for _, path := range []string{"/api/Gateway/RegisterPayment", "/api/Gateway/GetPaymentState"} {
		for _, sec := range []string{"", "wrong"} {
			body := register("1", "", 1)
			body["terminalSecret"] = sec
			status, out := post(t, h, path, body)
			if status != http.StatusUnauthorized || refused(out) == "" {
				t.Errorf("%s with secret %q = %d %v", path, sec, status, out)
			}
		}
	}
}

// preCheck is a RegisterPayment whose redirect is thrown away: a token means
// eligible, a 4xx refusal means not.
func TestPreCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		force  string
		amount int
		ok     bool
	}{
		{"eligible within the default limit", "", 50_000_000, true},
		{"over the default limit", "", 50_000_001, false},
		{"forced limit below the amount", "1000000", 2_000_000, false},
		{"forced limit above the amount", "3000000", 2_000_000, true},
		{"forced not eligible", "not_eligible", 1_000, false},
	} {
		h := newTestApp()
		if tc.force != "" {
			post(t, h, "/api/v1/test/config", map[string]any{"force": map[string]string{"pre_check": tc.force}})
		}
		status, out := post(t, h, "/api/Gateway/RegisterPayment", register("2001", "http://front.test/after-payment", tc.amount))
		if tc.ok != (status == 200 && out["success"] == true) {
			t.Errorf("%s = %d %v", tc.name, status, out)
		}
		if !tc.ok && (status != http.StatusBadRequest || refused(out) == "") {
			t.Errorf("%s: refusal shape = %d %v", tc.name, status, out)
		}
	}
}

func TestRegisterValidationAndDuplicate(t *testing.T) {
	h := newTestApp()
	token := create(t, h, "1001", "")
	// A pending sale may be registered again (a pre-check and the real leg
	// can share a code); a completed one may not.
	create(t, h, "1001", "")
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)

	for name, body := range map[string]map[string]any{
		"completed code": register("1001", "", 12_500_000),
		"no items":       func() map[string]any { b := register("3", "", 1); b["saleItems"] = []any{}; return b }(),
		"no national":    func() map[string]any { b := register("4", "", 1); b["customerNationalCode"] = ""; return b }(),
	} {
		if status, out := post(t, h, "/api/Gateway/RegisterPayment", body); status != 400 || refused(out) == "" {
			t.Errorf("%s = %d %v", name, status, out)
		}
	}
}

func TestPaymentState(t *testing.T) {
	h := newTestApp()
	token := create(t, h, "1001", "")
	want := func(name string, wantState string) {
		t.Helper()
		status, out := state(t, h, token)
		res, _ := out["result"].(map[string]any)
		if status != 200 || out["success"] != true || res["saleRequestState"] != wantState || res["totalSaleAmountRial"] != float64(12_500_000) {
			t.Fatalf("%s = %d %v", name, status, out)
		}
	}
	want("pending", "WaitForCustomerApprove")
	post(t, h, "/api/v1/test/orders/"+token+"/fail", nil)
	want("failed", "Canceled")
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)
	want("settled", "PaymentCompleted")
	want("settled twice", "PaymentCompleted")

	if status, out := state(t, h, "nope"); status != 400 || refused(out) == "" {
		t.Fatalf("unknown token = %d %v", status, out)
	}
	body := map[string]any{"terminalCode": terminal, "terminalSecret": secret, "customerNationalCode": "9999999999", "token": token}
	if status, out := post(t, h, "/api/Gateway/GetPaymentState", body); status != 400 || refused(out) == "" {
		t.Fatalf("someone else's token = %d %v", status, out)
	}
}

func TestForced(t *testing.T) {
	h := newTestApp()
	token := create(t, h, "1001", "")
	post(t, h, "/api/v1/test/orders/"+token+"/settle", nil)

	post(t, h, "/api/v1/test/config", map[string]any{"force": map[string]string{"verify": "Rejected"}})
	if _, out := state(t, h, token); out["result"].(map[string]any)["saleRequestState"] != "Rejected" {
		t.Fatalf("forced state = %v", out)
	}
	post(t, h, "/api/v1/test/config", map[string]any{"force": map[string]string{"verify": "", "create": "502"}})
	if status, _ := post(t, h, "/api/Gateway/RegisterPayment", register("5", "", 1)); status != 502 {
		t.Fatalf("forced create = %d", status)
	}
}

func TestPayPageAndCallback(t *testing.T) {
	var seen *url.URL
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL
		w.WriteHeader(http.StatusFound)
	}))
	defer merchant.Close()

	h := newTestApp()
	token := create(t, h, "1001", merchant.URL+"/v1/gateway/callback/hash-1")

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/pay/"+token, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "12,500,000") {
		t.Fatalf("pay page = %d", page.Code)
	}
	click := httptest.NewRequest("POST", "/pay/"+token, strings.NewReader("outcome=pay"))
	click.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, click)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || loc.Path != "/v1/gateway/callback/hash-1" || loc.Query().Get("token") != token {
		t.Fatalf("pay = %d %s", w.Code, loc)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/test/orders/"+token+"/callback", nil))
	if seen == nil || seen.Query().Get("token") != token {
		t.Fatalf("the merchant saw %v", seen)
	}

	view := httptest.NewRecorder()
	h.ServeHTTP(view, httptest.NewRequest("GET", "/api/v1/test/orders/"+token, nil))
	var v map[string]any
	json.Unmarshal(view.Body.Bytes(), &v)
	if v["sale_request_state"] != "PaymentCompleted" || v["calls"].(map[string]any)["callback"] != float64(2) ||
		v["request"].(map[string]any)["shoppingCardCode"] != "1001" {
		t.Fatalf("view = %v", v)
	}
}

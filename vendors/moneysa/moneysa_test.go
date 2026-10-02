package moneysa

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
	user = "test-user"
	pass = "test-pass"
)

func newTestApp() http.Handler {
	h, _ := core.Handler(&Vendor{username: user, password: pass}, "http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
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

// The payload the merchant's moneysa driver builds.
func tokenBody(tracking, callback string) map[string]any {
	return map[string]any{
		"username": user, "password": pass, "national_id": "0012345678", "branch_code": 1,
		"callback": callback, "amount": 12_500_000, "tracking_code": tracking,
	}
}

type tx struct{ token, id string }

func create(t *testing.T, h http.Handler, tracking, callback string) tx {
	t.Helper()
	status, out := post(t, h, "/api/payments/get-token", tokenBody(tracking, callback))
	if status != 200 || out["token"] == nil || out["tracking_code"] != tracking {
		t.Fatalf("get-token = %d %v", status, out)
	}
	id := out["transaction_id"].(string) // a string here, a number on verify
	if out["url"] != "http://gw.test/pay/"+id {
		t.Fatalf("url = %v", out["url"])
	}
	return tx{out["token"].(string), id}
}

// verify sends transaction_id as a number, as the driver does (Number(...)).
func verify(t *testing.T, h http.Handler, path string, x tx, amount int) (int, map[string]any) {
	t.Helper()
	return post(t, h, path, json.RawMessage(`{"token":"`+x.token+`","transaction_id":`+x.id+`,"amount":`+itoa(amount)+`}`))
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// refund sends transaction_id as a string, as the driver does (String(...)).
func refund(t *testing.T, h http.Handler, x tx, amount int, password string) (int, map[string]any) {
	t.Helper()
	return post(t, h, "/api/payments/refund", map[string]any{
		"username": user, "password": password, "token": x.token, "transaction_id": x.id, "amount": amount,
	})
}

func TestCredentialsAreChecked(t *testing.T) {
	h := newTestApp()
	x := create(t, h, "1", "")
	body := tokenBody("2", "")
	body["password"] = "wrong"
	if status, out := post(t, h, "/api/payments/get-token", body); status != 401 || out["code"] != float64(1011) {
		t.Fatalf("get-token = %d %v", status, out)
	}
	if status, out := refund(t, h, x, 1, "wrong"); status != 401 || out["code"] != float64(1011) {
		t.Fatalf("refund = %d %v", status, out)
	}
}

func TestGetTokenValidation(t *testing.T) {
	h := newTestApp()
	create(t, h, "1001", "")
	for name, mutate := range map[string]func(map[string]any){
		"duplicate tracking code": func(map[string]any) {},
		"short national id":       func(b map[string]any) { b["tracking_code"] = "2"; b["national_id"] = "12" },
		"tiny amount":             func(b map[string]any) { b["tracking_code"] = "3"; b["amount"] = 999 },
	} {
		body := tokenBody("1001", "")
		mutate(body)
		if status, out := post(t, h, "/api/payments/get-token", body); status != 422 || out["errors"] == nil {
			t.Errorf("%s = %d %v", name, status, out)
		}
	}
}

// The production facts of 2026-08-13: unpaid verify is 400 2002, a settled
// one answers status 10, and verifying it again is 400 2001.
func TestVerifyMatchesProduction(t *testing.T) {
	h := newTestApp()
	x := create(t, h, "1001", "")

	for _, tc := range []struct {
		name   string
		x      tx
		amount int
		status int
		code   float64
	}{
		{"unpaid", x, 12_500_000, 400, 2002},
		{"unknown transaction", tx{x.token, "1"}, 12_500_000, 404, 1001},
		{"wrong token", tx{"nope", x.id}, 12_500_000, 400, 1003},
		{"wrong amount", x, 1_000, 400, 2003},
	} {
		if status, out := verify(t, h, "/api/payments/verify", tc.x, tc.amount); status != tc.status || out["code"] != tc.code {
			t.Errorf("%s = %d %v", tc.name, status, out)
		}
	}

	if _, out := verify(t, h, "/api/payments/verify-information", x, 12_500_000); out["status"] != float64(2) {
		t.Fatalf("inquiry while pending = %v", out)
	}
	post(t, h, "/api/v1/test/orders/"+x.id+"/settle", nil)

	status, out := verify(t, h, "/api/payments/verify", x, 12_500_000)
	if status != 200 || out["status"] != float64(10) || out["status_label"] != "verified" || out["status_label_fa"] != "پرداخت شده" ||
		out["token"] != x.token || out["amount"] != float64(12_500_000) || out["transaction_id"] != toFloat(x.id) {
		t.Fatalf("verify settled = %d %v", status, out)
	}
	status, out = verify(t, h, "/api/payments/verify", x, 12_500_000)
	if status != 400 || out["code"] != float64(2001) || out["message"] != "تراکنش قبلا تسویه شده." {
		t.Fatalf("verify twice = %d %v", status, out)
	}
	if status, out := verify(t, h, "/api/payments/verify-information", x, 12_500_000); status != 200 || out["status"] != float64(10) {
		t.Fatalf("inquiry after verify = %d %v", status, out)
	}
}

func toFloat(s string) float64 {
	var f float64
	json.Unmarshal([]byte(s), &f)
	return f
}

func TestRefund(t *testing.T) {
	h := newTestApp()
	x := create(t, h, "1001", "")
	if status, out := refund(t, h, x, 12_500_000, pass); status != 400 || out["code"] != float64(2002) {
		t.Fatalf("refund unpaid = %d %v", status, out)
	}
	post(t, h, "/api/v1/test/orders/"+x.id+"/settle", nil)
	verify(t, h, "/api/payments/verify", x, 12_500_000)

	if status, out := refund(t, h, x, 12_500_001, pass); status != 400 || out["code"] != float64(2006) {
		t.Fatalf("refund too much = %d %v", status, out)
	}
	status, out := refund(t, h, x, 12_500_000, pass)
	if status != 200 || out["status"] != float64(3) || out["status_label"] != "refunded" || out["token"] != x.token {
		t.Fatalf("refund = %d %v", status, out)
	}
	if status, out := refund(t, h, x, 12_500_000, pass); status != 400 || out["code"] != float64(2002) {
		t.Fatalf("refund twice = %d %v", status, out)
	}
	if _, out := verify(t, h, "/api/payments/verify-information", x, 12_500_000); out["status"] != float64(3) {
		t.Fatalf("inquiry after refund = %v", out)
	}
}

func TestForced(t *testing.T) {
	h := newTestApp()
	x := create(t, h, "1001", "")
	post(t, h, "/api/v1/test/orders/"+x.id+"/settle", nil)
	for _, tc := range []struct {
		op, value string
		status    int
		code      float64
	}{
		{"verify", "2001", 400, 2001},
		{"verify", "1001", 404, 1001},
		{"inquiry", "503", 503, 0},
		{"create", "1012", 401, 1012},
	} {
		post(t, h, "/api/v1/test/config", map[string]any{"force": map[string]string{tc.op: tc.value}})
		var status int
		var out map[string]any
		switch tc.op {
		case "verify":
			status, out = verify(t, h, "/api/payments/verify", x, 12_500_000)
		case "inquiry":
			status, out = verify(t, h, "/api/payments/verify-information", x, 12_500_000)
		default:
			status, out = post(t, h, "/api/payments/get-token", tokenBody("2", ""))
		}
		if status != tc.status || tc.code != 0 && out["code"] != tc.code {
			t.Errorf("force %s=%s = %d %v", tc.op, tc.value, status, out)
		}
		post(t, h, "/api/v1/test/config", map[string]any{})
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
	x := create(t, h, "1001", merchant.URL+"/v1/gateway/callback/hash-1")

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/pay/"+x.id, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "12,500,000") {
		t.Fatalf("pay page = %d", page.Code)
	}
	click := httptest.NewRequest("POST", "/pay/"+x.id, strings.NewReader("outcome=pay"))
	click.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, click)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || loc.Path != "/v1/gateway/callback/hash-1" || loc.Query().Get("token") != x.token {
		t.Fatalf("pay = %d %s", w.Code, loc)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/test/orders/"+x.id+"/callback", nil))
	if seen == nil || seen.Query().Get("token") != x.token {
		t.Fatalf("the merchant saw %v", seen)
	}

	view := httptest.NewRecorder()
	h.ServeHTTP(view, httptest.NewRequest("GET", "/api/v1/test/orders/"+x.id, nil))
	var v map[string]any
	json.Unmarshal(view.Body.Bytes(), &v)
	if v["token"] != x.token || v["calls"].(map[string]any)["callback"] != float64(2) ||
		v["request"].(map[string]any)["tracking_code"] != "1001" {
		t.Fatalf("view = %v", v)
	}
}

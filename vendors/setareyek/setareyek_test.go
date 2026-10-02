package setareyek

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
	"time"

	"github.com/47vigen/fakeGateway/core"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type app struct {
	t     *testing.T
	h     http.Handler
	token string
}

func newApp(t *testing.T) *app {
	t.Helper()
	h, _ := core.Handler(newVendor("user", "pass", "m-1"), "http://sy.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
	a := &app{t: t, h: h}
	w := a.get("/GetToken?username=user&password=pass")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &a.token) != nil || a.token == "" {
		t.Fatalf("token = %d %s", w.Code, w.Body)
	}
	return a
}

func (a *app) get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func (a *app) post(path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func purchaseBody(callback string) map[string]any {
	return map[string]any{
		"Merchant": "m-1", "Amount": 150_000, "CallbackUrl": callback, "Mobile": "09120000000",
		"Orders": []map[string]any{{"OrderId": "p1", "Name": "Item", "Amount": 150_000}},
	}
}

func (a *app) purchase(callback string) string {
	a.t.Helper()
	got := decodeJSON(a.t, a.post("/Purchase", purchaseBody(callback)))
	if got["Result"] != float64(100) || got["Url"] != "http://sy.test/pay/"+got["TrackId"].(string) {
		a.t.Fatalf("purchase = %v", got)
	}
	return got["TrackId"].(string)
}

func (a *app) op(path, trackID string) map[string]any {
	a.t.Helper()
	return decodeJSON(a.t, a.post(path, map[string]any{"Merchant": "m-1", "TrackId": trackID}))
}

func (a *app) refund(trackID string) map[string]any {
	a.t.Helper()
	return decodeJSON(a.t, a.post("/RefundPayment", map[string]any{
		"Merchant": "m-1", "TrackId": trackID, "Amount": 150_000,
		"Orders": []map[string]any{{"OrderId": "1001", "Name": "refund", "Amount": 150_000}},
	}))
}

func TestToken(t *testing.T) {
	a := newApp(t)
	if w := a.get("/GetToken?username=user&password=wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", w.Code)
	}
	for _, tok := range []string{"", "forged"} {
		a.token = tok
		if w := a.post("/Purchase", purchaseBody("https://x.test")); w.Code != http.StatusUnauthorized {
			t.Fatalf("bearer %q = %d", tok, w.Code)
		}
	}
}

func TestPurchaseRefusals(t *testing.T) {
	a := newApp(t)
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want float64
	}{
		{"wrong merchant", func(b map[string]any) { b["Merchant"] = "other" }, 102},
		{"zero amount", func(b map[string]any) { b["Amount"] = 0 }, 106},
		{"relative callback", func(b map[string]any) { b["CallbackUrl"] = "/back" }, 107},
		{"no orders", func(b map[string]any) { b["Orders"] = []any{} }, 108},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := purchaseBody("https://x.test")
			tc.edit(b)
			w := a.post("/Purchase", b)
			if w.Code != http.StatusOK || decodeJSON(t, w)["Result"] != tc.want {
				t.Fatalf("= %d %s, want Result %v", w.Code, w.Body, tc.want)
			}
		})
	}
}

// The page, the GET back, then inquire, verify and refund answering by the
// loan's state.
func TestLoanLifecycle(t *testing.T) {
	a := newApp(t)
	id := a.purchase("https://merchant.test/v1/gateway/callback/h1")

	page := a.get("/pay/" + id)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "150,000") {
		t.Fatalf("pay page = %d", page.Code)
	}

	check := func(name string, got map[string]any, want map[string]any) {
		t.Helper()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: %s = %v, want %v (%v)", name, k, got[k], v, got)
			}
		}
	}
	check("inquire before payment", a.op("/Inquiry", id), map[string]any{"Result": float64(100), "Status": float64(1)})
	check("verify before payment", a.op("/Verify", id), map[string]any{"Result": float64(116), "PaidAt": nil})
	check("refund before verify", a.refund(id), map[string]any{"Result": float64(116), "Status": float64(1)})
	check("verify unknown", a.op("/Verify", "nope"), map[string]any{"Result": float64(115)})
	check("verify for another merchant", decodeJSON(t, a.post("/Verify", map[string]any{"Merchant": "x", "TrackId": id})), map[string]any{"Result": float64(102)})

	req := httptest.NewRequest("POST", "/pay/"+id, strings.NewReader("outcome=pay"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || loc.Path != "/v1/gateway/callback/h1" || loc.Query().Get("TrackId") != id || loc.Query().Get("Status") != "102" {
		t.Fatalf("return = %d %s", w.Code, loc)
	}

	check("inquire after payment", a.op("/Inquiry", id), map[string]any{"Status": float64(102)})
	check("verify", a.op("/Verify", id), map[string]any{"Result": float64(100), "Status": "104", "Amount": float64(150_000)})
	check("verify again", a.op("/Verify", id), map[string]any{"Result": float64(100)})
	check("inquire after verify", a.op("/Inquiry", id), map[string]any{"Status": float64(104)})
	check("refund", a.refund(id), map[string]any{"Result": float64(100), "Status": float64(0)})
	check("refund again", a.refund(id), map[string]any{"Result": float64(100), "Status": float64(1)})
	check("inquire after refund", a.op("/Inquiry", id), map[string]any{"Status": float64(106)})
	check("verify after refund", a.op("/Verify", id), map[string]any{"Result": float64(116)})

	order := decodeJSON(t, a.get("/api/v1/test/orders/"+id))
	calls := order["calls"].(map[string]any)
	if order["state"] != "refunded" || calls["verify"] != float64(4) || calls["inquire"] != float64(4) || calls["refund"] != float64(3) || calls["callback"] != float64(1) {
		t.Fatalf("order = %v", order)
	}
}

func TestControlFailThenVerify(t *testing.T) {
	a := newApp(t)
	id := a.purchase("https://x.test/back")
	a.post("/api/v1/test/orders/"+id+"/fail", nil)
	if got := a.op("/Verify", id); got["Result"] != float64(116) || got["Status"] != "103" {
		t.Fatalf("verify failed loan = %v", got)
	}
	a.post("/api/v1/test/orders/"+id+"/settle", nil)
	if got := a.op("/Verify", id); got["Result"] != float64(100) {
		t.Fatalf("verify after settle = %v", got)
	}
}

func TestForceAndDelay(t *testing.T) {
	a := newApp(t)
	id := a.purchase("https://x.test/back")
	a.post("/api/v1/test/config", map[string]any{"force": map[string]string{"verify": "120", "refund": "118", "inquire": "502"}})
	if got := a.op("/Verify", id); got["Result"] != float64(120) {
		t.Fatalf("forced verify = %v", got)
	}
	if got := a.refund(id); got["Result"] != float64(118) || got["Status"] != float64(1) {
		t.Fatalf("forced refund = %v", got)
	}
	if w := a.post("/Inquiry", map[string]any{"Merchant": "m-1", "TrackId": id}); w.Code != http.StatusBadGateway {
		t.Fatalf("forced inquire = %d", w.Code)
	}
	a.post("/api/v1/test/config", map[string]any{})
	a.post("/api/v1/test/config", map[string]any{"delay_ms": 30})
	start := time.Now()
	a.op("/Inquiry", id)
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("delay_ms did not delay inquire")
	}
}

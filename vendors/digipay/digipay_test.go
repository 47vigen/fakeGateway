package digipay

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const hash = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"

type app struct {
	t     *testing.T
	h     http.Handler
	token string
}

func newApp(t *testing.T, purchaseType int) *app {
	t.Helper()
	h, _ := core.Handler(newVendor("cid", "csecret", "user", "pass", purchaseType), "http://dp.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
	a := &app{t: t, h: h}
	w := a.tokenRequest("cid", "csecret", url.Values{"grant_type": {"password"}, "username": {"user"}, "password": {"pass"}})
	if w.Code != http.StatusOK {
		t.Fatalf("token = %d: %s", w.Code, w.Body)
	}
	a.token = decodeJSON(t, w)["access_token"].(string)
	return a
}

func (a *app) tokenRequest(id, secret string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	return w
}

func (a *app) call(path string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
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

func (a *app) control(path string, body any) map[string]any {
	a.t.Helper()
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	return decodeJSON(a.t, w)
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

// status is result.status of a Digipay answer.
func status(t *testing.T, w *httptest.ResponseRecorder) float64 {
	t.Helper()
	res, _ := decodeJSON(t, w)["result"].(map[string]any)
	return res["status"].(float64)
}

func ticketBody(providerID, callback string) map[string]any {
	return map[string]any{
		"amount": 1_500_000, "callbackUrl": callback, "providerId": providerID, "cellNumber": "09120000000",
		"basketDetailsDto": map[string]any{"basketId": "1001", "items": []map[string]any{
			{"count": 1, "productCode": "p1", "sellerId": "MERCHANT", "supplierId": "MERCHANT", "productType": 1, "brand": "B", "categoryId": "C"},
		}},
	}
}

// create opens a ticket and returns it with the trackingCode the shopper's
// callback would carry.
func (a *app) create(providerID string) (ticket, trackingCode string) {
	a.t.Helper()
	w := a.call("/tickets/business?type=11", ticketBody(providerID, "https://merchant.test/v1/gateway/callback/"+providerID))
	if w.Code != http.StatusOK || status(a.t, w) != 0 {
		a.t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	got := decodeJSON(a.t, w)
	ticket = got["ticket"].(string)
	if got["redirectUrl"] != "http://dp.test/web-pay/tgs/"+ticket {
		a.t.Fatalf("redirectUrl = %v", got["redirectUrl"])
	}
	order := httptest.NewRecorder()
	a.h.ServeHTTP(order, httptest.NewRequest("GET", "/api/v1/test/orders/"+ticket, nil))
	return ticket, decodeJSON(a.t, order)["tracking_code"].(string)
}

func (a *app) verify(trackingCode string, typ string) *httptest.ResponseRecorder {
	return a.call("/purchases/verify?type="+typ, map[string]any{"trackingCode": trackingCode, "providerId": hash})
}

func TestToken(t *testing.T) {
	a := newApp(t, typeCredit)
	refresh := decodeJSON(t, a.tokenRequest("cid", "csecret", url.Values{"grant_type": {"password"}, "username": {"user"}, "password": {"pass"}}))["refresh_token"].(string)
	for _, tc := range []struct {
		name       string
		id, secret string
		form       url.Values
		want       int
		wantErr    string
	}{
		{"password grant", "cid", "csecret", url.Values{"grant_type": {"password"}, "username": {"user"}, "password": {"pass"}}, 200, ""},
		{"refresh grant", "cid", "csecret", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, 200, ""},
		{"wrong client", "cid", "nope", url.Values{"grant_type": {"password"}, "username": {"user"}, "password": {"pass"}}, 401, "unauthorized"},
		{"wrong password", "cid", "csecret", url.Values{"grant_type": {"password"}, "username": {"user"}, "password": {"x"}}, 400, "invalid_grant"},
		{"unknown refresh", "cid", "csecret", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}}, 400, "invalid_grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := a.tokenRequest(tc.id, tc.secret, tc.form)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			got := decodeJSON(t, w)
			if tc.wantErr != "" && got["error"] != tc.wantErr {
				t.Fatalf("error = %v, want %s", got["error"], tc.wantErr)
			}
			if tc.wantErr == "" && (got["access_token"] == "" || got["token_type"] != "bearer") {
				t.Fatalf("token body = %v", got)
			}
		})
	}

	// A missing or unknown bearer is refused with Digipay's 401 shape.
	for _, tok := range []string{"", "forged"} {
		a.token = tok
		w := a.call("/tickets/business?type=11", ticketBody("p", "https://x.test"))
		if w.Code != http.StatusUnauthorized || decodeJSON(t, w)["error"] != "invalid_token" {
			t.Fatalf("token %q: %d %s", tok, w.Code, w.Body)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	a := newApp(t, typeCredit)
	a.create(hash)
	noCell := ticketBody("other", "https://x.test")
	delete(noCell, "cellNumber")
	for _, tc := range []struct {
		name     string
		path     string
		body     any
		code     int
		dpStatus float64
	}{
		{"ticket type is not 11", "/tickets/business?type=5", ticketBody("other", "https://x.test"), 400, 1054},
		{"no cellNumber", "/tickets/business?type=11", noCell, 400, 1054},
		{"providerId reused", "/tickets/business?type=11", ticketBody(hash, "https://x.test"), 422, 9008},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := a.call(tc.path, tc.body)
			if w.Code != tc.code || status(t, w) != tc.dpStatus {
				t.Fatalf("= %d %s, want %d/%v", w.Code, w.Body, tc.code, tc.dpStatus)
			}
		})
	}
}

// The page, the POST back with the fields the driver reads, then verify,
// deliver and refund, each answering by the purchase's state.
func TestPurchaseLifecycle(t *testing.T) {
	a := newApp(t, typeCredit)
	ticket, tc := a.create(hash)

	page := httptest.NewRecorder()
	a.h.ServeHTTP(page, httptest.NewRequest("GET", "/web-pay/tgs/"+ticket, nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "1,500,000") {
		t.Fatalf("pay page = %d", page.Code)
	}

	steps := []struct {
		name     string
		do       func() *httptest.ResponseRecorder
		code     int
		dpStatus float64
	}{
		{"verify before payment", func() *httptest.ResponseRecorder { return a.verify(tc, "5") }, 422, 9004},
		{"deliver before verify", func() *httptest.ResponseRecorder { return a.deliverCall(tc, "5") }, 422, 9012},
		{"pay", func() *httptest.ResponseRecorder { return a.pay(ticket, tc) }, 200, 0},
		{"verify unknown code", func() *httptest.ResponseRecorder { return a.verify("123", "5") }, 422, 9000},
		{"verify under the wrong type", func() *httptest.ResponseRecorder { return a.verify(tc, "11") }, 400, 1054},
		{"verify", func() *httptest.ResponseRecorder { return a.verify(tc, "5") }, 200, 0},
		{"verify again", func() *httptest.ResponseRecorder { return a.verify(tc, "5") }, 422, 9012},
		{"deliver", func() *httptest.ResponseRecorder { return a.deliverCall(tc, "5") }, 200, 0},
		{"deliver again", func() *httptest.ResponseRecorder { return a.deliverCall(tc, "5") }, 422, 9012},
		{"refund under the purchase providerId", func() *httptest.ResponseRecorder { return a.refundCall(tc, hash) }, 400, 1054},
		{"refund", func() *httptest.ResponseRecorder { return a.refundCall(tc, "refund-"+hash) }, 200, 0},
		{"refund again", func() *httptest.ResponseRecorder { return a.refundCall(tc, "refund-"+hash) }, 422, 9012},
		{"verify a refunded purchase", func() *httptest.ResponseRecorder { return a.verify(tc, "5") }, 422, 9012},
	}
	for _, s := range steps {
		w := s.do()
		if s.name == "pay" {
			continue
		}
		if w.Code != s.code || status(t, w) != s.dpStatus {
			t.Fatalf("%s = %d %s, want %d/%v", s.name, w.Code, w.Body, s.code, s.dpStatus)
		}
	}

	got := decodeJSON(t, func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/test/orders/"+ticket, nil))
		return w
	}())
	calls := got["calls"].(map[string]any)
	if got["state"] != "refunded" || got["delivered"] != true || calls["verify"] != float64(5) || calls["refund"] != float64(3) || calls["callback"] != float64(1) {
		t.Fatalf("order = %v", got)
	}
}

// pay clicks the page's pay button and checks the auto-posted return form.
func (a *app) pay(ticket, trackingCode string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest("POST", "/web-pay/tgs/"+ticket, strings.NewReader("outcome=pay"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	back := w.Body.String()
	for _, want := range []string{
		`action="https://merchant.test/v1/gateway/callback/` + hash + `"`,
		`name="result" value="SUCCESS"`, `name="providerId" value="` + hash + `"`,
		`name="trackingCode" value="` + trackingCode + `"`, `name="amount" value="1500000"`,
		`name="type" value="5"`, `name="isCredit" value="true"`,
	} {
		if !strings.Contains(back, want) {
			a.t.Fatalf("return form lacks %s:\n%s", want, back)
		}
	}
	return w
}

func (a *app) deliverCall(trackingCode, typ string) *httptest.ResponseRecorder {
	return a.call("/purchases/deliver?type="+typ, map[string]any{
		"trackingCode": trackingCode, "deliveryDate": time.Now().UnixMilli(), "invoiceNumber": "1001", "products": []string{"p1"},
	})
}

func (a *app) refundCall(trackingCode, providerID string) *httptest.ResponseRecorder {
	return a.call("/refunds?type=5", map[string]any{"providerId": providerID, "amount": 1_500_000, "saleTrackingCode": trackingCode})
}

func TestVerifyAnswersTheBindingFields(t *testing.T) {
	a := newApp(t, typeCredit)
	ticket, tc := a.create(hash)
	a.control("/api/v1/test/orders/"+ticket+"/settle", nil)
	got := decodeJSON(t, a.verify(tc, "5"))
	if got["providerId"] != hash || got["amount"] != float64(1_500_000) || got["paymentGateway"] != float64(4) || got["trackingCode"] != tc {
		t.Fatalf("verify = %v", got)
	}
}

func TestFailedAndWalletPurchases(t *testing.T) {
	a := newApp(t, typeWallet)
	ticket, tc := a.create(hash)
	a.control("/api/v1/test/orders/"+ticket+"/fail", nil)
	if w := a.verify(tc, "11"); w.Code != 422 || status(t, w) != 9007 {
		t.Fatalf("verify failed purchase = %d %s", w.Code, w.Body)
	}
	a.control("/api/v1/test/orders/"+ticket+"/settle", nil)
	if w := a.verify(tc, "11"); w.Code != 200 || decodeJSON(t, w)["paymentGateway"] != float64(3) {
		t.Fatalf("verify wallet = %d %s", w.Code, w.Body)
	}
	// Deliver is for credit purchases only.
	if w := a.deliverCall(tc, "11"); w.Code != 422 || status(t, w) != 9012 {
		t.Fatalf("deliver wallet = %d %s", w.Code, w.Body)
	}
}

// The control API posts the callback the way Digipay's page would.
func TestControlCallbackPosts(t *testing.T) {
	var got url.Values
	shop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		http.Redirect(w, r, "https://front.test/done", http.StatusFound)
	}))
	defer shop.Close()

	a := newApp(t, typeIPG)
	w := a.call("/tickets/business?type=11", ticketBody(hash, shop.URL+"/cb"))
	ticket := decodeJSON(t, w)["ticket"].(string)
	out := a.control("/api/v1/test/orders/"+ticket+"/fail?callback=1", nil)
	if cb := out["callback"].(map[string]any); cb["location"] != "https://front.test/done" {
		t.Fatalf("callback = %v", cb)
	}
	if got.Get("result") != "FAILURE" || got.Get("providerId") != hash || got.Get("type") != "0" || !regexp.MustCompile(`^\d{12}$`).MatchString(got.Get("rrn")) {
		t.Fatalf("shop got %v", got)
	}
}

func TestForceAndDelay(t *testing.T) {
	a := newApp(t, typeCredit)
	_, tc := a.create(hash)
	a.control("/api/v1/test/config", map[string]any{"force": map[string]string{"verify": "9011"}})
	if w := a.verify(tc, "5"); w.Code != 422 || status(t, w) != 9011 {
		t.Fatalf("forced 9011 = %d %s", w.Code, w.Body)
	}
	a.control("/api/v1/test/config", map[string]any{"force": map[string]string{"verify": "", "refund": "503"}})
	if w := a.refundCall(tc, "r"); w.Code != 503 {
		t.Fatalf("forced 503 = %d", w.Code)
	}
	a.control("/api/v1/test/config", map[string]any{"force": map[string]string{"refund": ""}, "delay_ms": 30})
	start := time.Now()
	a.verify(tc, "5")
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("delay_ms did not delay verify")
	}
}

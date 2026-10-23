package zarinplus

import (
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

const authority = "A00000000000000000000000000000000001"

func newTestApp() http.Handler {
	h, _ := core.Handler(&Vendor{token: "tok"}, "http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
	return h
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func requestBody(success, cancel string) string {
	return `{"token":"tok","amount":1010000,"success":"` + success + `","cancel":"` + cancel +
		`","item":"پرداخت تراکنش #1001","cellphone":"09120000000","gateway_slug":"zarinplus"}`
}

func verifyBody(amount string) string {
	return `{"token":"tok","authority":"` + authority + `","amount":` + amount + `}`
}

func TestRequestCall(t *testing.T) {
	h := newTestApp()
	w := do(h, "POST", pathRequest, requestBody("https://shop.test/back", "https://shop.test/back"))
	for _, want := range []string{`"status":true`, `"authority":"` + authority + `"`, `"redirect_url":"http://gw.test/pay/` + authority + `"`} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("request: %d %s, lacks %s", w.Code, w.Body, want)
		}
	}
	for name, tc := range map[string]struct{ body, message string }{
		"wrong token": {strings.Replace(requestBody("a", "b"), `"tok"`, `"nope"`, 1), "Invalid token"},
		"no amount":   {strings.Replace(requestBody("a", "b"), `"amount":1010000,`, "", 1), "invalid parameters"},
		"wrong slug":  {strings.Replace(requestBody("a", "b"), `"zarinplus"`, `"other"`, 1), "invalid parameters"},
		"not json":    {`nope`, "invalid JSON"},
	} {
		w := do(h, "POST", pathRequest, tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"status":false`) || !strings.Contains(w.Body.String(), tc.message) {
			t.Errorf("%s: %d %s, want 400 %q", name, w.Code, w.Body, tc.message)
		}
	}
}

// A path without its slash answers 301, which a client that does not follow
// redirects takes as the answer.
func TestTrailingSlashRedirects(t *testing.T) {
	h := newTestApp()
	for _, path := range []string{pathRequest, pathVerify} {
		w := do(h, "POST", strings.TrimSuffix(path, "/"), "{}")
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != path {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestVerifyCall(t *testing.T) {
	h := newTestApp()
	do(h, "POST", pathRequest, requestBody("https://shop.test/back", "https://shop.test/back"))

	unpaid := do(h, "POST", pathVerify, verifyBody("1010000"))
	if unpaid.Code != http.StatusBadRequest || !strings.Contains(unpaid.Body.String(), "User or Tx not exist") {
		t.Fatalf("verify before payment: %d %s", unpaid.Code, unpaid.Body)
	}
	do(h, "POST", "/api/v1/test/orders/"+authority+"/settle", "")

	if w := do(h, "POST", pathVerify, verifyBody("1010001")); w.Code != http.StatusBadRequest {
		t.Fatalf("verify with another amount: %d %s", w.Code, w.Body)
	}
	for _, code := range []string{`"code":200`, `"code":201`} {
		w := do(h, "POST", pathVerify, verifyBody("1010000"))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), code) || !strings.Contains(w.Body.String(), `"reference":"ref-1"`) {
			t.Fatalf("verify: %d %s, want %s", w.Code, w.Body, code)
		}
	}
	if w := do(h, "POST", pathVerify, strings.Replace(verifyBody("1010000"), `"tok"`, `"nope"`, 1)); !strings.Contains(w.Body.String(), "Invalid token") {
		t.Fatalf("verify with a wrong token: %s", w.Body)
	}
}

// The shopper pays or cancels on the page: the browser comes back by GET with
// ?authority= on success and with nothing on cancel.
func TestPayPageAndCallback(t *testing.T) {
	h := newTestApp()
	do(h, "POST", pathRequest, requestBody("https://shop.test/ok?order=7", "https://shop.test/cancel?order=7"))

	page := do(h, "GET", "/pay/"+authority, "").Body.String()
	for _, want := range []string{"1,010,000", `value="pay"`, `value="cancel"`, "پرداخت تراکنش #1001"} {
		if !strings.Contains(page, want) {
			t.Fatalf("pay page lacks %s", want)
		}
	}

	click := func(outcome string) *url.URL {
		req := httptest.NewRequest("POST", "/pay/"+authority, strings.NewReader("outcome="+outcome))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s click = %d, want 303", outcome, w.Code)
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	cancel := click("cancel")
	if cancel.Host != "shop.test" || cancel.Path != "/cancel" || cancel.Query().Get("order") != "7" || cancel.Query().Has("authority") {
		t.Fatalf("cancel returned to %s", cancel)
	}
	if w := do(h, "POST", pathVerify, verifyBody("1010000")); w.Code != http.StatusBadRequest {
		t.Fatalf("verify after a cancel: %d %s", w.Code, w.Body)
	}
	pay := click("pay")
	if pay.Path != "/ok" || pay.Query().Get("order") != "7" || pay.Query().Get("authority") != authority {
		t.Fatalf("pay returned to %s", pay)
	}
	if w := do(h, "POST", pathVerify, verifyBody("1010000")); w.Code != http.StatusOK {
		t.Fatalf("verify after the pay click: %d %s", w.Code, w.Body)
	}
}

func TestForce(t *testing.T) {
	h := newTestApp()
	do(h, "POST", "/api/v1/test/config", `{"force":{"request":"503","verify":"Invalid token"}}`)
	if w := do(h, "POST", pathRequest, requestBody("a", "b")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("forced request = %d", w.Code)
	}
	if w := do(h, "POST", pathVerify, verifyBody("1")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Invalid token") {
		t.Fatalf("forced verify = %d %s", w.Code, w.Body)
	}
	do(h, "POST", "/api/v1/test/config", `{"force":{"request":"","verify":""}}`)
	if w := do(h, "POST", pathRequest, requestBody("a", "b")); w.Code != http.StatusOK {
		t.Fatalf("request after clearing = %d", w.Code)
	}
}

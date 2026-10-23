package sep

import (
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

func newTestApp() http.Handler {
	h, _ := core.Handler(&Vendor{terminal: "7002"}, "http://gw.test", core.Behaviour{DefaultOutcome: core.OutcomeManual})
	return h
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func tokenBody(resNum, redirect string) string {
	return `{"action":"token","TerminalId":"7002","Amount":1010000,"ResNum":"` + resNum + `","RedirectUrl":"` + redirect + `","CellNumber":"9120000000"}`
}

// issue asks for a token and answers it.
func issue(t *testing.T, h http.Handler, resNum, redirect string) string {
	t.Helper()
	w := post(h, pathToken, tokenBody(resNum, redirect))
	var out struct {
		Status int
		Token  string
	}
	if err := jsonDecode(w, &out); err != nil || out.Status != 1 || out.Token == "" {
		t.Fatalf("token answered %s (%v)", w.Body, err)
	}
	return out.Token
}

func TestTokenCall(t *testing.T) {
	h := newTestApp()
	issue(t, h, "res-1", "https://shop.test/back")

	for name, tc := range map[string]struct{ body, code string }{
		"unknown terminal": {`{"action":"token","TerminalId":"9999","Amount":1,"ResNum":"r","RedirectUrl":"https://x"}`, "12"},
		"no amount":        {`{"action":"token","TerminalId":"7002","ResNum":"r","RedirectUrl":"https://x"}`, "5"},
		"no redirect":      {`{"action":"token","TerminalId":"7002","Amount":1,"ResNum":"r"}`, "5"},
		"not json":         {`nope`, "5"},
	} {
		w := post(h, pathToken, tc.body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":-1`) || !strings.Contains(w.Body.String(), `"errorCode":"`+tc.code+`"`) {
			t.Errorf("%s: %d %s, want errorCode %s", name, w.Code, w.Body, tc.code)
		}
	}

	got := get(h, "/api/v1/test/orders/res-1")
	if !strings.Contains(got, `"amount":1010000`) || !strings.Contains(got, `"CellNumber":"9120000000"`) || !strings.Contains(got, `"token":1`) {
		t.Fatalf("the order view lacks the request or the ledger: %s", got)
	}
}

func TestVerifyAndReverse(t *testing.T) {
	h := newTestApp()
	issue(t, h, "res-2", "https://shop.test/back")
	refNum := "refnum-1-res-2"
	body := `{"RefNum":"` + refNum + `","TerminalNumber":7002}`

	// Not paid yet: nothing to verify.
	if w := post(h, pathVerify, body); !strings.Contains(w.Body.String(), `"ResultCode":-2`) {
		t.Fatalf("verify before payment: %s", w.Body)
	}
	// The refused call still reaches the ledger.
	if got := get(h, "/api/v1/test/orders/res-2"); !strings.Contains(got, `"verify":1`) {
		t.Fatalf("an unpaid verify is not in the ledger: %s", got)
	}
	post(h, "/api/v1/test/orders/res-2/settle", "")

	w := post(h, pathVerify, body)
	for _, want := range []string{`"Success":true`, `"ResultCode":0`, `"OrginalAmount":1010000`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("verify lacks %s: %s", want, w.Body)
		}
	}
	// The repeat is a capture too: ResultCode 2 with the same detail.
	if w := post(h, pathVerify, body); !strings.Contains(w.Body.String(), `"ResultCode":2`) || !strings.Contains(w.Body.String(), `"OrginalAmount":1010000`) {
		t.Fatalf("second verify: %s", w.Body)
	}
	if w := post(h, pathVerify, `{"RefNum":"`+refNum+`","TerminalNumber":1}`); !strings.Contains(w.Body.String(), `"ResultCode":-105`) {
		t.Fatalf("wrong terminal: %s", w.Body)
	}

	if w := post(h, pathReverse, body); !strings.Contains(w.Body.String(), `"Success":true`) {
		t.Fatalf("reverse: %s", w.Body)
	}
	if w := post(h, pathVerify, body); !strings.Contains(w.Body.String(), `"Success":false`) || !strings.Contains(w.Body.String(), `"ResultCode":5`) {
		t.Fatalf("verify after reverse: %s", w.Body)
	}
}

// The shopper pays or cancels on the page; the browser then posts SEP's form.
func TestPayPageAndCallback(t *testing.T) {
	h := newTestApp()
	tok := issue(t, h, "res-3", "https://shop.test/back")

	page := get(h, pathSendToken+"?token="+url.QueryEscape(tok))
	for _, want := range []string{"1,010,000", `action="` + pathSendToken + `"`, `value="pay"`, `value="cancel"`, `name="token" value="` + tok + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("pay page lacks %s", want)
		}
	}

	click := func(outcome string) string {
		req := httptest.NewRequest(http.MethodPost, pathSendToken, strings.NewReader(url.Values{"token": {tok}, "outcome": {outcome}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Body.String()
	}
	back := click("pay")
	for _, want := range []string{`action="https://shop.test/back"`, `name="State" value="OK"`, `name="Status" value="2"`,
		`name="RefNum" value="refnum-1-res-3"`, `name="ResNum" value="res-3"`} {
		if !strings.Contains(back, want) {
			t.Fatalf("return page lacks %s:\n%s", want, back)
		}
	}
	if w := post(h, pathVerify, `{"RefNum":"refnum-1-res-3","TerminalNumber":7002}`); !strings.Contains(w.Body.String(), `"Success":true`) {
		t.Fatalf("verify after the pay click: %s", w.Body)
	}

	issue(t, h, "res-4", "https://shop.test/back")
	tok = "token-2"
	back = click("cancel")
	for _, want := range []string{`name="State" value="CanceledByUser"`, `name="Status" value="1"`, `name="RefNum" value=""`, `name="ResNum" value="res-4"`} {
		if !strings.Contains(back, want) {
			t.Fatalf("cancel return lacks %s:\n%s", want, back)
		}
	}
	if w := post(h, pathVerify, `{"RefNum":"refnum-2-res-4","TerminalNumber":7002}`); !strings.Contains(w.Body.String(), `"ResultCode":-2`) {
		t.Fatalf("verify after a cancel: %s", w.Body)
	}
}

func TestControlCallbackAndForce(t *testing.T) {
	var got url.Values
	shop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		http.Redirect(w, r, "https://front.test/done", http.StatusFound)
	}))
	defer shop.Close()
	h := newTestApp()
	issue(t, h, "res-5", shop.URL)

	out := post(h, "/api/v1/test/orders/res-5/settle?callback=1", "").Body.String()
	if !strings.Contains(out, `"location":"https://front.test/done"`) || got.Get("State") != "OK" || got.Get("ResNum") != "res-5" || got.Get("RefNum") == "" {
		t.Fatalf("callback: %s, the shop got %v", out, got)
	}

	post(h, "/api/v1/test/config", `{"force":{"token":"8","verify":"-104"}}`)
	if w := post(h, pathToken, tokenBody("res-6", "https://x")); !strings.Contains(w.Body.String(), `"errorCode":"8"`) {
		t.Fatalf("forced token: %s", w.Body)
	}
	if w := post(h, pathVerify, `{"RefNum":"refnum-1-res-5","TerminalNumber":7002}`); !strings.Contains(w.Body.String(), `"ResultCode":-104`) {
		t.Fatalf("forced verify: %s", w.Body)
	}
	post(h, "/api/v1/test/config", `{"force":{"verify":"503"}}`)
	if w := post(h, pathVerify, `{"RefNum":"x","TerminalNumber":7002}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("forced HTTP status = %d", w.Code)
	}
}

func get(h http.Handler, path string) string {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Body.String()
}

func jsonDecode(w *httptest.ResponseRecorder, into any) error {
	return json.Unmarshal(w.Body.Bytes(), into)
}

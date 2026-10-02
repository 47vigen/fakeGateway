package behpardakht

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/47vigen/fakeGateway/core"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func newTestApp(publicBase string) http.Handler {
	h, _ := core.Handler(New(), publicBase, core.Behaviour{DefaultOutcome: core.OutcomeManual})
	return h
}

// The shopper's round trip as the platform driver drives it: pay, the page,
// the browser posted back, then verify and settle.
func TestABrowserPaymentSettlesAndVerifies(t *testing.T) {
	bank := httptest.NewServer(newTestApp("http://localhost:8090"))
	defer bank.Close()

	soap := func(op, fields string) string {
		t.Helper()
		body := `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="http://interfaces.core.sw.bps.com/"><soapenv:Body><ns1:` +
			op + `>` + fields + `</ns1:` + op + `></soapenv:Body></soapenv:Envelope>`
		res, err := http.Post(bank.URL+"/pgwchannel/services/pgw", "text/xml", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		m := regexp.MustCompile(`<return>([^<]*)</return>`).FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("%s: no return in %s", op, raw)
		}
		return m[1]
	}
	creds := `<terminalId>1</terminalId><userName>fake-terminal</userName><userPassword>fake-password</userPassword>`

	answer := soap("bpPayRequest", creds+`<orderId>7</orderId><amount>50000</amount><callBackUrl>https://shop.test/back</callBackUrl><payerId>0</payerId>`)
	code, refID, _ := strings.Cut(answer, ",")
	if code != "0" {
		t.Fatalf("pay answered %q", answer)
	}

	page, err := http.Get(bank.URL + "/pgwchannel/startpay.mellat?RefId=" + url.QueryEscape(refID))
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("start page: %v %v", err, page.Status)
	}
	page.Body.Close()

	res, err := http.PostForm(bank.URL+"/pgwchannel/pay", url.Values{"RefId": {refID}, "outcome": {"settle"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	back := string(raw)
	for _, want := range []string{`action="https://shop.test/back"`, `name="ResCode" value="0"`, `name="RefId" value="` + refID + `"`} {
		if !strings.Contains(back, want) {
			t.Fatalf("return page lacks %s:\n%s", want, back)
		}
	}
	saleRef := regexp.MustCompile(`name="SaleReferenceId" value="(\d+)"`).FindStringSubmatch(back)[1]

	follow := creds + `<orderId>8</orderId><saleOrderId>7</saleOrderId><saleReferenceId>` + saleRef + `</saleReferenceId>`
	if got := soap("bpVerifyRequest", follow); got != "0" {
		t.Fatalf("verify answered %q", got)
	}
	if got := soap("bpVerifyRequest", follow); got != "43" {
		t.Fatalf("a second verify answered %q, want 43", got)
	}
	if got := soap("bpSettleRequest", follow); got != "0" {
		t.Fatalf("settle answered %q", got)
	}
}

// A WSDL fetched through the public host names the public scheme: an edge
// that terminates TLS must not leave callers posting to http.
func TestTheWSDLFetchedOnThePublicHostKeepsItsScheme(t *testing.T) {
	h := newTestApp("https://bank.example")
	req := httptest.NewRequest(http.MethodGet, "http://bank.example/pgwchannel/services/pgw?wsdl", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if want := `location="https://bank.example/pgwchannel/services/pgw"`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("the WSDL lacks %s", want)
	}
	inside := httptest.NewRequest(http.MethodGet, "http://fakebehpardakht:8090/pgwchannel/services/pgw?wsdl", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, inside)
	if want := `location="http://fakebehpardakht:8090/pgwchannel/services/pgw"`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("the in-network WSDL lacks %s", want)
	}
}

// The control API posts the callback the way the bank's form would, and the
// order shows its ledger under the names existing test suites read.
func TestControlSettleFiresAPostedCallback(t *testing.T) {
	var got url.Values
	shop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		http.Redirect(w, r, "https://front.test/done", http.StatusFound)
	}))
	defer shop.Close()

	h := newTestApp("http://localhost:8090")
	rec := httptest.NewRecorder()
	body := `<Envelope><Body><bpPayRequest><userName>fake-terminal</userName><userPassword>fake-password</userPassword><orderId>9</orderId><amount>1000</amount><callBackUrl>` + shop.URL + `</callBackUrl></bpPayRequest></Body></Envelope>`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pgwchannel/services/pgw", strings.NewReader(body)))
	refID := regexp.MustCompile(`<return>0,(\d+)</return>`).FindStringSubmatch(rec.Body.String())[1]

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/test/orders/"+refID+"/settle?callback=1", nil))
	out := rec.Body.String()
	for _, want := range []string{`"status":302`, `"location":"https://front.test/done"`, `"pay_calls":1`, `"callback_calls":1`, `"state":"settled"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("settle answer lacks %s: %s", want, out)
		}
	}
	if got.Get("RefId") != refID || got.Get("ResCode") != "0" || got.Get("SaleOrderId") != "9" {
		t.Fatalf("the shop got %v", got)
	}
}

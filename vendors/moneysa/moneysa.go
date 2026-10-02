// Package moneysa is a test double for Moneysa's credit gateway (a CPG):
// REST, a username and password in the get-token and refund bodies, verify,
// verify-information and refund. It behaves like production where production
// is known to differ from Moneysa's swagger (checked 2026-08-13): a paid
// transaction verifies as status 10, an unpaid one is HTTP 400 code 2002, and
// a second verify is HTTP 400 code 2001.
package moneysa

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// Moneysa's error body, as production sends it: {"message", "code"}. The
// swagger's {error_code, error_message, details} is not what arrives.
var messages = map[int]string{
	1001: "Transaction not found",
	1003: "Invalid token",
	1011: "Invalid webservice username or password",
	1012: "Your webservice is disabled",
	2001: "تراکنش قبلا تسویه شده.", // verbatim from production
	2002: "Invalid transaction status",
	2003: "Sent transaction amount does not match performed transaction",
	2006: "Sent amount exceeds performed transaction amount",
}

func httpStatus(code int) int {
	switch code {
	case 1011, 1012:
		return http.StatusUnauthorized
	case 1001:
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func fail(w http.ResponseWriter, code int) {
	msg, ok := messages[code]
	if !ok {
		msg = "Error " + strconv.Itoa(code)
	}
	core.WriteJSON(w, httpStatus(code), map[string]any{"message": msg, "code": code})
}

func invalid(w http.ResponseWriter, field, msg string) {
	core.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message": "Input validation error", "errors": map[string][]string{field: {msg}},
	})
}

// The transaction statuses this fake answers. Only 10 is confirmed against
// production; 3, 2 and 0 come from the swagger.
const (
	statusVerified = 10
	statusRefunded = 3
	statusPending  = 2
	statusFailed   = 0
)

var labels = map[int][2]string{
	statusVerified: {"verified", "پرداخت شده"},
	statusRefunded: {"refunded", "ریفاند شده"},
	statusPending:  {"pending", "در انتظار"},
	statusFailed:   {"failed", "ناموفق"},
}

// transaction is what Moneysa holds beyond core.Order. The order's ID is the
// numeric transaction_id; Verified marks the one verify that may succeed.
type transaction struct {
	Token        string
	TrackingCode string
	NationalID   string
	Verified     bool
	Refunded     bool
}

type Vendor struct {
	username string
	password string
	c        *core.Core
	sequence int64 // guarded by the core lock
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// New reads USERNAME (fake-username) and PASSWORD (fake-password).
func New() *Vendor {
	return &Vendor{username: env("USERNAME", "fake-username"), password: env("PASSWORD", "fake-password")}
}

func (v *Vendor) Name() string { return "moneysa" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST /api/payments/get-token", v.getToken)
	mux.HandleFunc("POST /api/payments/verify", func(w http.ResponseWriter, r *http.Request) { v.verify(w, r, "verify") })
	mux.HandleFunc("POST /api/payments/verify-information", func(w http.ResponseWriter, r *http.Request) { v.verify(w, r, "inquiry") })
	mux.HandleFunc("POST /api/payments/refund", v.refund)
	mux.HandleFunc("GET /pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("id"), "", nil)
	})
	mux.HandleFunc("POST /pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("id"))
	})
}

// Callback is a GET to the merchant's callback carrying the token. Moneysa's
// return parameters are undocumented and the merchant's driver reads none.
func (v *Vendor) Callback(o core.Order) core.Callback {
	return core.Callback{URL: o.CallbackURL, Fields: map[string]string{"token": o.Extra.(transaction).Token}}
}

type view struct {
	TransactionID string          `json:"transaction_id"`
	Token         string          `json:"token"`
	TrackingCode  string          `json:"tracking_code"`
	NationalID    string          `json:"national_id"`
	Amount        int64           `json:"amount"`
	State         core.State      `json:"state"`
	Verified      bool            `json:"verified"`
	Refunded      bool            `json:"refunded"`
	CallbackURL   string          `json:"callback"`
	URL           string          `json:"url"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Calls         map[string]int  `json:"calls"`
	Request       json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	t := o.Extra.(transaction)
	return view{
		TransactionID: o.ID,
		Token:         t.Token,
		TrackingCode:  t.TrackingCode,
		NationalID:    t.NationalID,
		Amount:        o.Amount,
		State:         o.State,
		Verified:      t.Verified,
		Refunded:      t.Refunded,
		CallbackURL:   o.CallbackURL,
		URL:           v.c.PublicBaseURL + "/pay/" + url.PathEscape(o.ID),
		CreatedAt:     o.CreatedAt,
		UpdatedAt:     o.UpdatedAt,
		Calls:         o.Calls,
		Request:       o.Request,
	}
}

// response is Moneysa's TransactionResponse.
func response(o *core.Order, amount int64, status int) map[string]any {
	t := o.Extra.(transaction)
	id, _ := strconv.ParseInt(o.ID, 10, 64)
	return map[string]any{
		"amount":          amount,
		"national_id":     t.NationalID,
		"status":          status,
		"status_label":    labels[status][0],
		"status_label_fa": labels[status][1],
		"token":           t.Token,
		"tracking_code":   t.TrackingCode,
		"transaction_id":  id,
	}
}

// begin is the common head of every operation: the delay, a forced answer (a
// Moneysa code, or a three-digit HTTP status), and the body. It reports false
// once it has answered.
func (v *Vendor) begin(w http.ResponseWriter, r *http.Request, op string, req any) ([]byte, bool) {
	b := v.c.Behaviour()
	b.Wait()
	if n, err := strconv.Atoi(b.Force[op]); err == nil {
		if n < 1000 {
			core.WriteJSON(w, n, map[string]any{"message": "Server Error"})
		} else {
			fail(w, n)
		}
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err == nil {
		err = json.Unmarshal(body, req)
	}
	if err != nil {
		invalid(w, "body", "invalid JSON: "+err.Error())
		return nil, false
	}
	return body, true
}

func (v *Vendor) authorized(w http.ResponseWriter, username, password string) bool {
	if username == v.username && password == v.password {
		return true
	}
	fail(w, 1011)
	return false
}

var nationalID = regexp.MustCompile(`^[0-9]{10}$`)

func (v *Vendor) getToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		TrackingCode string `json:"tracking_code"`
		Amount       int64  `json:"amount"`
		Callback     string `json:"callback"`
		NationalID   string `json:"national_id"`
	}
	body, ok := v.begin(w, r, "create", &req)
	if !ok || !v.authorized(w, req.Username, req.Password) {
		return
	}
	switch {
	case req.TrackingCode == "":
		invalid(w, "tracking_code", "The tracking code field is required.")
		return
	case req.Amount < 1000 || req.Amount > 500_000_000:
		invalid(w, "amount", "Amount must be between 1000 and 500000000 Rials")
		return
	case !nationalID.MatchString(req.NationalID):
		invalid(w, "national_id", "National ID must be 10 digits")
		return
	}

	initial := v.c.Behaviour().Initial(core.Pending)
	duplicate := false
	var o core.Order
	v.c.Do(func(orders map[string]*core.Order) {
		for _, other := range orders {
			if other.Extra.(transaction).TrackingCode == req.TrackingCode {
				duplicate = true
				return
			}
		}
		v.sequence++
		now := time.Now()
		o = core.Order{
			ID:          strconv.FormatInt(1_855_600+v.sequence, 10),
			Amount:      req.Amount,
			State:       initial,
			CallbackURL: req.Callback,
			Items:       []core.Item{{Name: "Order " + req.TrackingCode, Quantity: 1, Price: req.Amount, Total: req.Amount}},
			Calls:       map[string]int{"create": 1},
			Request:     body,
			CreatedAt:   now,
			UpdatedAt:   now,
			Extra:       transaction{Token: rand.Text(), TrackingCode: req.TrackingCode, NationalID: req.NationalID},
		}
		stored := o
		orders[o.ID] = &stored
	})
	if duplicate {
		invalid(w, "tracking_code", "The tracking code has already been taken.")
		return
	}
	slog.Info("moneysa token issued", "transaction_id", o.ID, "tracking_code", req.TrackingCode, "amount", o.Amount, "state", o.State)
	payURL := v.c.PublicBaseURL + "/pay/" + url.PathEscape(o.ID)
	core.WriteJSON(w, http.StatusOK, map[string]any{
		"tracking_code":  req.TrackingCode,
		"transaction_id": o.ID,
		"url_meta":       payURL,
		"token":          o.Extra.(transaction).Token,
		"url":            payURL,
	})
}

// flexID takes transaction_id as the number verify sends or the string refund
// sends.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	*f = flexID(strings.Trim(string(b), `"`))
	return nil
}

// verify serves /verify (op "verify") and /verify-information (op "inquiry").
// Verify succeeds once, on a paid transaction; inquiry only reports.
func (v *Vendor) verify(w http.ResponseWriter, r *http.Request, op string) {
	var req struct {
		Token         string `json:"token"`
		TransactionID flexID `json:"transaction_id"`
		Amount        int64  `json:"amount"`
	}
	if _, ok := v.begin(w, r, op, &req); !ok {
		return
	}
	code, status := 0, 0
	var out map[string]any
	_, found := v.c.Update(string(req.TransactionID), func(o *core.Order) {
		o.Count(op)
		t := o.Extra.(transaction)
		switch {
		case t.Token != req.Token:
			code = 1003
		case o.Amount != req.Amount:
			code = 2003
		case op == "inquiry":
			switch {
			case t.Refunded:
				status = statusRefunded
			case o.State == core.Settled:
				status = statusVerified
			case o.State == core.Pending:
				status = statusPending
			default:
				status = statusFailed
			}
		case t.Refunded || o.State != core.Settled:
			code = 2002
		case t.Verified:
			code = 2001
		default:
			t.Verified = true
			o.Extra, o.UpdatedAt = t, time.Now()
			status = statusVerified
		}
		if code == 0 {
			out = response(o, o.Amount, status)
		}
	})
	switch {
	case !found:
		fail(w, 1001)
	case code != 0:
		fail(w, code)
	default:
		core.WriteJSON(w, http.StatusOK, out)
	}
}

// refund returns a paid transaction's money once; partial refunds are not
// tracked, so any refund closes it.
// ponytail: one refund per transaction; keep a running total if a test needs partials.
func (v *Vendor) refund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		Token         string `json:"token"`
		TransactionID flexID `json:"transaction_id"`
		Amount        int64  `json:"amount"`
	}
	if _, ok := v.begin(w, r, "refund", &req); !ok || !v.authorized(w, req.Username, req.Password) {
		return
	}
	code := 0
	var out map[string]any
	_, found := v.c.Update(string(req.TransactionID), func(o *core.Order) {
		o.Count("refund")
		t := o.Extra.(transaction)
		switch {
		case t.Token != req.Token:
			code = 1003
		case t.Refunded || o.State != core.Settled:
			code = 2002
		case req.Amount > o.Amount:
			code = 2006
		default:
			t.Refunded = true
			o.Extra, o.State, o.UpdatedAt = t, core.Reversed, time.Now()
			out = response(o, req.Amount, statusRefunded)
		}
	})
	switch {
	case !found:
		fail(w, 1001)
	case code != 0:
		fail(w, code)
	default:
		core.WriteJSON(w, http.StatusOK, out)
	}
}

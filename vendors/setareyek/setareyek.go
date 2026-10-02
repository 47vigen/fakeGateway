// Package setareyek imitates SetareYek's buy-now-pay-later gateway (a BPG) as
// a production setareyek driver drives it: a bare-string token from GetToken, a
// Purchase that mints a TrackId, the shopper's GET back, then Verify, Inquiry
// and RefundPayment. Every business answer is HTTP 200 with a Result code.
package setareyek

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// Result codes (setareyek.constants.ts). Only 100 is a success.
const (
	resultSuccess          = 100
	resultMerchantNotFound = 102
	resultAmountTooLow     = 106
	resultCallbackInvalid  = 107
	resultOrdersRequired   = 108
	resultAmountTooHigh    = 113
	resultTrackIDNotFound  = 115
	resultNotPaid          = 116
)

var messages = map[int]string{
	resultSuccess:          "success",
	resultMerchantNotFound: "merchant not found",
	resultAmountTooLow:     "amount too low",
	resultCallbackInvalid:  "callback url invalid",
	resultOrdersRequired:   "orders required",
	resultAmountTooHigh:    "amount too high",
	resultTrackIDNotFound:  "track id not found",
	resultNotPaid:          "not paid",
}

// Payment status codes Inquiry and Verify report.
const (
	statusInitialCreate  = 1
	statusPaymentSuccess = 102
	statusPaymentFailed  = 103
	statusVerified       = 104
	statusReversed       = 105
	statusRefunded       = 106
)

// Refund statuses. FAILED rides on a Result of 100 for a well-formed request
// whose refund did not happen.
const (
	refundSuccess = 0
	refundFailed  = 1
)

// refunded is the vendor state an order takes once its money went back.
const refunded core.State = "refunded"

// loan is what SetareYek holds about an order beyond core.Order.
type loan struct {
	Verified bool
	PaidAt   time.Time
	Refunded bool
}

type Vendor struct {
	username, password, merchant string
	c                            *core.Core

	mu     sync.Mutex
	tokens map[string]bool
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// New reads USERNAME, PASSWORD and MERCHANT.
func New() *Vendor {
	return newVendor(env("USERNAME", "fake-user"), env("PASSWORD", "fake-password"), env("MERCHANT", "fake-merchant"))
}

func newVendor(username, password, merchant string) *Vendor {
	return &Vendor{username: username, password: password, merchant: merchant, tokens: map[string]bool{}}
}

func (v *Vendor) Name() string { return "setareyek" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("GET /GetToken", v.token)
	mux.HandleFunc("POST /Purchase", v.authed(v.purchase))
	mux.HandleFunc("POST /Verify", v.authed(v.verify))
	mux.HandleFunc("POST /Inquiry", v.authed(v.inquiry))
	mux.HandleFunc("POST /RefundPayment", v.authed(v.refund))
	mux.HandleFunc("GET /pay/{track_id}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("track_id"), "", nil)
	})
	mux.HandleFunc("POST /pay/{track_id}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("track_id"))
	})
}

// Callback is a GET back to CallbackUrl. The driver reads nothing from it:
// The merchant finds the leg by the hash in the callback path and asks Verify.
func (v *Vendor) Callback(o core.Order) core.Callback {
	return core.Callback{URL: o.CallbackURL, Fields: map[string]string{
		"TrackId": o.ID,
		"Status":  strconv.Itoa(paymentStatus(o.State, o.Extra.(loan))),
	}}
}

// paymentStatus is the order's state in SetareYek's payment status enum.
func paymentStatus(st core.State, l loan) int {
	switch {
	case l.Refunded:
		return statusRefunded
	case st == core.Reversed:
		return statusReversed
	case l.Verified:
		return statusVerified
	case st == core.Settled:
		return statusPaymentSuccess
	case st == core.Failed:
		return statusPaymentFailed
	}
	return statusInitialCreate
}

type view struct {
	TrackID       string          `json:"track_id"`
	Amount        int64           `json:"amount"`
	CallbackURL   string          `json:"callback_url"`
	PaymentURL    string          `json:"payment_url"`
	State         core.State      `json:"state"`
	PaymentStatus int             `json:"payment_status"`
	Verified      bool            `json:"verified"`
	Refunded      bool            `json:"refunded"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Calls         map[string]int  `json:"calls"`
	Request       json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	l := o.Extra.(loan)
	return view{
		TrackID:       o.ID,
		Amount:        o.Amount,
		CallbackURL:   o.CallbackURL,
		PaymentURL:    v.paymentURL(o.ID),
		State:         o.State,
		PaymentStatus: paymentStatus(o.State, l),
		Verified:      l.Verified,
		Refunded:      l.Refunded,
		CreatedAt:     o.CreatedAt,
		UpdatedAt:     o.UpdatedAt,
		Calls:         o.Calls,
		Request:       o.Request,
	}
}

func (v *Vendor) paymentURL(trackID string) string {
	return v.c.PublicBaseURL + "/pay/" + url.PathEscape(trackID)
}

func result(code int) map[string]any {
	return map[string]any{"Result": code, "Message": messages[code]}
}

// forced answers force[op]: 100–199 is a Result code in a 200 body, anything
// from 200 to 599 that HTTP status with no body.
func forced(w http.ResponseWriter, b core.Behaviour, op string) bool {
	n, err := strconv.Atoi(b.Force[op])
	switch {
	case err != nil || n < 100 || n >= 600:
		return false
	case n < 200:
		body := result(n)
		if body["Message"] == nil {
			body["Message"] = "forced result from the fake gateway control API"
		}
		if op == "refund" && n != resultSuccess {
			body["Status"] = refundFailed
		}
		core.WriteJSON(w, http.StatusOK, body)
	default:
		w.WriteHeader(n)
	}
	return true
}

// token answers the bearer token as a JSON string, not an object.
func (v *Vendor) token(w http.ResponseWriter, r *http.Request) {
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "token") {
		return
	}
	q := r.URL.Query()
	if q.Get("username") != v.username || q.Get("password") != v.password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	tok := rand.Text()
	v.mu.Lock()
	v.tokens[tok] = true
	v.mu.Unlock()
	core.WriteJSON(w, http.StatusOK, tok)
}

// authed requires a Bearer token this fake issued (an ASP.NET-style bare 401
// otherwise), waits DelayMS and decodes the JSON body.
func (v *Vendor) authed(next func(http.ResponseWriter, *request, core.Behaviour)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		v.mu.Lock()
		ok = ok && v.tokens[tok]
		v.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b := v.c.Behaviour()
		b.Wait()
		body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
		var req request
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			core.WriteJSON(w, http.StatusBadRequest, map[string]any{"title": "invalid JSON", "status": 400, "detail": err.Error()})
			return
		}
		req.raw = body
		next(w, &req, b)
	}
}

type orderItem struct {
	OrderID string `json:"OrderId"`
	Name    string `json:"Name"`
	Amount  int64  `json:"Amount"`
}

// request is the union of every body the driver posts.
type request struct {
	Merchant    string      `json:"Merchant"`
	TrackID     string      `json:"TrackId"`
	Amount      int64       `json:"Amount"`
	CallbackURL string      `json:"CallbackUrl"`
	Description string      `json:"Description"`
	Mobile      string      `json:"Mobile"`
	Orders      []orderItem `json:"Orders"`
	raw         []byte
}

func (v *Vendor) purchase(w http.ResponseWriter, req *request, b core.Behaviour) {
	if forced(w, b, "create") {
		return
	}
	code := resultSuccess
	if u, err := url.Parse(req.CallbackURL); err != nil || !u.IsAbs() {
		code = resultCallbackInvalid
	}
	switch {
	case req.Merchant != v.merchant:
		code = resultMerchantNotFound
	case req.Amount <= 0:
		code = resultAmountTooLow
	case len(req.Orders) == 0:
		code = resultOrdersRequired
	}
	if code != resultSuccess {
		core.WriteJSON(w, http.StatusOK, result(code))
		return
	}

	items := make([]core.Item, len(req.Orders))
	for i, it := range req.Orders {
		items[i] = core.Item{Name: it.Name, Quantity: 1, Price: it.Amount, Total: it.Amount}
	}
	now := time.Now()
	o := core.Order{
		ID:          strconv.FormatInt(now.UnixNano(), 10),
		Amount:      req.Amount,
		State:       b.Initial(core.Pending),
		CallbackURL: req.CallbackURL,
		Items:       items,
		Calls:       map[string]int{"create": 1},
		Request:     req.raw,
		CreatedAt:   now,
		UpdatedAt:   now,
		Extra:       loan{},
	}
	// ponytail: a nanosecond TrackId; a retry on the rare collision is enough.
	for !v.c.Create(o) {
		o.ID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	slog.Info("purchase created", "track_id", o.ID, "amount", o.Amount, "state", o.State)
	body := result(resultSuccess)
	body["TrackId"], body["Url"] = o.ID, v.paymentURL(o.ID)
	core.WriteJSON(w, http.StatusOK, body)
}

// operate runs fn on the order behind req.TrackId under the store lock,
// counting op, and writes the body fn answers. A wrong Merchant and an
// unknown TrackId are answered before fn.
func (v *Vendor) operate(w http.ResponseWriter, op string, req *request, fn func(o *core.Order, l *loan) map[string]any) {
	body := result(resultTrackIDNotFound)
	if req.Merchant != v.merchant {
		body = result(resultMerchantNotFound)
	} else {
		v.c.Do(func(orders map[string]*core.Order) {
			o := orders[req.TrackID]
			if o == nil {
				return
			}
			o.Count(op)
			l := o.Extra.(loan)
			body = fn(o, &l)
			o.Extra = l
		})
	}
	if op == "refund" && body["Result"] != resultSuccess {
		body["Status"] = refundFailed
	}
	slog.Info("setareyek "+op, "track_id", req.TrackID, "result", body["Result"])
	core.WriteJSON(w, http.StatusOK, body)
}

// verify is idempotent: a verified loan answers 100 again. An unpaid, failed,
// reversed or refunded one answers 116.
func (v *Vendor) verify(w http.ResponseWriter, req *request, b core.Behaviour) {
	if forced(w, b, "verify") {
		return
	}
	v.operate(w, "verify", req, func(o *core.Order, l *loan) map[string]any {
		body := result(resultNotPaid)
		if o.State == core.Settled && !l.Refunded {
			if !l.Verified {
				l.Verified, l.PaidAt = true, o.UpdatedAt
			}
			body = result(resultSuccess)
			body["PaidAt"] = l.PaidAt.UTC().Format(time.RFC3339)
		} else {
			body["PaidAt"] = nil
		}
		body["Status"] = strconv.Itoa(paymentStatus(o.State, *l))
		body["Amount"] = o.Amount
		return body
	})
}

func (v *Vendor) inquiry(w http.ResponseWriter, req *request, b core.Behaviour) {
	if forced(w, b, "inquire") {
		return
	}
	v.operate(w, "inquire", req, func(o *core.Order, l *loan) map[string]any {
		body := result(resultSuccess)
		body["Status"] = paymentStatus(o.State, *l)
		body["Amount"] = o.Amount
		body["CreatedAt"] = o.CreatedAt.UTC().Format(time.RFC3339)
		return body
	})
}

func (v *Vendor) refund(w http.ResponseWriter, req *request, b core.Behaviour) {
	if forced(w, b, "refund") {
		return
	}
	v.operate(w, "refund", req, func(o *core.Order, l *loan) map[string]any {
		switch {
		case req.Amount <= 0:
			return result(resultAmountTooLow)
		case req.Amount > o.Amount:
			return result(resultAmountTooHigh)
		case len(req.Orders) == 0:
			return result(resultOrdersRequired)
		case !l.Verified:
			return result(resultNotPaid)
		case l.Refunded:
			// A well-formed request for a refund that cannot happen.
			body := result(resultSuccess)
			body["Message"], body["Status"] = "already refunded", refundFailed
			return body
		}
		l.Refunded = true
		o.State, o.UpdatedAt = refunded, time.Now()
		body := result(resultSuccess)
		body["Status"] = refundSuccess
		return body
	})
}

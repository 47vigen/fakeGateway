// Package zarinplus imitates Zarinplus (a CPG with installments) as a
// production driver drives it: JSON over POST with the merchant token in every
// body, a request that answers an authority and a page, the shopper's GET back
// to the success address (with ?authority=) or the cancel address (with
// nothing), then a verify.
package zarinplus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// Each path keeps its trailing slash; the same path without it answers 301.
const (
	pathRequest = "/payment/v2/request/"
	pathVerify  = "/payment/v2/verify/"
)

const (
	gatewaySlug = "zarinplus"
	// The sentence of a refused merchant token or an unpaid authority.
	messageInvalidToken = "Invalid token"
	messageNoTx         = "User or Tx not exist"
)

// The verify codes that mean the money moved: verified now, and verified before.
const (
	codeVerified        = 200
	codeAlreadyVerified = 201
)

// request is what Zarinplus holds about an order beyond core.Order. The
// order's CallbackURL is the success address.
type request struct {
	Cancel    string
	Reference string
	Verified  int
}

type Vendor struct {
	token    string
	c        *core.Core
	sequence int64 // guarded by the core lock
}

// New reads TOKEN (default "fake-token"), the merchant token every body carries.
func New() *Vendor {
	t := os.Getenv("TOKEN")
	if t == "" {
		t = "fake-token"
	}
	return &Vendor{token: t}
}

func (v *Vendor) Name() string { return "zarinplus" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST "+pathRequest, v.request)
	mux.HandleFunc("POST "+pathVerify, v.verify)
	for _, path := range []string{pathRequest, pathVerify} {
		bare := path[:len(path)-1]
		mux.HandleFunc("POST "+bare, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, path, http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc("GET /pay/{authority}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("authority"), "", nil)
	})
	mux.HandleFunc("POST /pay/{authority}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("authority"))
	})
}

// Callback is the shopper's GET back: the success address with ?authority= once
// paid, the cancel address with nothing otherwise.
func (v *Vendor) Callback(o core.Order) core.Callback {
	if o.State == core.Settled {
		return core.Callback{URL: o.CallbackURL, Fields: map[string]string{"authority": o.ID}}
	}
	return core.Callback{URL: o.Extra.(request).Cancel}
}

type view struct {
	Authority  string     `json:"authority"`
	Reference  string     `json:"reference"`
	Amount     int64      `json:"amount"`
	State      core.State `json:"state"`
	SuccessURL string     `json:"success_url"`
	CancelURL  string     `json:"cancel_url"`
	PaymentURL string     `json:"payment_url"`
	Verified   int        `json:"verified"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	// Request is the request body as it arrived, token included.
	Request json.RawMessage `json:"request"`
	Calls   map[string]int  `json:"calls"`
}

func (v *Vendor) View(o core.Order) any {
	r := o.Extra.(request)
	return view{
		Authority: o.ID, Reference: r.Reference, Amount: o.Amount, State: o.State,
		SuccessURL: o.CallbackURL, CancelURL: r.Cancel, PaymentURL: v.paymentURL(o.ID),
		Verified: r.Verified, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, Request: o.Request, Calls: o.Calls,
	}
}

func (v *Vendor) paymentURL(authority string) string { return v.c.PublicBaseURL + "/pay/" + authority }

// failure is Zarinplus's business error: HTTP 400 with status false.
func failure(w http.ResponseWriter, status int, message string) {
	core.WriteJSON(w, status, map[string]any{"status": false, "message": message})
}

// forced answers a forced value: 100 to 599 is that HTTP status, anything else
// a 400 with the value as its message.
func forced(w http.ResponseWriter, b core.Behaviour, op string) bool {
	f := b.Force[op]
	if f == "" {
		return false
	}
	if n, err := strconv.Atoi(f); err == nil && n >= 100 && n <= 599 {
		failure(w, n, "forced fault from the fake gateway control API")
	} else {
		failure(w, http.StatusBadRequest, f)
	}
	return true
}

func (v *Vendor) request(w http.ResponseWriter, r *http.Request) {
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "request") {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err != nil {
		failure(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	var req struct {
		Token       string `json:"token"`
		Amount      int64  `json:"amount"`
		Success     string `json:"success"`
		Cancel      string `json:"cancel"`
		Item        string `json:"item"`
		GatewaySlug string `json:"gateway_slug"`
	}
	switch err := json.Unmarshal(body, &req); {
	case err != nil:
		failure(w, http.StatusBadRequest, "invalid JSON")
		return
	case req.Token != v.token:
		failure(w, http.StatusBadRequest, messageInvalidToken)
		return
	case req.Amount <= 0, req.Success == "", req.Cancel == "", req.Item == "", req.GatewaySlug != gatewaySlug:
		failure(w, http.StatusBadRequest, "invalid parameters")
		return
	}

	var o core.Order
	v.c.Do(func(orders map[string]*core.Order) {
		v.sequence++
		now := time.Now()
		o = core.Order{
			ID: fmt.Sprintf("A%035d", v.sequence), Amount: req.Amount, State: b.Initial(core.Pending),
			CallbackURL: req.Success, Items: []core.Item{{Name: req.Item, Quantity: 1, Price: req.Amount, Total: req.Amount}},
			Calls: map[string]int{"request": 1}, Request: json.RawMessage(body), CreatedAt: now, UpdatedAt: now,
			Extra: request{Cancel: req.Cancel, Reference: fmt.Sprintf("ref-%d", v.sequence)},
		}
		orders[o.ID] = &o
	})
	core.WriteJSON(w, http.StatusOK, map[string]any{"status": true, "authority": o.ID, "redirect_url": v.paymentURL(o.ID)})
}

// verify answers code 200 the first time a paid authority is verified and 201
// after that. An unpaid or unknown authority, or a different amount, is the
// provider's "User or Tx not exist".
func (v *Vendor) verify(w http.ResponseWriter, r *http.Request) {
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "verify") {
		return
	}
	var req struct {
		Token     string `json:"token"`
		Authority string `json:"authority"`
		Amount    int64  `json:"amount"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, core.MaxBody)).Decode(&req); err != nil {
		failure(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Token != v.token {
		failure(w, http.StatusBadRequest, messageInvalidToken)
		return
	}

	code, reference := 0, ""
	v.c.Update(req.Authority, func(o *core.Order) {
		o.Count("verify")
		rq := o.Extra.(request)
		if o.State != core.Settled || o.Amount != req.Amount {
			return
		}
		code, reference = codeVerified, rq.Reference
		if rq.Verified++; rq.Verified > 1 {
			code = codeAlreadyVerified
		}
		o.Extra = rq
	})
	if code == 0 {
		failure(w, http.StatusBadRequest, messageNoTx)
		return
	}
	core.WriteJSON(w, http.StatusOK, map[string]any{"status": true, "data": map[string]any{"code": code, "reference": reference}})
}

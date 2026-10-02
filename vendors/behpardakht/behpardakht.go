// Package behpardakht is a test double for Bank Mellat's internet payment
// gateway (an IPG): the SOAP service, the startpay.mellat page and the
// browser's POST back.
package behpardakht

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// Behpardakht answers every operation with a numeric result code as a string.
const (
	codeSuccess         = "0"
	codeInsufficient    = "12"
	codeDuplicateOrder  = "41"
	codeNotFound        = "42"
	codeAlreadyVerified = "43"
	codeAlreadySettled  = "45"
	codeReversed        = "48"
	codeSystemError     = "34"
)

// created is this bank's pending.
const created core.State = "created"

// bank is what the bank holds about an order beyond core.Order.
type bank struct {
	SaleOrderID     int64
	SaleReferenceID int64
	Verified        bool
	Settled         bool
	Reversed        bool
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

// New reads USERNAME and PASSWORD. They guard nothing; they exist so a
// wrong-credential answer (34) can be exercised. The terminal id is not
// checked.
func New() *Vendor {
	return &Vendor{
		username: env("USERNAME", "fake-terminal"),
		password: env("PASSWORD", "fake-password"),
	}
}

func (v *Vendor) Name() string { return "behpardakht" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	// Both URLs the merchant's driver keeps in config answer the same document.
	mux.HandleFunc("/pgwchannel/services/pgw", v.soap)
	mux.HandleFunc("/pgwchannel/services/pgwverify", v.soap)
	// startpay.mellat is the bank's real address; startpay is what the merchant's
	// local stack was configured with first.
	start := func(w http.ResponseWriter, r *http.Request) {
		id := r.FormValue("RefId")
		c.PayPage(w, id, "/pgwchannel/pay", map[string]string{"RefId": id})
	}
	mux.HandleFunc("/pgwchannel/startpay", start)
	mux.HandleFunc("/pgwchannel/startpay.mellat", start)
	mux.HandleFunc("POST /pgwchannel/pay", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.FormValue("RefId"))
	})
}

// Callback is what the bank posts to callBackUrl: exactly the fields the
// drivers read, plus the masked card the real one sends.
func (v *Vendor) Callback(o core.Order) core.Callback {
	resCode := codeSuccess
	if o.State != core.Settled {
		resCode = codeInsufficient
	}
	b := o.Extra.(bank)
	return core.Callback{URL: o.CallbackURL, Post: true, Fields: map[string]string{
		"RefId":           o.ID,
		"ResCode":         resCode,
		"SaleOrderId":     strconv.FormatInt(b.SaleOrderID, 10),
		"SaleReferenceId": strconv.FormatInt(b.SaleReferenceID, 10),
		"CardHolderPan":   "6037********1234",
	}}
}

type view struct {
	RefID           string     `json:"ref_id"`
	SaleOrderID     int64      `json:"sale_order_id"`
	SaleReferenceID int64      `json:"sale_reference_id"`
	Amount          int64      `json:"amount"`
	CallbackURL     string     `json:"callback_url"`
	State           core.State `json:"state"`
	Verified        bool       `json:"verified"`
	Settled         bool       `json:"settled"`
	Reversed        bool       `json:"reversed"`
	CreatedAt       string     `json:"created_at"`

	// The ledger a test reads instead of trusting the merchant's own answer.
	PayCalls      int `json:"pay_calls"`
	VerifyCalls   int `json:"verify_calls"`
	SettleCalls   int `json:"settle_calls"`
	ReversalCalls int `json:"reversal_calls"`
	CallbackCalls int `json:"callback_calls"`

	// The create request, so a test can assert what the bank was actually
	// asked for rather than what the merchant says it asked for.
	Request json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	b := o.Extra.(bank)
	return view{
		RefID:           o.ID,
		SaleOrderID:     b.SaleOrderID,
		SaleReferenceID: b.SaleReferenceID,
		Amount:          o.Amount,
		CallbackURL:     o.CallbackURL,
		State:           o.State,
		Verified:        b.Verified,
		Settled:         b.Settled,
		Reversed:        b.Reversed,
		CreatedAt:       o.CreatedAt.UTC().Format(time.RFC3339Nano),
		PayCalls:        o.Calls["pay"],
		VerifyCalls:     o.Calls["verify"],
		SettleCalls:     o.Calls["settle"],
		ReversalCalls:   o.Calls["reversal"],
		CallbackCalls:   o.Calls["callback"],
		Request:         o.Request,
	}
}

func (v *Vendor) soap(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery == "wsdl" || r.URL.Query().Has("wsdl") {
		// The endpoint a WSDL advertises must be where the WSDL was fetched
		// from, not a configured public address. node-soap POSTs to
		// <soap:address>, so advertising PUBLIC_BASE_URL to a caller inside
		// the docker network sends it to its own localhost and it gets
		// ECONNREFUSED. PUBLIC_BASE_URL is for the shopper's browser and the
		// payment page only.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		// Behind a TLS-terminating edge the caller still came in on https.
		// Traefik overwrites X-Forwarded-Proto with its own plain hop, so a
		// request for PUBLIC_BASE_URL's host takes that URL's scheme.
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		if public, err := neturl.Parse(v.c.PublicBaseURL); err == nil && public.Host == r.Host {
			scheme = public.Scheme
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		fmt.Fprint(w, wsdl(scheme+"://"+r.Host+r.URL.Path))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	b := v.c.Behaviour()
	b.Wait()

	env, err := readEnvelope(r)
	if err != nil {
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	switch {
	case env.Pay != nil:
		writeSOAP(w, "bpPayRequest", v.pay(b, *env.Pay))
	case env.Verify != nil:
		writeSOAP(w, "bpVerifyRequest", v.verify(b, *env.Verify))
	case env.Settle != nil:
		writeSOAP(w, "bpSettleRequest", v.settle(b, *env.Settle))
	case env.Reverse != nil:
		writeSOAP(w, "bpReversalRequest", v.reverse(*env.Reverse))
	default:
		http.Error(w, "no known operation in the envelope", http.StatusBadRequest)
	}
}

// find is the order the bank knows by our orderId.
// ponytail: a linear scan; a test double holds a handful of orders.
func find(orders map[string]*core.Order, saleOrderID int64) *core.Order {
	for _, o := range orders {
		if o.Extra.(bank).SaleOrderID == saleOrderID {
			return o
		}
	}
	return nil
}

// change applies fn to o's bank state.
func change(o *core.Order, fn func(*bank)) {
	b := o.Extra.(bank)
	fn(&b)
	o.Extra = b
}

// pay answers "ResCode,RefId" — the one operation whose result is two values
// in one string, which is the shape the driver splits on a comma.
func (v *Vendor) pay(b core.Behaviour, req payRequest) (answer string) {
	if code := b.Force["pay"]; code != "" {
		return code
	}
	if req.UserName != v.username || req.UserPassword != v.password {
		return codeSystemError
	}
	raw, _ := json.Marshal(req)
	v.c.Do(func(orders map[string]*core.Order) {
		if existing := find(orders, req.OrderID); existing != nil {
			existing.Count("pay")
			// The real acquirer refuses a repeated orderId rather than minting
			// a second reference for the same purchase.
			answer = codeDuplicateOrder
			return
		}
		v.sequence++
		now := time.Now()
		o := &core.Order{
			ID:          fmt.Sprintf("%d%06d", now.Unix(), v.sequence),
			Amount:      req.Amount,
			State:       b.Initial(created),
			CallbackURL: req.CallBackURL,
			Calls:       map[string]int{"pay": 1},
			Request:     raw,
			CreatedAt:   now,
			UpdatedAt:   now,
			Extra: bank{
				SaleOrderID:     req.OrderID,
				SaleReferenceID: rand.Int63n(9_000_000_000) + 1_000_000_000,
			},
		}
		orders[o.ID] = o
		answer = codeSuccess + "," + o.ID
	})
	return answer
}

// verify is what decides whether money was captured. The driver reverses on
// anything but 0 or 43, so those two are the only pair that lets a payment
// through.
func (v *Vendor) verify(b core.Behaviour, req verifyRequest) (answer string) {
	if code := b.Force["verify"]; code != "" {
		return code
	}
	v.c.Do(func(orders map[string]*core.Order) {
		o := find(orders, req.SaleOrderID)
		if o == nil {
			answer = codeNotFound
			return
		}
		o.Count("verify")
		bk := o.Extra.(bank)
		switch {
		case bk.SaleReferenceID != req.SaleReferenceID:
			answer = codeNotFound
		case bk.Reversed:
			answer = codeReversed
		case o.State == core.Failed:
			answer = codeInsufficient
		case bk.Verified:
			answer = codeAlreadyVerified
		case o.State != core.Settled:
			// Created but not settled at the bank: nothing to verify yet,
			// which is what an abandoned payment page looks like.
			answer = codeNotFound
		default:
			change(o, func(bk *bank) { bk.Verified = true })
			answer = codeSuccess
		}
	})
	return answer
}

func (v *Vendor) settle(b core.Behaviour, req verifyRequest) (answer string) {
	if code := b.Force["settle"]; code != "" {
		return code
	}
	v.c.Do(func(orders map[string]*core.Order) {
		o := find(orders, req.SaleOrderID)
		if o == nil {
			answer = codeNotFound
			return
		}
		o.Count("settle")
		bk := o.Extra.(bank)
		switch {
		case bk.Reversed:
			answer = codeReversed
		case bk.Settled:
			answer = codeAlreadySettled
		case !bk.Verified:
			answer = codeNotFound
		default:
			change(o, func(bk *bank) { bk.Settled = true })
			answer = codeSuccess
		}
	})
	return answer
}

func (v *Vendor) reverse(req verifyRequest) (answer string) {
	v.c.Do(func(orders map[string]*core.Order) {
		o := find(orders, req.SaleOrderID)
		if o == nil {
			answer = codeNotFound
			return
		}
		o.Count("reversal")
		change(o, func(bk *bank) { bk.Reversed = true })
		o.State, o.UpdatedAt = core.Reversed, time.Now()
		answer = codeSuccess
	})
	return answer
}

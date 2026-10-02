// Package gsmpay is a test double for GSMPay's credit payment gateway (a CPG):
// REST, the merchant code in every body, create, verify and reverse. It serves
// what the merchant's gsmpay driver calls.
package gsmpay

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// GSMPay's error envelope is {"type": "<code>"}; validation_error adds errors.
const (
	errValidation     = "validation_error"
	errMerchant       = "merchant_code_error"
	errToken          = "payment_token_error"
	errAmount         = "invoice_amount_error"
	errOrderStatus    = "invalid_order_status"
	errReverseExceeds = "reverse_amount_exceeded"
	errAuthorization  = "authorization_error"
	errServer         = "server_error"
)

// payment is what GSMPay holds beyond core.Order.
type payment struct {
	InvoiceReference string
	Reversed         int64
}

type Vendor struct {
	merchantCode string
	c            *core.Core
}

// New reads MERCHANT_CODE (default "fake-merchant").
func New() *Vendor {
	mc := os.Getenv("MERCHANT_CODE")
	if mc == "" {
		mc = "fake-merchant"
	}
	return &Vendor{merchantCode: mc}
}

func (v *Vendor) Name() string { return "gsmpay" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST /v1/cpg/payments", v.create)
	mux.HandleFunc("POST /v1/cpg/payments/verify", v.verify)
	mux.HandleFunc("POST /v1/cpg/payments/reverse", v.reverse)
	mux.HandleFunc("GET /pay/{token}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("token"), "", nil)
	})
	mux.HandleFunc("POST /pay/{token}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("token"))
	})
}

// Callback is GSMPay's documented return: a GET with token and status in the
// query, verify only on status=success. The failure value is not documented;
// A production driver reads neither field and decides by verify.
func (v *Vendor) Callback(o core.Order) core.Callback {
	status := "failed"
	if o.State == core.Settled {
		status = "success"
	}
	return core.Callback{URL: o.CallbackURL, Fields: map[string]string{"token": o.ID, "status": status}}
}

type view struct {
	Token            string          `json:"token"`
	InvoiceReference string          `json:"invoice_reference"`
	InvoiceAmount    int64           `json:"invoice_amount"`
	ReversedAmount   int64           `json:"reversed_amount"`
	State            core.State      `json:"state"`
	CallbackURL      string          `json:"callback_url"`
	RedirectURL      string          `json:"redirect_url"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Calls            map[string]int  `json:"calls"`
	Request          json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	p := o.Extra.(payment)
	return view{
		Token:            o.ID,
		InvoiceReference: p.InvoiceReference,
		InvoiceAmount:    o.Amount,
		ReversedAmount:   p.Reversed,
		State:            o.State,
		CallbackURL:      o.CallbackURL,
		RedirectURL:      v.c.PublicBaseURL + "/pay/" + url.PathEscape(o.ID),
		CreatedAt:        o.CreatedAt,
		UpdatedAt:        o.UpdatedAt,
		Calls:            o.Calls,
		Request:          o.Request,
	}
}

func fail(w http.ResponseWriter, typ string) {
	status := http.StatusUnprocessableEntity
	switch typ {
	case errAuthorization:
		status = http.StatusForbidden
	case errServer:
		status = http.StatusInternalServerError
	}
	core.WriteJSON(w, status, map[string]any{"type": typ})
}

func invalid(w http.ResponseWriter, field, msg string) {
	core.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"type": errValidation, "errors": map[string][]string{field: {msg}},
	})
}

// begin is the common head of every operation: the delay, a forced answer
// (an error type, or an HTTP status answered as server_error), the body, and
// the merchant code. It reports false once it has answered.
func (v *Vendor) begin(w http.ResponseWriter, r *http.Request, op string, req any) ([]byte, bool) {
	b := v.c.Behaviour()
	b.Wait()
	if f := b.Force[op]; f != "" {
		if status, err := strconv.Atoi(f); err == nil {
			core.WriteJSON(w, status, map[string]any{"type": errServer})
		} else {
			fail(w, f)
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
	var mc struct {
		MerchantCode string `json:"merchant_code"`
	}
	json.Unmarshal(body, &mc)
	if mc.MerchantCode != v.merchantCode {
		fail(w, errMerchant)
		return nil, false
	}
	return body, true
}

type item struct {
	Reference     string `json:"reference"`
	Name          string `json:"name"`
	Quantity      int64  `json:"quantity"`
	UnitPrice     string `json:"unit_price"`
	UnitDiscount  string `json:"unit_discount"`
	UnitTaxAmount string `json:"unit_tax_amount"`
}

func (v *Vendor) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackURL      string `json:"callback_url"`
		InvoiceReference string `json:"invoice_reference"`
		InvoiceAmount    int64  `json:"invoice_amount"`
		Items            []item `json:"items"`
	}
	body, ok := v.begin(w, r, "create", &req)
	if !ok {
		return
	}
	switch {
	case req.InvoiceReference == "":
		invalid(w, "invoice_reference", "The invoice reference field is required.")
		return
	case req.InvoiceAmount <= 0:
		invalid(w, "invoice_amount", "The invoice amount must be a positive integer.")
		return
	}

	items := make([]core.Item, len(req.Items))
	for i, it := range req.Items {
		price, _ := strconv.ParseInt(it.UnitPrice, 10, 64)
		items[i] = core.Item{Name: it.Name, Quantity: it.Quantity, Price: price, Total: price * it.Quantity}
	}
	now := time.Now()
	o := core.Order{
		ID:          rand.Text(),
		Amount:      req.InvoiceAmount,
		State:       v.c.Behaviour().Initial(core.Pending),
		CallbackURL: req.CallbackURL,
		Items:       items,
		Calls:       map[string]int{"create": 1},
		Request:     body,
		CreatedAt:   now,
		UpdatedAt:   now,
		Extra:       payment{InvoiceReference: req.InvoiceReference},
	}
	duplicate := false
	v.c.Do(func(orders map[string]*core.Order) {
		for _, other := range orders {
			if other.Extra.(payment).InvoiceReference == req.InvoiceReference {
				duplicate = true
				return
			}
		}
		orders[o.ID] = &o
	})
	if duplicate {
		invalid(w, "invoice_reference", "The invoice reference has already been taken.")
		return
	}
	slog.Info("gsmpay payment created", "token", o.ID, "invoice_reference", req.InvoiceReference, "amount", o.Amount, "state", o.State)
	core.WriteJSON(w, http.StatusCreated, map[string]any{"data": map[string]any{
		"token":        o.ID,
		"redirect_url": v.c.PublicBaseURL + "/pay/" + url.PathEscape(o.ID),
	}})
}

// verify answers is_paid by the order: true only for one that is settled and
// not reversed. It is idempotent; GSMPay documents no "already verified".
func (v *Vendor) verify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token            string `json:"token"`
		InvoiceReference string `json:"invoice_reference"`
		InvoiceAmount    int64  `json:"invoice_amount"`
	}
	if _, ok := v.begin(w, r, "verify", &req); !ok {
		return
	}
	o, ok := v.c.Update(req.Token, func(o *core.Order) { o.Count("verify") })
	switch {
	case !ok || o.Extra.(payment).InvoiceReference != req.InvoiceReference:
		fail(w, errToken)
		return
	case o.Amount != req.InvoiceAmount:
		fail(w, errAmount)
		return
	}
	core.WriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"token":          o.ID,
		"invoice_amount": o.Amount,
		"is_paid":        o.State == core.Settled,
	}})
}

// reverse refunds part or all of a paid payment; reversed_amount is the
// running total. A fully reversed payment moves to core.Reversed.
func (v *Vendor) reverse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token         string `json:"token"`
		ReverseAmount int64  `json:"reverse_amount"`
	}
	if _, ok := v.begin(w, r, "refund", &req); !ok {
		return
	}
	if req.ReverseAmount <= 0 {
		invalid(w, "reverse_amount", "The reverse amount must be a positive integer.")
		return
	}
	answer := ""
	o, ok := v.c.Update(req.Token, func(o *core.Order) {
		o.Count("refund")
		p := o.Extra.(payment)
		switch {
		case o.State != core.Settled:
			answer = errOrderStatus
		case p.Reversed+req.ReverseAmount > o.Amount:
			answer = errReverseExceeds
		default:
			p.Reversed += req.ReverseAmount
			o.Extra = p
			if p.Reversed == o.Amount {
				o.State = core.Reversed
			}
			o.UpdatedAt = time.Now()
		}
	})
	if !ok {
		answer = errToken
	}
	// A fully reversed payment is Reversed, so a further reverse lands on the
	// state check; GSMPay says reverse_amount_exceeded there.
	if answer == errOrderStatus && o.State == core.Reversed {
		answer = errReverseExceeds
	}
	if answer != "" {
		fail(w, answer)
		return
	}
	core.WriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"token":           o.ID,
		"reversed_amount": o.Extra.(payment).Reversed,
	}})
}

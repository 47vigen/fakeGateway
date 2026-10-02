// Package kamanlend is a test double for Kamanlend's lendtech gateway (a CPG):
// REST, the terminal code and secret in every body, RegisterPayment and
// GetPaymentState. A merchant's pre-check is a RegisterPayment whose redirect it
// throws away, so eligibility and the credit limit are decided there.
package kamanlend

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

// The SaleRequestState values this fake reports.
const (
	stateWaiting   = "WaitForCustomerApprove"
	stateCompleted = "PaymentCompleted"
	stateCanceled  = "Canceled"
)

// The refusal messages are Persian because the merchant shows the shopper only a
// Persian Kamanlend message («…: <reason>») and drops any other.
const (
	msgAuth        = "کد یا رمز پایانه نامعتبر است."
	msgInvalid     = "اطلاعات ارسال شده معتبر نیست."
	msgNotEligible = "مشتری واجد شرایط دریافت اعتبار نیست."
	msgOverLimit   = "اعتبار مشتری برای این خرید کافی نیست."
	msgPaid        = "این سبد خرید قبلا پرداخت شده است."
	msgNotFound    = "درخواست فروش یافت نشد."
	msgServer      = "خطای داخلی سرور."
)

// sale is what Kamanlend holds beyond core.Order.
type sale struct {
	ShoppingCardCode string
	NationalCode     string
}

type Vendor struct {
	terminalCode   string
	terminalSecret string
	creditLimit    int64
	c              *core.Core
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// New reads TERMINAL_CODE (fake-terminal), TERMINAL_SECRET (fake-secret) and
// CREDIT_LIMIT, every shopper's credit in Rial (1,000,000,000).
func New() *Vendor {
	limit, err := strconv.ParseInt(env("CREDIT_LIMIT", ""), 10, 64)
	if err != nil {
		limit = 1_000_000_000
	}
	return &Vendor{
		terminalCode:   env("TERMINAL_CODE", "fake-terminal"),
		terminalSecret: env("TERMINAL_SECRET", "fake-secret"),
		creditLimit:    limit,
	}
}

func (v *Vendor) Name() string { return "kamanlend" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST /api/Gateway/RegisterPayment", v.register)
	mux.HandleFunc("POST /api/Gateway/GetPaymentState", v.paymentState)
	mux.HandleFunc("GET /pay/{token}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("token"), "", nil)
	})
	mux.HandleFunc("POST /pay/{token}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("token"))
	})
}

// Callback is a GET to redirectionUrl carrying the token. Kamanlend's return
// parameters are not documented anywhere, and a production driver reads
// none: GetPaymentState decides.
func (v *Vendor) Callback(o core.Order) core.Callback {
	return core.Callback{URL: o.CallbackURL, Fields: map[string]string{"token": o.ID}}
}

func saleState(st core.State) string {
	switch st {
	case core.Settled:
		return stateCompleted
	case core.Pending:
		return stateWaiting
	}
	return stateCanceled
}

type view struct {
	Token            string          `json:"token"`
	ShoppingCardCode string          `json:"shopping_card_code"`
	NationalCode     string          `json:"customer_national_code"`
	TotalSaleAmount  int64           `json:"total_sale_amount_rial"`
	State            core.State      `json:"state"`
	SaleRequestState string          `json:"sale_request_state"`
	RedirectionURL   string          `json:"redirection_url"`
	GatewayURL       string          `json:"gateway_url"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Calls            map[string]int  `json:"calls"`
	Request          json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	s := o.Extra.(sale)
	return view{
		Token:            o.ID,
		ShoppingCardCode: s.ShoppingCardCode,
		NationalCode:     s.NationalCode,
		TotalSaleAmount:  o.Amount,
		State:            o.State,
		SaleRequestState: saleState(o.State),
		RedirectionURL:   o.CallbackURL,
		GatewayURL:       v.gatewayURL(o.ID),
		CreatedAt:        o.CreatedAt,
		UpdatedAt:        o.UpdatedAt,
		Calls:            o.Calls,
		Request:          o.Request,
	}
}

func (v *Vendor) gatewayURL(token string) string {
	return v.c.PublicBaseURL + "/pay/" + url.PathEscape(token)
}

// answer writes Kamanlend's ApiResult envelope.
func answer(w http.ResponseWriter, result any) {
	core.WriteJSON(w, http.StatusOK, map[string]any{"success": true, "messages": []any{}, "result": result})
}

// refuse writes the envelope with success false and one Error message.
func refuse(w http.ResponseWriter, status int, msg string) {
	core.WriteJSON(w, status, map[string]any{
		"success": false,
		"messages": []any{map[string]any{
			"messageKind": "Error", "message": msg, "messageDate": time.Now().Format(time.RFC3339),
		}},
		"result": nil,
	})
}

type credentials struct {
	TerminalCode         string `json:"terminalCode"`
	TerminalSecret       string `json:"terminalSecret"`
	CustomerNationalCode string `json:"customerNationalCode"`
}

// begin is the common head of both operations: the delay, a forced HTTP
// status, the body, and the terminal. It reports false once it has answered.
func (v *Vendor) begin(w http.ResponseWriter, r *http.Request, op string, req any) ([]byte, bool) {
	b := v.c.Behaviour()
	b.Wait()
	if status, err := strconv.Atoi(b.Force[op]); err == nil {
		refuse(w, status, msgServer)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err == nil {
		err = json.Unmarshal(body, req)
	}
	var cr credentials
	if err == nil {
		err = json.Unmarshal(body, &cr)
	}
	if err != nil {
		refuse(w, http.StatusBadRequest, msgInvalid)
		return nil, false
	}
	if cr.TerminalCode != v.terminalCode || cr.TerminalSecret != v.terminalSecret {
		refuse(w, http.StatusUnauthorized, msgAuth)
		return nil, false
	}
	return body, true
}

// register registers a sale and answers a token and the gateway URL. Every
// call is also an eligibility check: force.pre_check "not_eligible" refuses
// it, a number replaces CREDIT_LIMIT, and a sale over the limit is refused.
func (v *Vendor) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShoppingCardCode     string `json:"shoppingCardCode"`
		CustomerNationalCode string `json:"customerNationalCode"`
		RedirectionURL       string `json:"redirectionUrl"`
		SaleItems            []struct {
			Code            string `json:"code"`
			Title           string `json:"title"`
			Quantity        int64  `json:"quantity"`
			TotalAmountRial int64  `json:"totalAmountRial"`
		} `json:"saleItems"`
	}
	body, ok := v.begin(w, r, "create", &req)
	if !ok {
		return
	}
	var total int64
	items := make([]core.Item, len(req.SaleItems))
	for i, it := range req.SaleItems {
		total += it.TotalAmountRial
		price := it.TotalAmountRial
		if it.Quantity > 0 {
			price /= it.Quantity
		}
		items[i] = core.Item{Name: it.Title, Quantity: it.Quantity, Price: price, Total: it.TotalAmountRial}
	}
	if req.ShoppingCardCode == "" || req.CustomerNationalCode == "" || len(items) == 0 || total <= 0 {
		refuse(w, http.StatusBadRequest, msgInvalid)
		return
	}

	b := v.c.Behaviour()
	limit := v.creditLimit
	switch f := b.Force["pre_check"]; f {
	case "":
	case "not_eligible":
		refuse(w, http.StatusBadRequest, msgNotEligible)
		return
	default:
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			limit = n
		}
	}
	if total > limit {
		refuse(w, http.StatusBadRequest, msgOverLimit)
		return
	}

	now := time.Now()
	o := core.Order{
		ID:          rand.Text(),
		Amount:      total,
		State:       b.Initial(core.Pending),
		CallbackURL: req.RedirectionURL,
		Items:       items,
		Calls:       map[string]int{"create": 1},
		Request:     body,
		CreatedAt:   now,
		UpdatedAt:   now,
		Extra:       sale{ShoppingCardCode: req.ShoppingCardCode, NationalCode: req.CustomerNationalCode},
	}
	// A pending code may be registered again: the merchant's pre-check and the
	// real leg can carry the same code. A completed one is refused.
	paid := false
	v.c.Do(func(orders map[string]*core.Order) {
		for _, other := range orders {
			if other.Extra.(sale).ShoppingCardCode == req.ShoppingCardCode && other.State == core.Settled {
				paid = true
				return
			}
		}
		orders[o.ID] = &o
	})
	if paid {
		refuse(w, http.StatusBadRequest, msgPaid)
		return
	}
	slog.Info("kamanlend sale registered", "token", o.ID, "shopping_card_code", req.ShoppingCardCode, "amount", total, "state", o.State)
	answer(w, map[string]any{"token": o.ID, "gatewayUrl": v.gatewayURL(o.ID)})
}

// paymentState reports the sale's state. force.verify takes an HTTP status or
// a SaleRequestState name to report instead.
func (v *Vendor) paymentState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	var cr credentials
	body, ok := v.begin(w, r, "verify", &req)
	if !ok {
		return
	}
	json.Unmarshal(body, &cr)
	o, ok := v.c.Update(req.Token, func(o *core.Order) { o.Count("verify") })
	if !ok || o.Extra.(sale).NationalCode != cr.CustomerNationalCode {
		refuse(w, http.StatusBadRequest, msgNotFound)
		return
	}
	st := saleState(o.State)
	if f := v.c.Behaviour().Force["verify"]; f != "" {
		st = f
	}
	answer(w, map[string]any{
		"totalSaleAmountRial":   o.Amount,
		"cashPaymentAmountRial": 0,
		"initializeDate":        o.CreatedAt.Format(time.RFC3339),
		"lastUpdateDate":        o.UpdatedAt.Format(time.RFC3339),
		"saleRequestState":      st,
		"saleRequestStateTitle": st,
	})
}

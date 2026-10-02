// Package vibe imitates the HTTP contract of the "vibe" credit gateway (a CPG)
// that the merchant drives: REST, an X-API-Key header, a create and a verify.
package vibe

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

type Vendor struct {
	apiKey string
	c      *core.Core
}

// New reads API_KEY (default "fake-key").
func New() *Vendor {
	key := os.Getenv("API_KEY")
	if key == "" {
		key = "fake-key"
	}
	return &Vendor{apiKey: key}
}

func (v *Vendor) Name() string { return "vibe" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST /api/v1/orders/", v.createOrder)
	mux.HandleFunc("POST /api/v1/orders/verify", v.verifyOrder)
	mux.HandleFunc("GET /pay/{order_id}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("order_id"), "", nil)
	})
	mux.HandleFunc("POST /pay/{order_id}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("order_id"))
	})
}

// Callback carries the provider's documented parameters in the query:
// `result=OK|NOK` and `ref_id=<order_id>`. A merchant's callback route takes both
// GET and POST and merges query with body, so a GET carries everything.
func (v *Vendor) Callback(o core.Order) core.Callback {
	result := "NOK"
	if o.State == core.Settled {
		result = "OK"
	}
	return core.Callback{URL: o.CallbackURL, Fields: map[string]string{"result": result, "ref_id": o.ID}}
}

type view struct {
	OrderID     string     `json:"order_id"`
	State       core.State `json:"state"`
	CartAmount  int64      `json:"cart_amount"`
	CallbackURL string     `json:"callback_url"`
	PaymentURL  string     `json:"payment_url"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Request is the create body exactly as it arrived, for a test asserting
	// what the merchant asked the gateway for.
	Request json.RawMessage `json:"request"`
	Calls   map[string]int  `json:"calls"`
}

func (v *Vendor) View(o core.Order) any {
	return view{
		OrderID:     o.ID,
		State:       o.State,
		CartAmount:  o.Amount,
		CallbackURL: o.CallbackURL,
		PaymentURL:  v.paymentURL(o.ID),
		CreatedAt:   o.CreatedAt,
		UpdatedAt:   o.UpdatedAt,
		Request:     o.Request,
		Calls:       o.Calls,
	}
}

func (v *Vendor) paymentURL(id string) string {
	return v.c.PublicBaseURL + "/pay/" + url.PathEscape(id)
}

// flexID accepts the `string | integer` the provider's OrderCreateDto declares
// for `order_id` and for an item `id`. Numbers are kept as their literal bytes
// so a large id does not lose precision through float64.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*f = ""
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexID(v)
		return nil
	}
	*f = flexID(s)
	return nil
}

type item struct {
	ID       flexID `json:"id"`
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Quantity int64  `json:"quantity"`
	Discount int64  `json:"discount"`
}

type orderData struct {
	Tax         int64  `json:"tax"`
	GoodsAmount int64  `json:"goods_amount"`
	Items       []item `json:"items"`
}

type createRequest struct {
	OrderID               flexID    `json:"order_id"`
	CartAmount            int64     `json:"cart_amount"`
	CartAmountInstallment *int64    `json:"cart_amount_installment"`
	Installments          *int64    `json:"installments"`
	CallbackURL           string    `json:"callback_url"`
	Data                  orderData `json:"data"`
}

// authorized mirrors the provider's APIKeyHeader security scheme, down to the
// 401 body shape.
func (v *Vendor) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-API-Key") == v.apiKey {
		return true
	}
	core.WriteJSON(w, http.StatusUnauthorized, map[string]any{
		"code":   1001,
		"detail": "Invalid API key provided",
		"data":   nil,
	})
	return false
}

// validationError answers in the provider's 422 shape.
func validationError(w http.ResponseWriter, loc []any, msg string) {
	core.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"detail": []any{map[string]any{"loc": loc, "msg": msg, "type": "value_error"}},
	})
}

// forced answers a status forced through the control API (force.create,
// force.verify), in the shape the provider uses for its own 4xx/5xx envelopes.
func forced(w http.ResponseWriter, b core.Behaviour, op string) bool {
	status, err := strconv.Atoi(b.Force[op])
	if err != nil || status < 100 || status > 999 {
		return false
	}
	core.WriteJSON(w, status, map[string]any{
		"code":   5007,
		"detail": "forced fault from the fake gateway control API",
		"data":   nil,
	})
	return true
}

func (v *Vendor) createOrder(w http.ResponseWriter, r *http.Request) {
	if !v.authorized(w, r) {
		return
	}
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "create") {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err != nil {
		validationError(w, []any{"body"}, "could not read request body: "+err.Error())
		return
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		validationError(w, []any{"body"}, "invalid JSON: "+err.Error())
		return
	}

	switch {
	case req.OrderID == "":
		validationError(w, []any{"body", "order_id"}, "field required")
		return
	case req.CartAmount <= 0:
		validationError(w, []any{"body", "cart_amount"}, "must be a positive integer amount in Rial")
		return
	case len(req.Data.Items) == 0:
		validationError(w, []any{"body", "data", "items"}, "at least one item is required")
		return
	}
	if b.EnforceAmounts && req.CartAmount != req.Data.GoodsAmount+req.Data.Tax {
		validationError(w, []any{"body", "cart_amount"}, fmt.Sprintf(
			"cart_amount (%d) must equal data.goods_amount + data.tax (%d + %d = %d)",
			req.CartAmount, req.Data.GoodsAmount, req.Data.Tax, req.Data.GoodsAmount+req.Data.Tax))
		return
	}

	items := make([]core.Item, len(req.Data.Items))
	for i, it := range req.Data.Items {
		items[i] = core.Item{Name: it.Name, Quantity: it.Quantity, Price: it.Price, Total: it.Price*it.Quantity - it.Discount}
	}
	now := time.Now()
	o := core.Order{
		ID:          string(req.OrderID),
		Amount:      req.CartAmount,
		State:       b.Initial(core.Pending),
		CallbackURL: req.CallbackURL,
		Items:       items,
		Calls:       map[string]int{"create": 1},
		Request:     json.RawMessage(body),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if !v.c.Create(o) {
		validationError(w, []any{"body", "order_id"}, "order_id already exists")
		return
	}

	slog.Info("order created",
		"order_id", o.ID,
		"cart_amount", o.Amount,
		"goods_amount", req.Data.GoodsAmount,
		"tax", req.Data.Tax,
		"items", len(req.Data.Items),
		"callback_url", o.CallbackURL,
		"state", o.State,
	)
	core.WriteJSON(w, http.StatusOK, map[string]any{"payment_url": v.paymentURL(o.ID)})
}

func (v *Vendor) verifyOrder(w http.ResponseWriter, r *http.Request) {
	if !v.authorized(w, r) {
		return
	}
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "verify") {
		return
	}

	var req struct {
		RefID flexID `json:"ref_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, core.MaxBody)).Decode(&req); err != nil {
		validationError(w, []any{"body"}, "invalid JSON: "+err.Error())
		return
	}
	if req.RefID == "" {
		validationError(w, []any{"body", "ref_id"}, "field required")
		return
	}

	// The real provider answers 200 with `status: false` for an order it cannot
	// find or has not finalised — it does not 404. A resolved request is not
	// proof the money moved, and this fake must fail a test the same way.
	status, message := false, "order not found"
	if o, ok := v.c.Update(string(req.RefID), func(o *core.Order) { o.Count("verify") }); ok {
		switch o.State {
		case core.Settled:
			status, message = true, "order settled"
		default:
			message = "order not finalised (" + string(o.State) + ")"
		}
	}

	slog.Info("order verified", "ref_id", string(req.RefID), "status", status, "message", message)
	core.WriteJSON(w, http.StatusOK, map[string]any{"status": status, "message": message})
}

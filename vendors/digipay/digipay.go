// Package digipay imitates Digipay's UPG (a CPG) as the merchant's digipay driver
// drives it: an OAuth2 password grant, a business ticket, a browser POST back,
// then verify, deliver and refund, each keyed by Digipay's own trackingCode
// and the purchase type the shopper paid with.
package digipay

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// Digipay's result.status codes (جدول 49). Only 0 is a success.
const (
	statusOK            = 0
	statusBadInput      = 1054
	statusNotFound      = 9000
	statusInProgress    = 9004
	statusNotSuccessful = 9007
	statusDifferentData = 9008
	statusWrongState    = 9012
)

// messages are Digipay's own wording for each status, as its envelope
// carries them.
var messages = map[int]string{
	statusOK:            "عملیات با موفقیت انجام شد",
	statusBadInput:      "اطلاعات ورودی اشتباه می باشد",
	statusNotFound:      "اطلاعات خرید یافت نشد",
	9001:                "توکن پرداخت معتبر نمی باشد",
	9003:                "خرید مورد نظر منقضی شده است",
	statusInProgress:    "خرید مورد نظر درحال انجام است",
	9005:                "خرید قابل پرداخت نمی باشد",
	9006:                "خطا در برقراری ارتباط با درگاه پرداخت",
	statusNotSuccessful: "خرید با موفقیت انجام نشده است",
	statusDifferentData: "این خرید با داده های متفاوتی قبلا ثبت شده است",
	9009:                "محدوده زمانی تایید تراکنش گذشته است",
	9010:                "تایید خرید ناموفق بود",
	9011:                "نتیجه تایید خرید نامشخص است",
	statusWrongState:    "وضعیت خرید برای این درخواست صحیح نمی باشد",
}

// ticketType is the only `type` ticket creation takes: UPG, for every feature.
const ticketType = "11"

// Purchase types (جدول 18): what verify, deliver and refund take as `type`.
const (
	typeIPG    = 0
	typeCredit = 5
	typeWallet = 11
	typeBNPL   = 13
	typeCard   = 24
)

var purchaseTypes = map[int]bool{typeIPG: true, typeCredit: true, typeWallet: true, typeBNPL: true, typeCard: true}

// refunded is the vendor state an order takes once its money went back.
const refunded core.State = "refunded"

// purchase is what Digipay holds about an order beyond core.Order.
type purchase struct {
	ProviderID         string
	TrackingCode       string
	PurchaseType       int
	RRN                string
	Verified           bool
	Delivered          bool
	InvoiceNumber      string
	Refunded           bool
	RefundTrackingCode string
	RefundProviderID   string
}

type Vendor struct {
	clientID, clientSecret string
	username, password     string
	purchaseType           int
	c                      *core.Core

	mu      sync.Mutex
	access  map[string]bool
	refresh map[string]bool
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// New reads CLIENT_ID, CLIENT_SECRET, USERNAME, PASSWORD and PURCHASE_TYPE,
// the method every shopper "pays" with (default 5, CREDIT, so deliver works).
func New() *Vendor {
	pt, err := strconv.Atoi(env("PURCHASE_TYPE", "5"))
	if err != nil || !purchaseTypes[pt] {
		slog.Warn("PURCHASE_TYPE is not a Digipay purchase type; using 5", "value", os.Getenv("PURCHASE_TYPE"))
		pt = typeCredit
	}
	return newVendor(env("CLIENT_ID", "fake-client"), env("CLIENT_SECRET", "fake-secret"),
		env("USERNAME", "fake-user"), env("PASSWORD", "fake-password"), pt)
}

func newVendor(clientID, clientSecret, username, password string, purchaseType int) *Vendor {
	return &Vendor{
		clientID:     clientID,
		clientSecret: clientSecret,
		username:     username,
		password:     password,
		purchaseType: purchaseType,
		access:       map[string]bool{},
		refresh:      map[string]bool{},
	}
}

func (v *Vendor) Name() string { return "digipay" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST /oauth/token", v.token)
	mux.HandleFunc("POST /tickets/business", v.authed(v.create))
	mux.HandleFunc("POST /purchases/verify", v.authed(v.verify))
	mux.HandleFunc("POST /purchases/deliver", v.authed(v.deliver))
	mux.HandleFunc("POST /refunds", v.authed(v.refund))
	mux.HandleFunc("GET /web-pay/tgs/{ticket}", func(w http.ResponseWriter, r *http.Request) {
		c.PayPage(w, r.PathValue("ticket"), "", nil)
	})
	mux.HandleFunc("POST /web-pay/tgs/{ticket}", func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, r.PathValue("ticket"))
	})
}

// Callback is the form Digipay's page posts to callbackUrl. The driver reads
// trackingCode and type, and refuses a body whose providerId or amount is not
// the leg's.
func (v *Vendor) Callback(o core.Order) core.Callback {
	p := o.Extra.(purchase)
	result := "FAILURE"
	if o.State == core.Settled {
		result = "SUCCESS"
	}
	fields := map[string]string{
		"result":       result,
		"providerId":   p.ProviderID,
		"trackingCode": p.TrackingCode,
		"amount":       strconv.FormatInt(o.Amount, 10),
		"type":         strconv.Itoa(p.PurchaseType),
		"isCredit":     strconv.FormatBool(isCredit(p.PurchaseType)),
	}
	if p.PurchaseType == typeIPG {
		fields["rrn"] = p.RRN
	}
	return core.Callback{URL: o.CallbackURL, Post: true, Fields: fields}
}

type view struct {
	Ticket             string          `json:"ticket"`
	ProviderID         string          `json:"provider_id"`
	TrackingCode       string          `json:"tracking_code"`
	PurchaseType       int             `json:"purchase_type"`
	Amount             int64           `json:"amount"`
	CallbackURL        string          `json:"callback_url"`
	PaymentURL         string          `json:"payment_url"`
	State              core.State      `json:"state"`
	Verified           bool            `json:"verified"`
	Delivered          bool            `json:"delivered"`
	InvoiceNumber      string          `json:"invoice_number,omitempty"`
	Refunded           bool            `json:"refunded"`
	RefundTrackingCode string          `json:"refund_tracking_code,omitempty"`
	RefundProviderID   string          `json:"refund_provider_id,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	Calls              map[string]int  `json:"calls"`
	Request            json.RawMessage `json:"request"`
}

func (v *Vendor) View(o core.Order) any {
	p := o.Extra.(purchase)
	return view{
		Ticket:             o.ID,
		ProviderID:         p.ProviderID,
		TrackingCode:       p.TrackingCode,
		PurchaseType:       p.PurchaseType,
		Amount:             o.Amount,
		CallbackURL:        o.CallbackURL,
		PaymentURL:         v.paymentURL(o.ID),
		State:              o.State,
		Verified:           p.Verified,
		Delivered:          p.Delivered,
		InvoiceNumber:      p.InvoiceNumber,
		Refunded:           p.Refunded,
		RefundTrackingCode: p.RefundTrackingCode,
		RefundProviderID:   p.RefundProviderID,
		CreatedAt:          o.CreatedAt,
		UpdatedAt:          o.UpdatedAt,
		Calls:              o.Calls,
		Request:            o.Request,
	}
}

func (v *Vendor) paymentURL(ticket string) string {
	return v.c.PublicBaseURL + "/web-pay/tgs/" + url.PathEscape(ticket)
}

func isCredit(t int) bool { return t == typeCredit || t == typeBNPL }

// paymentGateway is the verify response's coarser enum (جدول 25): IPG 0,
// WALLET 3, CPG 4. Digipay places CREDIT-CARD nowhere; the fake calls it IPG.
func paymentGateway(t int) int {
	switch t {
	case typeWallet:
		return 3
	case typeCredit, typeBNPL:
		return 4
	}
	return 0
}

// apiResult is Digipay's `result` envelope.
type apiResult struct {
	Title   string `json:"title,omitempty"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Level   string `json:"level"`
}

func success() apiResult {
	return apiResult{Title: "SUCCESS", Status: statusOK, Message: messages[statusOK], Level: "INFO"}
}

// refusal answers one of Digipay's codes in its ErrorResponse: 1054 on 400,
// every business refusal on 422 (جدول 48).
func refusal(w http.ResponseWriter, status int) {
	httpStatus := http.StatusUnprocessableEntity
	if status == statusBadInput {
		httpStatus = http.StatusBadRequest
	}
	msg := messages[status]
	if msg == "" {
		msg = "forced refusal from the fake gateway control API"
	}
	core.WriteJSON(w, httpStatus, map[string]any{"result": apiResult{Status: status, Message: msg, Level: "ERROR"}})
}

// forced answers force[op]: a value under 600 is that HTTP status, anything
// else a Digipay result.status.
func forced(w http.ResponseWriter, b core.Behaviour, op string) bool {
	n, err := strconv.Atoi(b.Force[op])
	if err != nil || n <= 0 {
		return false
	}
	if n >= 100 && n < 600 {
		core.WriteJSON(w, n, map[string]any{"result": apiResult{Status: n, Message: "forced fault from the fake gateway control API", Level: "ERROR"}})
		return true
	}
	refusal(w, n)
	return true
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	core.WriteJSON(w, status, map[string]any{"error": code, "error_description": desc})
}

// token is the OAuth2 endpoint: the client in a Basic header, a password or a
// refresh_token grant in a urlencoded form.
func (v *Vendor) token(w http.ResponseWriter, r *http.Request) {
	b := v.c.Behaviour()
	b.Wait()
	if forced(w, b, "token") {
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok || id != v.clientID || secret != v.clientSecret {
		oauthError(w, http.StatusUnauthorized, "unauthorized", "Bad client credentials")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "password":
		if r.PostForm.Get("username") != v.username || r.PostForm.Get("password") != v.password {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "Bad credentials")
			return
		}
	case "refresh_token":
		if !v.refresh[r.PostForm.Get("refresh_token")] {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "Invalid refresh token")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant type: "+r.PostForm.Get("grant_type"))
		return
	}
	access, refresh := rand.Text(), rand.Text()
	v.access[access], v.refresh[refresh] = true, true
	core.WriteJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "bearer",
		"refresh_token": refresh,
		"expires_in":    3599,
		"scope":         "USER WALLET_BUSINESS_INTEGRATION BUSINESS_API",
		"jti":           rand.Text(),
	})
}

// authed requires a Bearer token this fake issued, and waits DelayMS.
func (v *Vendor) authed(next func(http.ResponseWriter, *http.Request, core.Behaviour)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		v.mu.Lock()
		ok = ok && v.access[tok]
		v.mu.Unlock()
		if !ok {
			oauthError(w, http.StatusUnauthorized, "invalid_token", "Invalid access token: "+tok)
			return
		}
		b := v.c.Behaviour()
		b.Wait()
		next(w, r, b)
	}
}

// decode reads a JSON body, answering 1054 when it cannot.
func decode(w http.ResponseWriter, r *http.Request, into any) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err == nil {
		err = json.Unmarshal(body, into)
	}
	if err != nil {
		refusal(w, statusBadInput)
		return nil, false
	}
	return body, true
}

// queryType is the purchase type in ?type=, answering 1054 when it is not one.
func queryType(w http.ResponseWriter, r *http.Request) (int, bool) {
	t, err := strconv.Atoi(r.URL.Query().Get("type"))
	if err != nil || !purchaseTypes[t] {
		refusal(w, statusBadInput)
		return 0, false
	}
	return t, true
}

func digits(n int) string {
	s := make([]byte, n)
	for i := range s {
		s[i] = byte('0' + mrand.IntN(10))
	}
	if s[0] == '0' {
		s[0] = '1'
	}
	return string(s)
}

type ticketRequest struct {
	Amount      int64  `json:"amount"`
	CellNumber  string `json:"cellNumber"`
	ProviderID  string `json:"providerId"`
	CallbackURL string `json:"callbackUrl"`
}

func (v *Vendor) create(w http.ResponseWriter, r *http.Request, b core.Behaviour) {
	if forced(w, b, "create") {
		return
	}
	if r.URL.Query().Get("type") != ticketType {
		refusal(w, statusBadInput)
		return
	}
	var req ticketRequest
	body, ok := decode(w, r, &req)
	if !ok {
		return
	}
	if req.Amount < 1000 || req.CellNumber == "" || req.ProviderID == "" || req.CallbackURL == "" {
		refusal(w, statusBadInput)
		return
	}

	now := time.Now()
	o := core.Order{
		ID:          fmt.Sprintf("v2:%016x%016x", mrand.Uint64(), mrand.Uint64()),
		Amount:      req.Amount,
		State:       b.Initial(core.Pending),
		CallbackURL: req.CallbackURL,
		Calls:       map[string]int{"create": 1},
		Request:     body,
		CreatedAt:   now,
		UpdatedAt:   now,
		Extra: purchase{
			ProviderID:   req.ProviderID,
			TrackingCode: digits(23),
			PurchaseType: v.purchaseType,
			RRN:          digits(12),
		},
	}
	duplicate := false
	v.c.Do(func(orders map[string]*core.Order) {
		for _, existing := range orders {
			if existing.Extra.(purchase).ProviderID == req.ProviderID {
				existing.Count("create")
				duplicate = true
				return
			}
		}
		orders[o.ID] = &o
	})
	if duplicate {
		refusal(w, statusDifferentData)
		return
	}
	slog.Info("ticket created", "ticket", o.ID, "provider_id", req.ProviderID, "amount", o.Amount, "state", o.State)
	core.WriteJSON(w, http.StatusOK, map[string]any{
		"result":      success(),
		"ticket":      o.ID,
		"redirectUrl": v.paymentURL(o.ID),
	})
}

// find is the order Digipay knows by trackingCode.
// ponytail: a linear scan; a test double holds a handful of orders.
func find(orders map[string]*core.Order, trackingCode string) *core.Order {
	for _, o := range orders {
		if o.Extra.(purchase).TrackingCode == trackingCode {
			return o
		}
	}
	return nil
}

// operate runs fn on the order behind trackingCode under the store lock,
// counting op. fn answers a Digipay status and, on 0, the success body.
func (v *Vendor) operate(w http.ResponseWriter, op, trackingCode string, fn func(o *core.Order, p *purchase) (int, map[string]any)) {
	status, body := statusNotFound, map[string]any(nil)
	v.c.Do(func(orders map[string]*core.Order) {
		o := find(orders, trackingCode)
		if o == nil {
			return
		}
		o.Count(op)
		p := o.Extra.(purchase)
		status, body = fn(o, &p)
		o.Extra = p
	})
	slog.Info("digipay "+op, "tracking_code", trackingCode, "status", status)
	if status != statusOK {
		refusal(w, status)
		return
	}
	body["result"] = success()
	core.WriteJSON(w, http.StatusOK, body)
}

func (v *Vendor) verify(w http.ResponseWriter, r *http.Request, b core.Behaviour) {
	if forced(w, b, "verify") {
		return
	}
	t, ok := queryType(w, r)
	if !ok {
		return
	}
	var req struct {
		TrackingCode string `json:"trackingCode"`
		ProviderID   string `json:"providerId"`
	}
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if req.TrackingCode == "" || req.ProviderID == "" {
		refusal(w, statusBadInput)
		return
	}
	v.operate(w, "verify", req.TrackingCode, func(o *core.Order, p *purchase) (int, map[string]any) {
		switch {
		case p.ProviderID != req.ProviderID:
			return statusNotFound, nil
		case p.PurchaseType != t:
			return statusBadInput, nil
		case p.Refunded || o.State == core.Reversed:
			return statusWrongState, nil
		case o.State == core.Failed:
			return statusNotSuccessful, nil
		case o.State != core.Settled:
			return statusInProgress, nil
		case p.Verified:
			return statusWrongState, nil
		}
		p.Verified = true
		res := map[string]any{
			"trackingCode":   p.TrackingCode,
			"providerId":     p.ProviderID,
			"amount":         o.Amount,
			"paymentGateway": paymentGateway(p.PurchaseType),
		}
		if isCredit(p.PurchaseType) {
			res["fpCode"], res["fpName"] = "7", "DIGIPAY"
			res["additionalInfo"] = map[string]any{
				"prepaymentAmount": 0, "cashAmount": 0, "creditAmount": o.Amount,
				"instantFinalization": false, "generateInvoice": false,
			}
		} else {
			res["terminalId"], res["rrn"], res["maskedPan"] = "44579180", p.RRN, "502229******7467"
			res["pspCode"], res["pspName"] = "002", "PARSIAN"
		}
		return statusOK, res
	})
}

func (v *Vendor) deliver(w http.ResponseWriter, r *http.Request, b core.Behaviour) {
	if forced(w, b, "deliver") {
		return
	}
	t, ok := queryType(w, r)
	if !ok {
		return
	}
	var req struct {
		TrackingCode  string   `json:"trackingCode"`
		DeliveryDate  int64    `json:"deliveryDate"`
		InvoiceNumber string   `json:"invoiceNumber"`
		Products      []string `json:"products"`
	}
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if req.TrackingCode == "" || req.InvoiceNumber == "" || req.DeliveryDate == 0 {
		refusal(w, statusBadInput)
		return
	}
	v.operate(w, "deliver", req.TrackingCode, func(o *core.Order, p *purchase) (int, map[string]any) {
		switch {
		case p.PurchaseType != t:
			return statusBadInput, nil
		case !isCredit(p.PurchaseType), !p.Verified, p.Refunded, p.Delivered:
			return statusWrongState, nil
		}
		p.Delivered, p.InvoiceNumber = true, req.InvoiceNumber
		o.UpdatedAt = time.Now()
		return statusOK, map[string]any{}
	})
}

func (v *Vendor) refund(w http.ResponseWriter, r *http.Request, b core.Behaviour) {
	if forced(w, b, "refund") {
		return
	}
	t, ok := queryType(w, r)
	if !ok {
		return
	}
	var req struct {
		ProviderID       string `json:"providerId"`
		Amount           int64  `json:"amount"`
		SaleTrackingCode string `json:"saleTrackingCode"`
	}
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if req.ProviderID == "" || req.SaleTrackingCode == "" || req.Amount <= 0 {
		refusal(w, statusBadInput)
		return
	}
	v.operate(w, "refund", req.SaleTrackingCode, func(o *core.Order, p *purchase) (int, map[string]any) {
		switch {
		// The refund's providerId must differ from the purchase's.
		case p.PurchaseType != t, req.ProviderID == p.ProviderID, req.Amount > o.Amount:
			return statusBadInput, nil
		case !p.Verified, p.Refunded:
			return statusWrongState, nil
		}
		p.Refunded, p.RefundTrackingCode, p.RefundProviderID = true, digits(12), req.ProviderID
		o.State, o.UpdatedAt = refunded, time.Now()
		return statusOK, map[string]any{"trackingCode": p.RefundTrackingCode}
	})
}

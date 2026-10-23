// Package sep imitates SEP (Saman), a token based IPG, as a production driver
// drives it: a JSON token call, the shopper sent to SendToken, a browser POST
// back to the merchant, then VerifyTransaction and ReverseTransaction.
package sep

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/47vigen/fakeGateway/core"
)

// The paths SEP serves.
const (
	pathToken     = "/OnlinePG/OnlinePG"
	pathSendToken = "/OnlinePG/SendToken"
	pathVerify    = "/verifyTxnRandomSessionkey/ipg/VerifyTransaction"
	pathReverse   = "/verifyTxnRandomSessionkey/ipg/ReverseTransaction"
)

// bank is what SEP holds about an order beyond core.Order.
type bank struct {
	Token string
	// RefNum is minted with the token and shown to the merchant only once the
	// shopper has paid.
	RefNum   string
	Verified int
}

type Vendor struct {
	terminal string
	c        *core.Core
	sequence int64 // guarded by the core lock
}

// New reads TERMINAL_ID (default "1000"). It must be a number: the verify body
// carries it as one.
func New() *Vendor {
	t := os.Getenv("TERMINAL_ID")
	if t == "" {
		t = "1000"
	}
	return &Vendor{terminal: t}
}

func (v *Vendor) Name() string { return "sep" }

func (v *Vendor) Routes(mux *http.ServeMux, c *core.Core) {
	v.c = c
	mux.HandleFunc("POST "+pathToken, v.token)
	mux.HandleFunc("POST "+pathVerify, func(w http.ResponseWriter, r *http.Request) { v.settle(w, r, "verify") })
	mux.HandleFunc("POST "+pathReverse, func(w http.ResponseWriter, r *http.Request) { v.settle(w, r, "reverse") })
	// The shopper's browser arrives by GET with the token, and the page posts
	// back to the same path.
	mux.HandleFunc("GET "+pathSendToken, func(w http.ResponseWriter, r *http.Request) {
		tok := r.FormValue("token")
		c.PayPage(w, v.idOf(tok), pathSendToken, map[string]string{"token": tok})
	})
	mux.HandleFunc("POST "+pathSendToken, func(w http.ResponseWriter, r *http.Request) {
		c.PaySubmit(w, r, v.idOf(r.FormValue("token")))
	})
}

// idOf is the ResNum a token was issued for, "" when there is none.
// ponytail: a linear scan; a test double holds a handful of orders.
func (v *Vendor) idOf(token string) (id string) {
	if token == "" {
		return ""
	}
	v.c.Do(func(orders map[string]*core.Order) {
		for _, o := range orders {
			if o.Extra.(bank).Token == token {
				id = o.ID
			}
		}
	})
	return id
}

// Callback is the form SEP's page posts to the RedirectUrl. A shopper who does
// not pay comes back as CanceledByUser with no RefNum.
func (v *Vendor) Callback(o core.Order) core.Callback {
	b := o.Extra.(bank)
	state, status, refNum := "CanceledByUser", "1", ""
	if o.State == core.Settled || o.State == core.Reversed {
		state, status, refNum = "OK", "2", b.RefNum
	}
	return core.Callback{URL: o.CallbackURL, Post: true, Fields: map[string]string{
		"State": state, "Status": status, "RefNum": refNum, "ResNum": o.ID,
		"TerminalId": v.terminal, "Amount": strconv.FormatInt(o.Amount, 10),
	}}
}

type view struct {
	ResNum      string     `json:"res_num"`
	Token       string     `json:"token"`
	RefNum      string     `json:"ref_num"`
	Amount      int64      `json:"amount"`
	State       core.State `json:"state"`
	RedirectURL string     `json:"redirect_url"`
	PaymentURL  string     `json:"payment_url"`
	Verified    int        `json:"verified"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Request is the token request as it arrived.
	Request json.RawMessage `json:"request"`
	Calls   map[string]int  `json:"calls"`
}

func (v *Vendor) View(o core.Order) any {
	b := o.Extra.(bank)
	return view{
		ResNum: o.ID, Token: b.Token, RefNum: b.RefNum, Amount: o.Amount, State: o.State,
		RedirectURL: o.CallbackURL,
		PaymentURL:  v.c.PublicBaseURL + pathSendToken + "?token=" + url.QueryEscape(b.Token),
		Verified:    b.Verified, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, Request: o.Request, Calls: o.Calls,
	}
}

// forcedStatus answers a forced value of 100 to 599 as that HTTP status.
func forcedStatus(w http.ResponseWriter, forced string) bool {
	n, err := strconv.Atoi(forced)
	if err != nil || n < 100 || n > 599 {
		return false
	}
	core.WriteJSON(w, n, map[string]any{"error": "forced fault from the fake gateway control API"})
	return true
}

func tokenError(w http.ResponseWriter, code, description string) {
	core.WriteJSON(w, http.StatusOK, map[string]any{"status": -1, "errorCode": code, "errorDesc": description})
}

func (v *Vendor) token(w http.ResponseWriter, r *http.Request) {
	b := v.c.Behaviour()
	b.Wait()
	forced := b.Force["token"]
	if forcedStatus(w, forced) {
		return
	}
	if forced != "" {
		tokenError(w, forced, "forced by the fake gateway control API")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxBody))
	if err != nil {
		tokenError(w, "5", "the body could not be read")
		return
	}
	var req struct {
		Action      string `json:"action"`
		TerminalID  string `json:"TerminalId"`
		Amount      int64  `json:"Amount"`
		ResNum      string `json:"ResNum"`
		RedirectURL string `json:"RedirectUrl"`
	}
	switch err := json.Unmarshal(body, &req); {
	case err != nil:
		tokenError(w, "5", "invalid JSON")
		return
	case !strings.EqualFold(req.Action, "token"), req.Amount <= 0, req.ResNum == "", len(req.ResNum) > 50, req.RedirectURL == "":
		tokenError(w, "5", "invalid parameters")
		return
	case req.TerminalID != v.terminal:
		tokenError(w, "12", "terminal not found")
		return
	}

	var tok string
	v.c.Do(func(orders map[string]*core.Order) {
		v.sequence++
		now := time.Now()
		tok = fmt.Sprintf("token-%d", v.sequence)
		o := &core.Order{
			ID: req.ResNum, Amount: req.Amount, State: b.Initial(core.Pending), CallbackURL: req.RedirectURL,
			Calls: map[string]int{}, Request: json.RawMessage(body), CreatedAt: now, UpdatedAt: now,
			Extra: bank{Token: tok, RefNum: fmt.Sprintf("refnum-%d-%s", v.sequence, req.ResNum)},
		}
		// A repeated ResNum replaces the earlier token, as a retried token call
		// after a lost answer would meet; the ledger keeps counting.
		if old := orders[req.ResNum]; old != nil {
			o.Calls = old.Calls
		}
		o.Count("token")
		orders[o.ID] = o
	})
	core.WriteJSON(w, http.StatusOK, map[string]any{"status": 1, "token": tok})
}

func verdict(success bool, code int, description string, detail any) map[string]any {
	a := map[string]any{"Success": success, "ResultCode": code, "ResultDescription": description}
	if detail != nil {
		a["TransactionDetail"] = detail
	}
	return a
}

// settle is VerifyTransaction and ReverseTransaction, which take one body and
// answer one shape. The first verify of a paid RefNum answers ResultCode 0 and
// a later one ResultCode 2 with the same detail.
func (v *Vendor) settle(w http.ResponseWriter, r *http.Request, op string) {
	b := v.c.Behaviour()
	b.Wait()
	if forced := b.Force[op]; forced != "" {
		if forcedStatus(w, forced) {
			return
		}
		if code, err := strconv.Atoi(forced); err == nil {
			core.WriteJSON(w, http.StatusOK, verdict(false, code, "forced by the fake gateway control API", nil))
			return
		}
	}
	var req struct {
		RefNum         string `json:"RefNum"`
		TerminalNumber int64  `json:"TerminalNumber"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, core.MaxBody)).Decode(&req); err != nil || req.RefNum == "" {
		core.WriteJSON(w, http.StatusBadRequest, verdict(false, -1, "RefNum is missing", nil))
		return
	}
	if strconv.FormatInt(req.TerminalNumber, 10) != strings.TrimLeft(v.terminal, "0") {
		core.WriteJSON(w, http.StatusOK, verdict(false, -105, "terminal not found", nil))
		return
	}

	answer := verdict(false, -2, "transaction not found", nil)
	v.c.Do(func(orders map[string]*core.Order) {
		for _, o := range orders {
			bk := o.Extra.(bank)
			if bk.RefNum != req.RefNum {
				continue
			}
			o.Count(op)
			if o.State != core.Settled && o.State != core.Reversed {
				return
			}
			if o.State == core.Reversed {
				answer = verdict(false, 5, "transaction reversed", nil)
				return
			}
			code := 0
			if op == "reverse" {
				o.State, o.UpdatedAt = core.Reversed, time.Now()
			} else if bk.Verified++; bk.Verified > 1 {
				code = 2
			}
			o.Extra = bk
			answer = verdict(true, code, "ok", map[string]any{
				"RefNum": bk.RefNum, "OrginalAmount": o.Amount, "AffectiveAmount": o.Amount, "TerminalNumber": req.TerminalNumber,
			})
			return
		}
	})
	core.WriteJSON(w, http.StatusOK, answer)
}

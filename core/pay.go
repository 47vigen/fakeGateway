package core

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed pay.html
var pages embed.FS

var payTemplate = template.Must(template.New("pay.html").
	Funcs(template.FuncMap{"irr": FormatIRR}).
	ParseFS(pages, "pay.html"))

// returnPage is how a provider that posts back sends the shopper on: a form
// the browser posts to the callback itself.
var returnPage = template.Must(template.New("return").Parse(`<!doctype html><meta charset="utf-8"><title>بازگشت به فروشگاه</title>
<body onload="document.forms[0].submit()">
<form method="post" action="{{.URL}}">
{{range $k, $v := .Fields}}<input type="hidden" name="{{$k}}" value="{{$v}}">
{{end}}<noscript><button>بازگشت به فروشگاه</button></noscript>
</form>
</body>`))

type payView struct {
	Vendor string
	Order  Order
	Action string
	Hidden map[string]string
}

// PayPage renders the payment page for order id. Its two buttons post
// outcome=pay|cancel, plus hidden, to action ("" posts back to the same URL);
// the vendor routes that to PaySubmit.
func (c *Core) PayPage(w http.ResponseWriter, id, action string, hidden map[string]string) {
	o, ok := c.Get(id)
	if !ok {
		http.Error(w, "unknown order", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := payTemplate.Execute(w, payView{Vendor: c.vendor.Name(), Order: o, Action: action, Hidden: hidden}); err != nil {
		slog.Error("render pay page", "id", id, "err", err)
	}
}

// PaySubmit is the shopper's choice on the payment page: the order settles or
// fails and the browser goes on to the merchant the way the vendor sends it.
func (c *Core) PaySubmit(w http.ResponseWriter, r *http.Request, id string) {
	outcome := r.FormValue("outcome")
	st := Failed
	if outcome == "pay" || outcome == "settle" {
		st = Settled
	}
	o, ok := c.Update(id, func(o *Order) {
		o.State, o.UpdatedAt = st, time.Now()
		if o.CallbackURL != "" {
			o.Count("callback")
		}
	})
	if !ok {
		http.Error(w, "unknown order", http.StatusNotFound)
		return
	}
	slog.Info("pay page submitted", "id", id, "outcome", outcome, "state", o.State)

	if o.CallbackURL == "" {
		fmt.Fprintf(w, "order %s is now %s; no callback_url was given at creation", o.ID, o.State)
		return
	}
	cb := c.vendor.Callback(o)
	if cb.Post {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = returnPage.Execute(w, cb)
		return
	}
	target, err := withQuery(cb.URL, cb.Fields)
	if err != nil {
		http.Error(w, "stored callback_url is not a URL: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// FormatIRR groups whole Rial in thousands.
func FormatIRR(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	return sign + strings.Join(append([]string{s}, out...), ",")
}

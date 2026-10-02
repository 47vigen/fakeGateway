package core

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// control is the test-driving surface. Nothing under /api/v1/test exists at a
// real provider, and it takes no key, because a test should reach it in one
// call. Anyone who reaches it can settle or fail any order it holds.
func (c *Core) control(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/test/orders", c.listOrders)
	mux.HandleFunc("GET /api/v1/test/orders/{id}", c.getOrder)
	mux.HandleFunc("POST /api/v1/test/orders/{id}/settle", func(w http.ResponseWriter, r *http.Request) { c.move(w, r, Settled) })
	mux.HandleFunc("POST /api/v1/test/orders/{id}/fail", func(w http.ResponseWriter, r *http.Request) { c.move(w, r, Failed) })
	mux.HandleFunc("POST /api/v1/test/orders/{id}/callback", c.callback)
	mux.HandleFunc("POST /api/v1/test/reset", c.reset)
	mux.HandleFunc("GET /api/v1/test/config", c.getConfig)
	mux.HandleFunc("POST /api/v1/test/config", c.setConfig)
}

func unknownOrder(w http.ResponseWriter) {
	WriteJSON(w, http.StatusNotFound, map[string]any{"error": "unknown order"})
}

func (c *Core) listOrders(w http.ResponseWriter, _ *http.Request) {
	orders := c.List()
	views := make([]any, len(orders))
	for i, o := range orders {
		views[i] = c.vendor.View(o)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"count": len(views), "orders": views})
}

func (c *Core) getOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := c.Get(r.PathValue("id"))
	if !ok {
		unknownOrder(w)
		return
	}
	WriteJSON(w, http.StatusOK, c.vendor.View(o))
}

// move settles or fails an order. Firing the callback is a separate act, opted
// into with ?callback=1: the state is what verify reports, the callback is a
// network call to the merchant, and a test asserting verify semantics should
// not need the merchant to be up.
func (c *Core) move(w http.ResponseWriter, r *http.Request, st State) {
	o, ok := c.Update(r.PathValue("id"), func(o *Order) { o.State, o.UpdatedAt = st, time.Now() })
	if !ok {
		unknownOrder(w)
		return
	}
	slog.Info("order moved by control API", "id", o.ID, "state", o.State)

	body := map[string]any{"order": c.vendor.View(o)}
	if r.URL.Query().Get("callback") == "1" {
		o, body["callback"] = c.fire(o.ID)
		body["order"] = c.vendor.View(o)
	}
	WriteJSON(w, http.StatusOK, body)
}

// callback is the browser coming back without anything having happened at
// the provider: the state is left alone.
func (c *Core) callback(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.Get(r.PathValue("id")); !ok {
		unknownOrder(w)
		return
	}
	o, res := c.fire(r.PathValue("id"))
	WriteJSON(w, http.StatusOK, map[string]any{"order": c.vendor.View(o), "callback": res})
}

func (c *Core) reset(w http.ResponseWriter, _ *http.Request) {
	n := c.Reset()
	slog.Info("store reset by control API", "cleared", n)
	WriteJSON(w, http.StatusOK, map[string]any{"cleared": n})
}

func (c *Core) getConfig(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, c.Behaviour())
}

func (c *Core) setConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
	if err == nil {
		var b Behaviour
		b, err = c.Behaviour().merge(body, c.defaults)
		if err == nil {
			c.SetBehaviour(b)
			slog.Info("behaviour changed by control API", "behaviour", b)
			WriteJSON(w, http.StatusOK, b)
			return
		}
	}
	WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
}

// CallbackResult is what the merchant answered, unfollowed: its status and
// Location are usually the thing a test wants to assert.
type CallbackResult struct {
	URL      string            `json:"url,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	Status   int               `json:"status,omitempty"`
	Location string            `json:"location,omitempty"`
	Dropped  bool              `json:"dropped,omitempty"`
	Error    string            `json:"error,omitempty"`
}

var callbackClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// fire calls the merchant back in the order's current state, the way the
// vendor's browser return would arrive.
func (c *Core) fire(id string) (Order, CallbackResult) {
	if c.Behaviour().DropCallback {
		o, _ := c.Get(id)
		return o, CallbackResult{Dropped: true}
	}
	o, _ := c.Update(id, func(o *Order) {
		if o.CallbackURL != "" {
			o.Count("callback")
		}
	})
	if o.CallbackURL == "" {
		return o, CallbackResult{Error: "order has no callback_url"}
	}
	cb := c.vendor.Callback(o)

	var req *http.Request
	var err error
	res := CallbackResult{URL: cb.URL}
	if cb.Post {
		res.Fields = cb.Fields
		req, err = http.NewRequest(http.MethodPost, cb.URL, strings.NewReader(values(cb.Fields).Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else if res.URL, err = withQuery(cb.URL, cb.Fields); err == nil {
		req, err = http.NewRequest(http.MethodGet, res.URL, nil)
	}
	if err != nil {
		res.Error = "stored callback_url is not a URL: " + err.Error()
		return o, res
	}

	resp, err := callbackClient.Do(req)
	if err != nil {
		res.Error = err.Error()
		slog.Error("callback failed", "id", o.ID, "url", res.URL, "err", err)
		return o, res
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBody))

	res.Status = resp.StatusCode
	res.Location = resp.Header.Get("Location")
	slog.Info("callback fired", "id", o.ID, "url", res.URL, "status", res.Status, "location", res.Location)
	return o, res
}

func values(fields map[string]string) url.Values {
	v := url.Values{}
	for k, f := range fields {
		v.Set(k, f)
	}
	return v
}

// withQuery sets fields in raw's query, keeping what is already there.
func withQuery(raw string, fields map[string]string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range fields {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

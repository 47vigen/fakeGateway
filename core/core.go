// Package core is what every fake vendor shares: the in-memory order store,
// the behaviour knobs, the control API under /api/v1/test/, the payment page
// and the callback. A vendor package adds only its own wire contract.
package core

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MaxBody caps every request body this service reads.
const MaxBody = 1 << 20

// Vendor is one provider this process can play.
type Vendor interface {
	// Name is the value of GATEWAY that selects this vendor.
	Name() string
	// Routes registers the provider's own paths. It runs once, before any
	// order exists, so a vendor may keep c for later.
	Routes(mux *http.ServeMux, c *Core)
	// Callback is what the provider sends the merchant for o in its current
	// state.
	Callback(o Order) Callback
	// View is o as the control API shows it.
	View(o Order) any
}

// Callback is the provider's return to the merchant. Post sends Fields as a
// form the browser posts itself; otherwise they ride in the query of a GET.
type Callback struct {
	URL    string
	Fields map[string]string
	Post   bool
}

// State is an order's state. Settled and Failed are the two the control API
// and the payment page move an order to; a vendor may use others.
type State string

const (
	Pending  State = "pending"
	Settled  State = "settled"
	Failed   State = "failed"
	Reversed State = "reversed"
)

// Item is one line the payment page lists.
type Item struct {
	Name     string
	Quantity int64
	Price    int64
	Total    int64
}

// Order is one payment as the provider holds it.
type Order struct {
	// ID is the key the control API and the payment page address it by.
	ID          string
	Amount      int64
	State       State
	CallbackURL string
	Items       []Item
	// Calls counts each provider operation for this order: the ledger a test
	// reads instead of trusting the merchant's own account.
	Calls map[string]int
	// Request is the create request as it arrived.
	Request   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
	// Extra is the vendor's own state. Keep it a value type: Get hands out
	// copies.
	Extra any
}

// Count adds one call of op to the ledger.
func (o *Order) Count(op string) {
	if o.Calls == nil {
		o.Calls = map[string]int{}
	}
	o.Calls[op]++
}

// Core holds the orders and the behaviour behind one lock.
// ponytail: one global lock, one map. A test double serves a handful of orders.
type Core struct {
	PublicBaseURL string

	vendor Vendor
	mu     sync.Mutex
	orders map[string]*Order
	// bmu guards b apart from mu, so a vendor may read its knobs inside Do.
	bmu      sync.Mutex
	b        Behaviour
	defaults Behaviour
}

// Handler assembles one vendor's service: its provider routes, the control
// API, /healthz, and request logging.
func Handler(v Vendor, publicBaseURL string, defaults Behaviour) (http.Handler, *Core) {
	c := &Core{
		PublicBaseURL: publicBaseURL,
		vendor:        v,
		orders:        map[string]*Order{},
		b:             defaults,
		defaults:      defaults,
	}
	mux := http.NewServeMux()
	v.Routes(mux, c)
	c.control(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	return LogRequests(mux), c
}

// Do runs fn with the store locked, for a vendor operation that must look and
// change in one step.
func (c *Core) Do(fn func(orders map[string]*Order)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.orders)
}

// Create stores o. It reports false when o.ID is taken: a test that reuses an
// id should hear about it rather than silently overwrite the first order.
func (c *Core) Create(o Order) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.orders[o.ID]; taken {
		return false
	}
	c.orders[o.ID] = &o
	return true
}

func (c *Core) Get(id string) (Order, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Update changes one order under the lock and returns it as it now is.
// UpdatedAt is the caller's to move: a counted call is not a change of state.
func (c *Core) Update(id string, fn func(*Order)) (Order, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.orders[id]
	if !ok {
		return Order{}, false
	}
	fn(o)
	return *o, true
}

// List returns every order, oldest first.
func (c *Core) List() []Order {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Order, 0, len(c.orders))
	for _, o := range c.orders {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (c *Core) Reset() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.orders)
	c.orders = map[string]*Order{}
	return n
}

func (c *Core) Behaviour() Behaviour {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	return c.b
}

func (c *Core) SetBehaviour(b Behaviour) {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	c.b = b
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *logWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *logWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// LogRequests writes one line per request. A failing end-to-end run is read
// through these, so they carry what arrived and what was answered.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logWriter{ResponseWriter: w}
		next.ServeHTTP(lw, r)
		if lw.status == 0 {
			lw.status = http.StatusOK
		}
		slog.Info("request",
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", lw.status,
			"bytes", lw.bytes,
			"ms", time.Since(start).Milliseconds(),
			"location", lw.Header().Get("Location"),
		)
	})
}

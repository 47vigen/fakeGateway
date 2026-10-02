package core

import (
	"encoding/json"
	"maps"
	"strconv"
	"time"
)

// Behaviour is the knob set an end-to-end test turns.
type Behaviour struct {
	// DefaultOutcome is manual, settle or fail. Anything but manual puts a new
	// order straight into that state. It never fires the callback: at creation
	// the merchant has not yet answered the shopper with the redirect it is
	// about to issue, so a callback there would race it.
	DefaultOutcome string `json:"default_outcome"`
	// EnforceAmounts makes a vendor reject a create whose total does not add
	// up, where its contract has such a rule.
	EnforceAmounts bool `json:"enforce_amounts"`
	// DelayMS delays every provider operation, which is how a client timeout
	// is provoked without breaking anything.
	DelayMS int `json:"delay_ms"`
	// DropCallback keeps the money moving while the control API's callback
	// never arrives — the case reconciliation exists for.
	DropCallback bool `json:"drop_callback"`
	// Force maps a provider operation to the code or HTTP status it answers
	// regardless of the order. Absent means "decide from the order".
	Force map[string]string `json:"force"`
}

// The DefaultOutcome values.
const (
	OutcomeManual = "manual"
	OutcomeSettle = "settle"
	OutcomeFail   = "fail"
)

// Initial is the state a new order starts in: Settled or Failed when the
// default outcome says so, otherwise pending (each vendor names its own).
func (b Behaviour) Initial(pending State) State {
	switch b.DefaultOutcome {
	case OutcomeSettle:
		return Settled
	case OutcomeFail:
		return Failed
	}
	return pending
}

// Wait sleeps DelayMS.
func (b Behaviour) Wait() {
	if b.DelayMS > 0 {
		time.Sleep(time.Duration(b.DelayMS) * time.Millisecond)
	}
}

// legacyForce maps the per-vendor knob names the two original fakes took to
// the operation they force. existing test suites still post
// them.
var legacyForce = map[string]string{
	"pay_code":      "pay",
	"verify_code":   "verify",
	"settle_code":   "settle",
	"create_status": "create",
	"verify_status": "verify",
}

// merge applies a posted config onto b. The fields posted change and the rest
// stay, so a test can turn one knob without restating the others; an empty
// object restores the startup defaults. A forced value of "" or 0 clears it.
func (b Behaviour) merge(body []byte, defaults Behaviour) (Behaviour, error) {
	var fields map[string]json.RawMessage
	if len(body) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil {
			return b, err
		}
	}
	if len(fields) == 0 {
		return defaults, nil
	}
	b.Force = maps.Clone(b.Force)
	if err := json.Unmarshal(body, &b); err != nil {
		return b, err
	}
	if b.Force == nil {
		b.Force = map[string]string{}
	}
	for name, op := range legacyForce {
		if raw, ok := fields[name]; ok {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return b, err
			}
			switch v := v.(type) {
			case string:
				b.Force[op] = v
			case float64:
				b.Force[op] = strconv.FormatFloat(v, 'f', -1, 64)
			default:
				b.Force[op] = ""
			}
		}
	}
	for op, v := range b.Force {
		if v == "" || v == "0" {
			delete(b.Force, op)
		}
	}
	return b, nil
}

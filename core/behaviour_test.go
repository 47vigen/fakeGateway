package core

import (
	"maps"
	"testing"
)

func TestMerge(t *testing.T) {
	defaults := Behaviour{DefaultOutcome: OutcomeManual, EnforceAmounts: true}
	b := defaults
	for _, step := range []struct {
		body  string
		force map[string]string
		delay int
	}{
		{`{"verify_code":"34","delay_ms":50}`, map[string]string{"verify": "34"}, 50},
		{`{"create_status":500}`, map[string]string{"verify": "34", "create": "500"}, 50},
		{`{"force":{"refund":"9"},"verify_code":""}`, map[string]string{"create": "500", "refund": "9"}, 50},
		{`{"create_status":0,"force":{"refund":""}}`, map[string]string{}, 50},
		{`{}`, nil, 0},
	} {
		var err error
		b, err = b.merge([]byte(step.body), defaults)
		if err != nil {
			t.Fatalf("%s: %v", step.body, err)
		}
		if !maps.Equal(b.Force, step.force) || b.DelayMS != step.delay || !b.EnforceAmounts {
			t.Fatalf("after %s: %+v", step.body, b)
		}
	}
}

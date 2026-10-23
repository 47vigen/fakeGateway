package main

import (
	"strings"
	"testing"
)

func TestPick(t *testing.T) {
	for _, name := range []string{"", "nope"} {
		_, err := pick(name)
		if err == nil || !strings.Contains(err.Error(), "known vendors: behpardakht, digipay, gsmpay, kamanlend, moneysa, sep, setareyek, vibe, zarinplus") {
			t.Errorf("pick(%q) = %v, want an error listing the vendors", name, err)
		}
	}
	if v, err := pick("vibe"); err != nil || v.Name() != "vibe" {
		t.Errorf("pick(vibe) = %v, %v", v, err)
	}
}

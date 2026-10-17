package main

// snclaim_cmd_test.go — the `provider snclaim` dispatch. Everything the
// docopt usage would reject is handed to the on-chain tool before the parse,
// so only an argument list whose first word is exactly `snclaim` qualifies.

import (
	"strings"
	"testing"
)

func TestIsSnclaimInvocation(t *testing.T) {
	for args, want := range map[string]bool{
		"provider":                 false,
		"provider snclaim":         true,
		"provider snclaim status":  true,
		"provider claim":           false,
		"provider provide snclaim": false,
		"provider  snclaimx":       false,
	} {
		if got := isSnclaimInvocation(strings.Fields(args)); got != want {
			t.Errorf("isSnclaimInvocation(%q) = %v, want %v", args, got, want)
		}
	}
}

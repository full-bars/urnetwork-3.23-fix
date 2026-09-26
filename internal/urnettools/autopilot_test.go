package urnettools

import (
	"strings"
	"testing"
)

func TestParseAutopilotArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		limit   int
		wantErr string
	}{
		{"log with the default limit", []string{"log"}, 20, ""},
		{"log with a limit", []string{"log", "5"}, 5, ""},
		{"a huge limit is clamped, not rejected", []string{"log", "9999"}, 200, ""},
		{"no subcommand", nil, 0, "usage"},
		{"unknown subcommand", []string{"nope"}, 0, "unknown subcommand"},
		{"zero limit", []string{"log", "0"}, 0, "positive integer"},
		{"negative limit", []string{"log", "-3"}, 0, "positive integer"},
		{"junk limit", []string{"log", "lots"}, 0, "positive integer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limit, _, err := parseAutopilotArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil || limit != c.limit {
				t.Fatalf("limit = %d, err = %v; want %d", limit, err, c.limit)
			}
		})
	}
}

// The ledger view must read as a timeline an operator can scan: UTC time (so
// it does not depend on the machine's zone), actor, action, the change, the
// mode, and why.
func TestRenderLedger(t *testing.T) {
	raw := `[
	 {"t":"2026-09-26T01:00:00Z","actor":"oomcap","action":"reduce","from":0,"to":3301,"mode":"shadow","reason":"OOM kill since the last start (peak running 4127)"},
	 {"t":"2026-09-26T01:05:30Z","actor":"trim","action":"applied","from":4127,"to":2000,"mode":"operator","reason":"shed 2127 worst-graded running, held 0 additions"},
	 {"t":"2026-09-26T02:00:00Z","actor":"trim","action":"cleared","from":2000,"to":0,"mode":"operator"}
	]`
	got, err := renderLedger(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-09-26 01:00:00Z  oomcap  reduce   0 -> 3301  [shadow]    OOM kill since the last start (peak running 4127)",
		"2026-09-26 01:05:30Z  trim    applied  4127 -> 2000  [operator]  shed 2127 worst-graded running, held 0 additions",
		"2026-09-26 02:00:00Z  trim    cleared  2000 -> 0  [operator]",
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), got)
	}
	for i := range want {
		// Columns are padded for alignment; compare with runs of spaces collapsed
		// so the test pins the content and order, not the exact padding.
		if collapse(lines[i]) != collapse(want[i]) {
			t.Errorf("line %d:\n got  %q\n want %q", i, collapse(lines[i]), collapse(want[i]))
		}
	}
}

func TestRenderLedgerEdgeCases(t *testing.T) {
	if got, err := renderLedger("[]"); err != nil || !strings.Contains(got, "No autopilot decisions recorded yet") {
		t.Fatalf("empty ledger: %q %v", got, err)
	}
	if _, err := renderLedger("{not json"); err == nil {
		t.Fatalf("a malformed reply must be an error, not a silent empty view")
	}
	// An unparseable timestamp is shown as-is rather than dropping the entry.
	got, err := renderLedger(`[{"t":"yesterday","actor":"trim","action":"applied","from":1,"to":2}]`)
	if err != nil || !strings.Contains(got, "yesterday") {
		t.Fatalf("bad timestamp: %q %v", got, err)
	}
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestAutopilotCommandIsRegistered(t *testing.T) {
	root := buildRootCmd()
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "autopilot" {
			found = true
		}
	}
	if !found {
		t.Fatal("urnet-tools has no autopilot command")
	}
}

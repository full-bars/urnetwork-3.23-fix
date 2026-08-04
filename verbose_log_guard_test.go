package connect

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests are a Go-side mirror of the ".github/workflows/build.yml"
// "Check hot-path verbose logging guards" step (Task 17): every unguarded
// `V(n).Inf*` call in the Tier-1 hot-path files (ip.go, transfer.go,
// ip_remote_multi_client.go) must carry a format string that is present in
// the checked-in allowlist (.github/verbose_log_allowlist.txt). Mirroring
// the check here means `go test` catches a regression locally, not just in
// CI.

// verboseLogCallRe finds unguarded `<expr>.V(<digit>).Inf<word>(` call sites,
// same as the CI script. A line that has been wrapped in the
// `if v := log.V(n); v.Enabled() { v.Infof(...) }` guard no longer chains
// `.Infof(` directly off `.V(n)`, so it does not match.
// [^\n/]* (rather than [^/]*) keeps the match on one line: the bare form
// spanned newlines, misattributing sites and silently skipping some.
// \d+ (rather than \d) so multi-digit levels such as V(10) are detected.
var verboseLogCallRe = regexp.MustCompile(`(?m)^[^\n/]*\.V\(\d+\)\.Inf\w*\(`)

// verboseLogFormatRe matches the call's immediate first argument when it is a
// string literal. Leading whitespace is allowed (the literal may sit on the
// next line of a multiline call) but nothing else: a dynamic first argument
// must not be able to hide behind a sanctioned literal later in the call.
// The string itself is captured in group 1 (whitespace is skipped, not kept).
var verboseLogFormatRe = regexp.MustCompile(`^\s*("(?:[^"\\]|\\.)*")`)

type verboseLogViolation struct {
	line   int
	format string
}

func (v verboseLogViolation) String() string {
	return fmt.Sprintf("line %d: %s", v.line, v.format)
}

// findUnguardedVerboseLogViolations scans src for unguarded V(n).Inf* calls
// whose format string is not in sanctioned. It mirrors the Python check
// embedded in .github/workflows/build.yml.
func findUnguardedVerboseLogViolations(src string, sanctioned map[string]bool) []verboseLogViolation {
	var violations []verboseLogViolation
	for _, loc := range verboseLogCallRe.FindAllStringIndex(src, -1) {
		rest := src[loc[1]:]
		format := "<no format string>"
		if m := verboseLogFormatRe.FindStringSubmatch(rest); m != nil {
			format = m[1]
		}
		if !sanctioned[format] {
			lineNo := strings.Count(src[:loc[0]], "\n") + 1
			violations = append(violations, verboseLogViolation{line: lineNo, format: format})
		}
	}
	return violations
}

func loadVerboseLogAllowlist(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(".github/verbose_log_allowlist.txt")
	if err != nil {
		t.Fatalf("failed to read allowlist: %s", err)
	}
	sanctioned := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sanctioned[line] = true
	}
	return sanctioned
}

// TestVerboseLogAllowlist_EntriesAreWellFormed validates the structure of
// the checked-in allowlist itself: every non-comment, non-blank line must be
// a properly quoted Go string literal, and there must be no duplicates.
func TestVerboseLogAllowlist_EntriesAreWellFormed(t *testing.T) {
	data, err := os.ReadFile(".github/verbose_log_allowlist.txt")
	if err != nil {
		t.Fatalf("failed to read allowlist: %s", err)
	}

	quoted := regexp.MustCompile(`^"(?:[^"\\]|\\.)*"$`)
	seen := map[string]int{}
	entryCount := 0

	for i, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entryCount++
		if !quoted.MatchString(line) {
			t.Errorf("line %d is not a properly quoted Go string literal: %q", i+1, line)
		}
		seen[line]++
	}

	for entry, count := range seen {
		if count > 1 {
			t.Errorf("duplicate allowlist entry %q appears %d times", entry, count)
		}
	}

	if entryCount == 0 {
		t.Fatal("allowlist should contain at least one sanctioned format string")
	}
}

// TestFindUnguardedVerboseLogViolations_SyntheticCases unit-tests the
// checker logic in isolation, independent of the current state of the
// Tier-1 files, so a bug in the checker itself (e.g. a regex that never
// matches, silently passing everything) would be caught here.
func TestFindUnguardedVerboseLogViolations_SyntheticCases(t *testing.T) {
	sanctioned := map[string]bool{
		`"[ok]allowlisted %s\n"`: true,
	}

	tests := map[string]struct {
		src      string
		expected []verboseLogViolation
	}{
		"guarded call is ignored even with a non-allowlisted format": {
			src: "if v := self.log.V(1); v.Enabled() {\n" +
				"\tv.Infof(\"[bad]new hot path %s\\n\", x)\n" +
				"}\n",
			expected: nil,
		},
		"unguarded call with an allowlisted format is not a violation": {
			src:      `self.log.V(1).Infof("[ok]allowlisted %s\n", x)` + "\n",
			expected: nil,
		},
		"unguarded call with a non-allowlisted format is a violation": {
			src:      `self.log.V(1).Infof("[bad]new hot path %s\n", x)` + "\n",
			expected: []verboseLogViolation{{line: 1, format: `"[bad]new hot path %s\n"`}},
		},
		"commented-out call is ignored": {
			src:      `// self.log.V(1).Infof("[bad]new hot path %s\n", x)` + "\n",
			expected: nil,
		},
		"unguarded call with no format string literal at all": {
			src:      `self.log.V(1).Infof(dynamicFormat)`,
			expected: []verboseLogViolation{{line: 1, format: "<no format string>"}},
		},
		"dynamic format before a sanctioned literal is still a violation": {
			src:      `self.log.V(1).Infof(dynamicFormat, "[ok]allowlisted %s\n")` + "\n",
			expected: []verboseLogViolation{{line: 1, format: "<no format string>"}},
		},
		"multi-digit verbosity level is caught": {
			src:      `self.log.V(10).Infof("[bad]new hot path %s\n", x)` + "\n",
			expected: []verboseLogViolation{{line: 1, format: `"[bad]new hot path %s\n"`}},
		},
		"violation line number tracks preceding newlines": {
			src: "package x\n\nfunc f() {\n" +
				`self.log.V(2).Infof("[bad]line three\n", x)` + "\n" +
				"}\n",
			expected: []verboseLogViolation{{line: 4, format: `"[bad]line three\n"`}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := findUnguardedVerboseLogViolations(tc.src, sanctioned)
			if len(got) != len(tc.expected) {
				t.Fatalf("expected %d violation(s), got %d: %v", len(tc.expected), len(got), got)
			}
			for i, v := range got {
				if v != tc.expected[i] {
					t.Fatalf("violation %d: expected %v, got %v", i, tc.expected[i], v)
				}
			}
		})
	}
}

// TestHotPathVerboseLogging_UnguardedCallsAreAllowlisted is the direct
// regression counterpart to the CI "Check hot-path verbose logging guards"
// step: every unguarded V(n).Inf* call in the Tier-1 files must carry an
// allowlisted format string.
func TestHotPathVerboseLogging_UnguardedCallsAreAllowlisted(t *testing.T) {
	sanctioned := loadVerboseLogAllowlist(t)

	files := []string{"ip.go", "transfer.go", "ip_remote_multi_client.go"}
	totalCallsSeen := 0
	var bad []string

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read %s: %s", f, err)
		}
		text := string(src)
		totalCallsSeen += len(verboseLogCallRe.FindAllStringIndex(text, -1))
		for _, v := range findUnguardedVerboseLogViolations(text, sanctioned) {
			bad = append(bad, fmt.Sprintf("%s:%d %s", f, v.line, v.format))
		}
	}

	// Sanity check that the regex is actually matching call sites in these
	// files; otherwise this test would vacuously pass.
	if totalCallsSeen == 0 {
		t.Fatal("expected to find at least one unguarded V(n).Inf* call site across the Tier-1 files; the checker regex may be broken")
	}

	if len(bad) > 0 {
		t.Fatalf(
			"unguarded V(n).Inf* with a non-allowlisted format string (wrap with `if v := log.V(n); v.Enabled()` or extend the allowlist via a Tier-2 sweep):\n%s",
			strings.Join(bad, "\n"),
		)
	}
}

// TestHotPathVerboseLogging_KnownSanctionedSitesRemainUnguarded is a
// positive control: it confirms that the deliberately-unguarded Tier-2 sites
// this PR left untouched (from the "sanctioned" list) are still present and
// unguarded in source, so the passing state of the check above isn't due to
// the sites having disappeared or the regex failing to match them.
func TestHotPathVerboseLogging_KnownSanctionedSitesRemainUnguarded(t *testing.T) {
	cases := []struct {
		file   string
		needle string
	}{
		{"transfer.go", `self.log.V(2).Infof("[f]drop = %s", err)`},
		{"transfer.go", `self.log.V(2).Infof("[f]exit idle timeout %s->%s s(%s)", self.clientTag, self.destination.DestinationId, self.destination.StreamId)`},
	}

	for _, c := range cases {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("failed to read %s: %s", c.file, err)
		}
		if !strings.Contains(string(src), c.needle) {
			t.Errorf("%s: expected sanctioned unguarded call still present: %s", c.file, c.needle)
		}
	}
}

// TestHotPathVerboseLogging_ModifiedSitesAreNowGuarded spot-checks a
// representative sample of the call sites this PR actually guarded, so a
// future edit that accidentally un-guards one of them (reverting to a
// direct `.V(n).Infof(` chain) is caught even though it might still carry an
// allowlisted format string. Whitespace between tokens is matched loosely
// so the test isn't brittle to reformatting.
func TestHotPathVerboseLogging_ModifiedSitesAreNowGuarded(t *testing.T) {
	cases := []struct {
		file   string
		level  int
		format string
	}{
		{"ip.go", 2, `"[f%d]udp forward %d\n"`},
		{"ip.go", 1, `"[f%d]timeout\n"`},
		{"transfer.go", 1, `"[cr] %s %s<-%s s(%s)\n"`},
		{"ip_remote_multi_client.go", 1, `"[multi]drop packet ipv%d p%v -> %s:%d\n"`},
	}

	for _, c := range cases {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("failed to read %s: %s", c.file, err)
		}

		guardRe := regexp.MustCompile(
			fmt.Sprintf(`if v := self\.log\.V\(%d\); v\.Enabled\(\) \{\s*v\.Infof\(\s*%s`, c.level, regexp.QuoteMeta(c.format)),
		)
		if !guardRe.MatchString(string(src)) {
			t.Errorf("%s: expected format %s to be reached through an `if v := self.log.V(%d); v.Enabled()` guard", c.file, c.format, c.level)
		}
	}
}

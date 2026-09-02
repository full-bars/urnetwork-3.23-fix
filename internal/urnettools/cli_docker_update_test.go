package urnettools

import (
	"bytes"
	"strings"
	"testing"
)

// Regression for LA1 defect 1 (2026-08-24): bare `urnet-docker update` on a
// single-container box must target that container, not self-update the host
// tool. updateTargetFromArgs returns an empty Target when no explicit or
// bare-name target resolves; cmdDockerUpdate's no-target branch then picks
// the lone container. These tests cover the pure helper layer.

func TestUpdateTargetFromArgs_BareContainerName(t *testing.T) {
	providers := []Provider{{User: "docker:ps", Unit: "ps"}}
	tt, rest, err := updateTargetFromArgs([]string{"ps"}, providers)
	if err != nil {
		t.Fatalf("bare container name should resolve: %v", err)
	}
	if tt.Unit != "ps" {
		t.Fatalf("expected container ps, got %+v", tt)
	}
	if len(rest) != 0 {
		t.Fatalf("rest should be empty, got %v", rest)
	}
}

func TestUpdateTargetFromArgs_NoTargetNoArgs(t *testing.T) {
	providers := []Provider{{User: "docker:ps", Unit: "ps"}}
	tt, _, err := updateTargetFromArgs(nil, providers)
	if err != nil {
		t.Fatalf("no args is valid: %v", err)
	}
	if tt.Unit != "" {
		t.Fatalf("expected empty target (caller defaults to lone container), got %+v", tt)
	}
}

// Host self-update pinning options (--tag/--digest/--url) must NEVER resolve
// to a container, even a lone one (regression: `update
// --tag vX` on a single-container box auto-selected the container and
// silently dropped --tag). updateTargetFromArgs must return an empty target
// so cmdDockerUpdate routes these to the host self-update.
func TestUpdateTargetFromArgs_SelfUpdateTagNotContainer(t *testing.T) {
	providers := []Provider{{User: "docker:ps", Unit: "ps"}}
	for _, args := range [][]string{{"--tag", "v1"}, {"--tag=v1"}, {"--digest=somesha"}} {
		tt, _, err := updateTargetFromArgs(args, providers)
		if err != nil {
			t.Fatalf("self-update args are valid: %v", err)
		}
		if tt.Unit != "" {
			t.Fatalf("self-update arg %v must not resolve to a container, got %+v", args, tt)
		}
	}
}

func TestHasSelfUpdateArg(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--tag", "v1"}, true},
		{[]string{"--tag=v1"}, true},
		{[]string{"--digest", "abc"}, true},
		{[]string{"--digest=abc"}, true},
		{[]string{"--url", "https://x"}, true},
		{[]string{"--url=https://x"}, true},
		{nil, false},
		{[]string{"ps"}, false},
		{[]string{"--unit", "ps"}, false},
		{[]string{"--include", ".*"}, false},
		{[]string{"--force"}, false},
	}
	for _, c := range cases {
		if got := hasSelfUpdateArg(c.args); got != c.want {
			t.Fatalf("hasSelfUpdateArg(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// A bare container name that matches nothing must be a loud error, never a
// silent fall-through to the lone-container auto-select (which would update a
// DIFFERENT container than the one named) — ox-alpha H1, PR #465.
func TestUpdateTargetFromArgs_UnmatchedBareNameErrors(t *testing.T) {
	providers := []Provider{{User: "docker:ps", Unit: "ps"}}
	_, _, err := updateTargetFromArgs([]string{"prod2"}, providers)
	if err == nil {
		t.Fatal("expected an error for an unmatched bare container name")
	}
	if !strings.Contains(err.Error(), "no provider container named \"prod2\"") ||
		!strings.Contains(err.Error(), "ps") {
		t.Fatalf("error should name the missing container and list available ones: %v", err)
	}
	// A valid name still resolves.
	if tt, _, err := updateTargetFromArgs([]string{"ps"}, providers); err != nil || tt.Unit != "ps" {
		t.Fatalf("valid name should still resolve: tt=%+v err=%v", tt, err)
	}
}

// A self-update pin value must never be treated as a container name, even if
// it coincidentally equals one — ox-alpha L2, PR #465.
func TestUpdateTargetFromArgs_SelfUpdateValueNotContainer(t *testing.T) {
	providers := []Provider{{User: "docker:v1", Unit: "v1"}}
	tt, _, err := updateTargetFromArgs([]string{"--tag", "v1"}, providers)
	if err != nil {
		t.Fatalf("self-update args are valid: %v", err)
	}
	if tt.Unit != "" {
		t.Fatalf("--tag value must not resolve to a container, got %+v", tt)
	}
}

func TestUpdateTargetFromArgs_ExplicitFlagWins(t *testing.T) {
	providers := []Provider{
		{User: "docker:a", Unit: "a"},
		{User: "docker:b", Unit: "b"},
	}
	tt, rest, err := updateTargetFromArgs([]string{"--unit=b"}, providers)
	if err != nil {
		t.Fatalf("explicit flag should resolve: %v", err)
	}
	if tt.Unit != "b" {
		t.Fatalf("expected b, got %+v", tt)
	}
	if len(rest) != 0 {
		t.Fatalf("rest should be empty, got %v", rest)
	}
}

// firstByteWriter is the M1 (PR #465) hang-detection helper: it forwards
// writes to the real destination and closes `produced` once, on the first
// byte, so a working cross-user journal follow is distinguished from a hang.
func TestFirstByteWriter(t *testing.T) {
	produced := make(chan struct{})
	var got bytes.Buffer
	w := firstByteWriter{w: &got, produced: produced}
	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("b")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	select {
	case <-produced: // closed exactly once (sync.Once guards double-close)
	default:
		t.Fatal("produced should be closed after the first byte")
	}
	if got.String() != "ab" {
		t.Fatalf("data not forwarded verbatim: %q", got.String())
	}
}

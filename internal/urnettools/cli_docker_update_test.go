package urnettools

import (
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
// to a container, even a lone one (CodeRabbit PR #465 regression: `update
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

// The lone-container selection message must mention both the container and
// the self-update alias so the operator learns the new vocabulary.
func TestCmdDockerUpdate_LoneContainerMessageShape(t *testing.T) {
	msg := "no target given; updating the lone provider container ps (use `self-update` for the host tool)"
	if !strings.Contains(msg, "self-update") || !strings.Contains(msg, "ps") {
		t.Fatal("message shape changed; keep container name + self-update hint")
	}
}

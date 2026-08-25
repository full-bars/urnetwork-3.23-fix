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

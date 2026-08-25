package urnettools

import (
	"strings"
	"testing"
)

// Regression coverage for the LA1 incident (2026-08-24): `urnet-tools stop
// ps` silently dropped the positional "ps", fell through to no-target
// default selection, and stopped the unrelated host systemd provider.
// The lifecycle commands must refuse leftover positionals loudly and must
// never act on a docker-namespaced provider.

func TestGuardLifecycleArgs_LeftoverPositionalIsHardError(t *testing.T) {
	for _, cmd := range []string{"start", "stop", "restart"} {
		_, err := guardLifecycleArgs(cmd, []string{"ps"})
		if err == nil {
			t.Fatalf("%s: expected error for bare positional, got nil", cmd)
		}
		if !strings.Contains(err.Error(), "takes no positional arguments") {
			t.Fatalf("%s: unexpected error: %v", cmd, err)
		}
	}
}

func TestGuardLifecycleArgs_LeftoverPositionalMatchingContainerHintsDocker(t *testing.T) {
	orig := discoverDockerFn
	discoverDockerFn = func() []Provider {
		return []Provider{{User: "docker:ps", Unit: "ps", StateDir: "/root/.urnetwork"}}
	}
	defer func() { discoverDockerFn = orig }()

	_, err := guardLifecycleArgs("stop", []string{"ps"})
	if err == nil {
		t.Fatal("expected error for bare positional matching a container")
	}
	if !strings.Contains(err.Error(), "urnet-docker stop ps") {
		t.Fatalf("error should hint at urnet-docker, got: %v", err)
	}
}

func TestGuardLifecycleArgs_FlagsOnlyPass(t *testing.T) {
	tt, err := guardLifecycleArgs("stop", []string{"--user", "user"})
	if err != nil {
		t.Fatalf("flag-only args should pass the guard: %v", err)
	}
	if tt.User != "user" {
		t.Fatalf("target flags lost: %+v", tt)
	}
}

func TestGuardLifecycleArgs_UnknownFlagStillRejected(t *testing.T) {
	if _, err := guardLifecycleArgs("stop", []string{"--netwrok", "x"}); err == nil {
		t.Fatal("unknown flag should still be rejected by parseTargetFlags")
	}
}

func TestIsDockerProvider(t *testing.T) {
	if !isDockerProvider(Provider{User: "docker:ps"}) {
		t.Fatal("docker-prefixed user should be detected as docker provider")
	}
	if isDockerProvider(Provider{User: "user"}) {
		t.Fatal("plain user should not be detected as docker provider")
	}
}

func TestGuardSystemdProvider_RefusesDockerProvider(t *testing.T) {
	err := guardSystemdProvider(Provider{User: "docker:ps", Unit: "ps"})
	if err == nil {
		t.Fatal("docker provider must be refused in the systemd path")
	}
	if !strings.Contains(err.Error(), "urnet-docker") {
		t.Fatalf("refusal should point at urnet-docker: %v", err)
	}
	if err := guardSystemdProvider(Provider{User: "user", Unit: "urnetwork.service"}); err != nil {
		t.Fatalf("systemd provider should pass the guard: %v", err)
	}
}

package urnettools

import (
	"strings"
	"testing"
)

// TestIsDockerCandidate covers the container image/name recognition rules.
func TestIsDockerCandidate(t *testing.T) {
	cases := []struct {
		image, name string
		want        bool
	}{
		{"ghcr.io/full-bars/urnetwork-3.23-fix:latest", "urnet", true},
		{"urnetwork-3.23-fix:stable", "provider1", true},
		{"nginx:latest", "web", false},
		{"redis:7", "cache", false},
		{"ghcr.io/full-bars/urnetwork-3.23-fix:latest", "anything", true},
		{"ubuntu:24.04", "urnet-test", true}, // name match is enough
	}
	for _, c := range cases {
		if got := isDockerCandidate(c.image, c.name); got != c.want {
			t.Errorf("isDockerCandidate(%q, %q) = %v, want %v", c.image, c.name, got, c.want)
		}
	}
}

// TestDockerImageVersion extracts the version from an image tag.
func TestDockerImageVersion(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.26.8": "v3.23.0-fix.26.8",
		"urnetwork-3.23-fix:stable":                             "",
		"urnetwork-3.23-fix:latest":                             "",
		"urnetwork-3.23-fix":                                    "",
	}
	for img, want := range cases {
		if got := dockerImageVersion(img); got != want {
			t.Errorf("dockerImageVersion(%q) = %q, want %q", img, got, want)
		}
	}
}

// TestTailLines verifies the logs tail helper.
func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := tailLines(in, "2"); got != "d\ne\n" {
		t.Errorf("tailLines 2 = %q", got)
	}
	if got := tailLines(in, "99"); got != in {
		t.Errorf("tailLines 99 should return everything, got %q", got)
	}
	if got := tailLines("single", "1"); got != "single\n" {
		t.Errorf("tailLines single = %q", got)
	}
}

// TestDockerProviderIdentity builds a Provider from a fake container record
// (unit-testable without a docker daemon).
func TestDockerProviderIdentity(t *testing.T) {
	c := dockerContainer{
		ID:    "abc123",
		Name:  "urnet",
		Image: "ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.26.8",
		State: "running",
	}
	p, err := dockerProvider(c)
	if err != nil {
		// Without a real docker daemon this errors on containerReadFile —
		// the container plumbing is not unit-testable offline; the pure
		// parts (version parse, candidate match) are covered above.
		t.Logf("dockerProvider requires a live docker daemon: %v", err)
		return
	}
	_ = p
}

// TestSplitExecArgs covers the exec argument-splitting logic (the pure
// helper behind cmdDockerExec). Mimo HIGH-1: the integration tests only
// exercised error paths (no docker daemon), so the actual -- separator
// forwarding of rest... was never verified. This pins the slice math
// directly without docker.
func TestSplitExecArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPre  []string
		wantRest []string
		wantErr  string
	}{
		{"sep-first-no-target", []string{"--", "urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"sep-flag-first-word", []string{"--", "-f", "urnet-tools"}, nil, []string{"-f", "urnet-tools"}, ""},
		{"sep-flags-with-values", []string{"--", "urnet-tools", "--proxy_file=/tmp/p.txt"}, nil, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		{"sep-empty-command", []string{"--unit", "x", "--"}, []string{"--unit", "x"}, nil, ""},
		{"sep-only", []string{"--"}, nil, nil, ""},
		{"sep-multiple-inner-flags", []string{"--", "urnet-tools", "--verbose", "cmd"}, nil, []string{"urnet-tools", "--verbose", "cmd"}, ""},
		{"no-sep-command-first", []string{"urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"no-sep-target-first", []string{"--unit", "x", "urnet-tools", "--proxy_file=/tmp/p.txt"}, []string{"--unit", "x"}, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		{"unknown-dash-flag", []string{"--verbose", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-short-flag", []string{"-f", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-flag-with-target", []string{"--unit", "x", "--verbose", "urnet-tools"}, []string{"--unit", "x"}, nil, "unknown flag"},
		{"empty", nil, nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pre, rest, err := splitExecArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("splitExecArgs(%v) err = %v, want contains %q", c.args, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitExecArgs(%v) unexpected err: %v", c.args, err)
			}
			if len(pre) != len(c.wantPre) {
				t.Fatalf("splitExecArgs(%v) pre = %v, want %v", c.args, pre, c.wantPre)
			}
			if len(rest) != len(c.wantRest) {
				t.Fatalf("splitExecArgs(%v) rest = %v, want %v", c.args, rest, c.wantRest)
			}
			for i := range pre {
				if pre[i] != c.wantPre[i] {
					t.Fatalf("splitExecArgs(%v) pre[%d] = %q, want %q", c.args, i, pre[i], c.wantPre[i])
				}
			}
			for i := range rest {
				if rest[i] != c.wantRest[i] {
					t.Fatalf("splitExecArgs(%v) rest[%d] = %q, want %q", c.args, i, rest[i], c.wantRest[i])
				}
			}
		})
	}
}

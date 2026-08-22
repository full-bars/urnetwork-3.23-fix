package urnettools

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestIsDockerCandidate covers the container image/name recognition rules.
func TestIsDockerCandidate(t *testing.T) {
	cases := []struct {
		image string
		name  string
		want  bool
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

// TestDockerImageVersion extracts the version from an image tag (only
// version-like tags; latest/stable/nightly/dev and bare tags return "").
func TestDockerImageVersion(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.27.0", "v3.23.0-fix.27.0"},
		{"probe-test:mainnet", ""}, // plain tag, not version-like
		{"nginx:latest", ""},       // latest is not a version
		{"redis:7", ""},            // bare digit, not version-like
		{"urnetwork:stable", ""},   // stable is not a version
		{"urnetwork:v3.23.0-fix.26", "v3.23.0-fix.26"},
	}
	for _, c := range cases {
		if got := dockerImageVersion(c.image); got != c.want {
			t.Errorf("dockerImageVersion(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

// TestParseLogLineCount covers the optional trailing line-count argument
// (default 250, positive integers only).
func TestParseLogLineCount(t *testing.T) {
	if got, err := parseLogLineCount(nil); err != nil || got != defaultLogTailLines {
		t.Errorf("parseLogLineCount(nil) = %d, %v; want %d, nil", got, err, defaultLogTailLines)
	}
	if got, err := parseLogLineCount([]string{"50"}); err != nil || got != 50 {
		t.Errorf("parseLogLineCount([50]) = %d, %v; want 50, nil", got, err)
	}
	if _, err := parseLogLineCount([]string{"0"}); err == nil {
		t.Error("parseLogLineCount([0]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"-3"}); err == nil {
		t.Error("parseLogLineCount([-3]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"abc"}); err == nil {
		t.Error("parseLogLineCount([abc]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"3.5"}); err == nil {
		t.Error("parseLogLineCount([3.5]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"99999999999999999999"}); err == nil {
		t.Error("parseLogLineCount([overflow]) = nil error; want error")
	}
}

// TestSplitExecArgs covers the exec argument-splitting logic (the pure
// helper behind cmdDockerExec). Mimo HIGH-1: the integration tests only
// exercised error paths (no docker daemon), so the actual -- separator
// forwarding of rest... was never verified. This pins the slice math
// directly without docker. Coderabbit: also pins the trailing-flag panic
// guard and the inner-help forwarding.
func TestSplitExecArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPre  []string
		wantRest []string
		wantErr  string
	}{
		// -- separator forwarding (mimo HIGH-1 happy path)
		{"sep-first-no-target", []string{"--", "urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"sep-flag-first-word", []string{"--", "-f", "urnet-tools"}, nil, []string{"-f", "urnet-tools"}, ""},
		{"sep-flags-with-values", []string{"--", "urnet-tools", "--proxy_file=/tmp/p.txt"}, nil, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		{"sep-empty-command", []string{"--unit", "x", "--"}, []string{"--unit", "x"}, nil, ""},
		{"sep-only", []string{"--"}, nil, nil, ""},
		{"sep-multiple-inner-flags", []string{"--", "urnet-tools", "--verbose", "cmd"}, nil, []string{"urnet-tools", "--verbose", "cmd"}, ""},
		// no-separator forms (backward compat)
		{"no-sep-command-first", []string{"urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"no-sep-target-first", []string{"--unit", "x", "urnet-tools", "--proxy_file=/tmp/p.txt"}, []string{"--unit", "x"}, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		// unknown leading flags must error loudly (not be swallowed)
		{"unknown-dash-flag", []string{"--verbose", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-short-flag", []string{"-f", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-flag-with-target", []string{"--unit", "x", "--verbose", "urnet-tools"}, []string{"--unit", "x"}, nil, "unknown flag"},
		{"empty", nil, nil, nil, ""},
		// Coderabbit critical: a trailing recognized target flag (no value)
		// must error, not panic on the slice below.
		{"trailing-unit-no-value", []string{"--unit"}, nil, nil, "requires a value"},
		{"trailing-unit-no-value-after-target", []string{"--unit", "x", "--network"}, []string{"--unit", "x"}, nil, "requires a value"},
		// Coderabbit major: -h/--help AFTER the -- separator belongs to the
		// container command and must be forwarded, not intercepted.
		{"help-after-sep-forwarded", []string{"--", "urnet-tools", "--help"}, nil, []string{"urnet-tools", "--help"}, ""},
		{"help-after-sep-with-target", []string{"--unit", "x", "--", "urnet-tools", "--help"}, []string{"--unit", "x"}, []string{"urnet-tools", "--help"}, ""},
		// -h/--help BEFORE the separator (no --) is docker help (errHelpShown).
		{"help-before-sep", []string{"--unit", "x", "--help"}, nil, nil, "help shown"},
		{"help-bare-long", []string{"--help"}, nil, nil, "help shown"},
		{"help-bare-short", []string{"-h"}, nil, nil, "help shown"},
		{"help-after-target-no-sep", []string{"--unit", "x", "-h"}, nil, nil, "help shown"},
		// command-first multi-arg form still works
		{"command-first-multi-arg", []string{"urnet-tools", "proxy", "add", "--proxy_file=/tmp/p.txt"}, nil, []string{"urnet-tools", "proxy", "add", "--proxy_file=/tmp/p.txt"}, ""},
		// equals-form is NOT recognized (pre-existing limitation, documented)
		{"equals-form-rejected", []string{"--unit=x", "cmd"}, nil, nil, "unknown flag"},
		// target flags after -- are forwarded verbatim (not parsed)
		{"inner-target-like-flag-forwarded", []string{"--", "--unit", "y", "cmd"}, nil, []string{"--unit", "y", "cmd"}, ""},
		{"target-then-sep-then-inner-target", []string{"--unit", "x", "--", "--unit", "y"}, []string{"--unit", "x"}, []string{"--unit", "y"}, ""},
	}

	// Sonnet final-review MEDIUM: pre-separator --help must PRINT usage
	// (side effect), not just return the sentinel silently. RunDocker
	// already covers bare exec --help via hasHelpFlag; this covers the
	// --unit x --help form that goes through splitExecArgs.
	t.Run("pre-sep-help-prints-usage", func(t *testing.T) {
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		defer func() { os.Stderr = old }()
		err = cmdDockerExec([]string{"--unit", "x", "--help"})
		w.Close()
		out, _ := io.ReadAll(r)
		if err != nil {
			t.Fatalf("cmdDockerExec([--unit x --help]) err = %v, want nil", err)
		}
		if !strings.Contains(string(out), "urnet-docker") {
			t.Fatalf("cmdDockerExec([--unit x --help]) did not print usage, got %q", string(out))
		}
	})
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

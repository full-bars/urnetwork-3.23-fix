package urnettools

import (
	"os"
	"strings"
	"testing"
)

func TestTriggerHotSwapInvalidPID(t *testing.T) {
	p := Provider{
		PID: -1,
	}
	err := triggerHotSwap(p)
	if err == nil {
		t.Errorf("expected error for invalid PID, got nil")
	}
}

func TestCmdHotswapNotRunning(t *testing.T) {
	// Targeting non-running provider must error
	err := cmdHotswap([]string{"--unit", "nonexistent.service"}, true, false)
	if err == nil {
		t.Errorf("expected error targeting non-existent provider, got nil")
	}
}

func TestCmdHotswapHelp(t *testing.T) {
	// Running help for hotswap through Run CLI
	err := Run([]string{"hotswap", "--help"})
	if err != nil {
		t.Errorf("expected hotswap --help to succeed, got %v", err)
	}
}

func TestHotswapCobraCommandRegistered(t *testing.T) {
	cmd := buildRootCmd()
	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "hotswap" {
			found = true
			if !strings.Contains(c.Short, "zero-downtime") {
				t.Errorf("unexpected short description: %s", c.Short)
			}
			break
		}
	}
	if !found {
		t.Errorf("hotswap command not registered in root command list")
	}
}

func TestIsHotSwapSupportedVersion(t *testing.T) {
	cases := []struct {
		ver  string
		want bool
	}{
		{"v3.23.0-fix.30.9", false},
		{"v3.23.0-fix.30.0", false},
		{"v3.23.0-fix.28.1", false},
		{"v3.23.0-fix.31.0", true},
		{"v3.23.0-fix.31.0-alpha1", true},
		{"v3.23.0-fix.31.0-alpha2", true},
		{"v3.23.0-fix.32.0", true},
		{"dev", true},
		{"", false},
		{"invalid", false},
	}
	for _, tc := range cases {
		got := isHotSwapSupportedVersion(tc.ver)
		if got != tc.want {
			t.Errorf("isHotSwapSupportedVersion(%q) = %v, want %v", tc.ver, got, tc.want)
		}
	}
}

func TestTriggerHotSwapNotCapable(t *testing.T) {
	p := Provider{
		PID:     os.Getpid(),
		Version: "v3.23.0-fix.30.9", // older release without hotswap
		Running: true,
	}
	err := triggerHotSwap(p)
	if err == nil {
		t.Errorf("expected error triggering hotswap on provider running v3.23.0-fix.30.9")
	}
}

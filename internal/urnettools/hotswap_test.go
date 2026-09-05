package urnettools

import (
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

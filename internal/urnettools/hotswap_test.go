package urnettools

import (
	"errors"
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

// TestSupportsHotSwapUnitType pins the interaction between version support
// and systemd unit Type=: a supported version is not enough on its own if
// the owning unit isn't Type=notify, because provider/hotswap.go silently
// aborts the in-process handoff for any other unit type (see hotSwapUnitOK
// for the full mechanism). Getting this backwards either strands
// pre-existing Type=simple fleet nodes on a permanently-no-op update path
// (false "yes"), or needlessly denies zero-downtime handoff to units that
// are genuinely Type=notify (false "no").
func TestSupportsHotSwapUnitType(t *testing.T) {
	origUnitType := unitTypeFunc
	defer func() { unitTypeFunc = origUnitType }()

	const supportedVersion = "v3.23.0-fix.31.0"
	const unsupportedVersion = "v3.23.0-fix.30.9"

	cases := []struct {
		name        string
		version     string
		unitType    string
		unitTypeErr error
		hasUnit     bool
		want        bool
	}{
		{
			name:     "notify unit + supported version -> supported",
			version:  supportedVersion,
			unitType: "notify",
			hasUnit:  true,
			want:     true,
		},
		{
			name:     "simple unit + supported version -> NOT supported",
			version:  supportedVersion,
			unitType: "simple",
			hasUnit:  true,
			want:     false,
		},
		{
			name:     "notify unit + unsupported version -> not supported regardless of unit type",
			version:  unsupportedVersion,
			unitType: "notify",
			hasUnit:  true,
			want:     false,
		},
		{
			name:     "simple unit + unsupported version -> not supported",
			version:  unsupportedVersion,
			unitType: "simple",
			hasUnit:  true,
			want:     false,
		},
		{
			name:        "unit type undeterminable -> not supported (safe fallback)",
			version:     supportedVersion,
			unitTypeErr: errors.New("systemctl: command not found"),
			hasUnit:     true,
			want:        false,
		},
		{
			name:    "no owning unit at all (e.g. Docker PID-1) -> version alone decides",
			version: supportedVersion,
			hasUnit: false,
			want:    true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unitTypeFunc = func(Provider) (string, error) {
				if c.unitTypeErr != nil {
					return "", c.unitTypeErr
				}
				return c.unitType, nil
			}
			p := Provider{Version: c.version}
			if c.hasUnit {
				p.Unit = "urnetwork.service"
			}
			if got := supportsHotSwap(p); got != c.want {
				t.Errorf("supportsHotSwap(%+v) = %v, want %v", p, got, c.want)
			}
		})
	}
}

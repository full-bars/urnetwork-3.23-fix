package urnettools

// Verifies that `urnet-tools choose-network main` passes the network preset
// through to the provider binary as `provider choose_network main` (the
// preset support added to the provider's choose_network command).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProviderSubcommandChooseNetworkPresetPassthrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub binary is POSIX-only")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-provider")
	script := "#!/bin/sh\necho \"ARGS:$*\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Provider{Binary: bin, Unit: "test.service"}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = providerSubcommand(p, "choose_network", "main")
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("providerSubcommand(choose_network main) = %v, want nil", err)
	}
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "ARGS:choose_network main") {
		t.Errorf("provider binary saw %q, want it to include 'ARGS:choose_network main'", out)
	}
	fmt.Fprint(os.Stderr, out)
}

// parseTargetFlagsLenient must keep a bare preset token ("main"/"beta") in
// rest — that is what makes `choose-network main` delegate as
// `provider choose_network main`.
func TestParseTargetFlagsLenientKeepsPresetInRest(t *testing.T) {
	for _, preset := range []string{"main", "beta"} {
		_, rest, err := parseTargetFlagsLenient([]string{preset})
		if err != nil {
			t.Fatalf("parseTargetFlagsLenient([%q]) = %v, want nil", preset, err)
		}
		if len(rest) != 1 || rest[0] != preset {
			t.Errorf("parseTargetFlagsLenient([%q]) rest = %v, want [%q] (preset must survive for pass-through)", preset, rest, preset)
		}
	}
}

package urnettools

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDockerTopArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		persistent bool
		want       []string
	}{
		{"unset", nil, false, []string{"exec", "-it", "unit", "/usr/local/bin/urnet-tools-go", "top", "--copy"}},
		{"set", map[string]string{"TERM": "xterm", "COLORTERM": ""}, true, []string{"exec", "-it", "-e", "COLORTERM", "-e", "TERM", "-e", "XDG_CONFIG_HOME=/root/.urnetwork/.config", "unit", "/usr/local/bin/urnet-tools-go", "top", "--copy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerTopArgs("unit", []string{"--copy"}, func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }, tc.persistent)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDockerTopTargets(t *testing.T) {
	providers := []Provider{{Unit: "alpha"}, {Unit: "500ms"}}
	for _, tc := range []struct {
		args, want []string
		unit       string
	}{
		{[]string{"alpha", "--interval", "500ms"}, []string{"--interval", "500ms"}, "alpha"},
		{[]string{"--interval", "500ms", "alpha"}, []string{"--interval", "500ms"}, "alpha"},
		{[]string{"--interval", "500ms"}, []string{"--interval", "500ms"}, ""},
	} {
		got, rest := consumeDockerTopTarget(providers, Target{}, tc.args)
		if got.Unit != tc.unit || !reflect.DeepEqual(rest, tc.want) {
			t.Fatalf("args %q: got %q %q", tc.args, got.Unit, rest)
		}
	}
}

func TestDockerTopFakeCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\nif [ \"$1\" = inspect ]; then printf yes; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$TOP_TEST_LOG\"\nexit \"${TOP_TEST_EXIT:-0}\"\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	old := testDockerBin
	testDockerBin = fake
	t.Cleanup(func() { testDockerBin = old })
	t.Setenv("TOP_TEST_LOG", log)
	t.Setenv("TERM", "xterm")
	t.Setenv("COLORTERM", "truecolor")
	if err := containerTopExecWithTerminal("alpha", []string{"--copy"}, false); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("no tty: %v", err)
	}
	if err := containerTopExecWithTerminal("alpha", []string{"--copy"}, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "exec\n-it\n-e\nCOLORTERM\n-e\nTERM\n-e\nXDG_CONFIG_HOME=/root/.urnetwork/.config\nalpha\n/usr/local/bin/urnet-tools-go\ntop\n--copy\n"
	if string(b) != want {
		t.Fatalf("arguments: %q", b)
	}
	t.Setenv("TOP_TEST_EXIT", "127")
	if err := containerTopExecWithTerminal("alpha", nil, true); err == nil || !strings.Contains(err.Error(), "update the image") {
		t.Fatalf("missing binary: %v", err)
	}
	t.Setenv("TOP_TEST_EXIT", "1")
	if err := containerTopExecWithTerminal("alpha", nil, true); err == nil || !strings.Contains(err.Error(), "may have stopped") {
		t.Fatalf("exec failure: %v", err)
	}
}

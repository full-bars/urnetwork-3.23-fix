package urnettools

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The live screen follows the provider: after a restart the next look-up gives
// the new process, and a provider discovery no longer lists keeps its last value.
func TestRefreshStatusProviderFollowsARestart(t *testing.T) {
	old := Provider{Unit: "a.service", User: "u", StateDir: "/state/a", Running: true, PID: 100, Version: "v1"}
	stubStatus(t, []Provider{{Unit: "a.service", User: "u", StateDir: "/state/a", Running: true, PID: 200, Version: "v2"}}, nil, true)
	got := refreshStatusProvider(old)
	if got.PID != 200 || got.Version != "v2" {
		t.Fatalf("refreshed provider = %+v, want pid 200 and v2", got)
	}
	stubStatus(t, []Provider{{Unit: "a.service", User: "u", StateDir: "/state/a", Running: false}}, nil, true)
	if got := refreshStatusProvider(old); got.Running || got.PID != 0 {
		t.Fatalf("a stopped unit must read stopped, got %+v", got)
	}
	stubStatus(t, []Provider{{Unit: "other.service", StateDir: "/state/other"}}, nil, true)
	if got := refreshStatusProvider(old); got.PID != 100 {
		t.Fatalf("a provider that is not listed keeps its value, got %+v", got)
	}
}

func TestStatusFrameLinesCountsWrappedRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		width int
		want  int
	}{
		{"empty", "", 80, 0},
		{"two rows", "one\ntwo\n", 80, 2},
		{"no trailing newline", "one\ntwo", 80, 2},
		{"a long row wraps", strings.Repeat("x", 25) + "\nshort\n", 10, 4},
		{"escapes take no width", "\x1b[31m" + strings.Repeat("x", 10) + "\x1b[0m\n", 10, 1},
		{"unknown width", strings.Repeat("x", 500) + "\n", 0, 1},
	} {
		if got := statusFrameLines(tc.text, tc.width); got != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.name, got, tc.want)
		}
	}
}

// Only the previous frame is replaced: the cursor moves up over exactly its rows.
func TestLiveRedrawPrefix(t *testing.T) {
	if got := liveRedrawPrefix(0, 40); got != "" {
		t.Fatalf("first frame prefix = %q, want none", got)
	}
	if got := liveRedrawPrefix(7, 40); got != "\x1b[7A\r\x1b[J" {
		t.Fatalf("prefix = %q, want cursor up 7 and erase below", got)
	}
	if got := liveRedrawPrefix(40, 40); got != "\x1b[H\x1b[J" {
		t.Fatalf("a frame as tall as the screen clears it, got %q", got)
	}
	if got := liveRedrawPrefix(7, 0); got != "\x1b[7A\r\x1b[J" {
		t.Fatalf("unknown height prefix = %q", got)
	}
}

// `status --brief` answers a script with its exit code 1, 2 or 3. Under sudo
// that code must reach the caller and not become a generic failure.
func TestElevatedRunErrorKeepsTheBriefExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	exitWith := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	for _, code := range []string{"1", "2", "3"} {
		err := elevatedRunError([]string{"status", "--brief", "--user", "x"}, exitWith(code))
		var coded briefExitError
		if !errors.As(err, &coded) || int(coded) != int(code[0]-'0') {
			t.Fatalf("exit %s through status --brief gave %v", code, err)
		}
	}
	for name, args := range map[string][]string{
		"another command":   {"restart", "--user", "x"},
		"status without it": {"status", "--user", "x"},
	} {
		if err := elevatedRunError(args, exitWith("2")); err == nil || errors.As(err, new(briefExitError)) || !strings.Contains(err.Error(), "elevated") {
			t.Fatalf("%s: err = %v, want a failed elevation", name, err)
		}
	}
	if err := elevatedRunError([]string{"status", "--brief"}, exitWith("4")); err == nil || errors.As(err, new(briefExitError)) {
		t.Fatalf("exit 4 is not a status answer, got %v", err)
	}
	if err := elevatedRunError([]string{"status", "--brief"}, nil); err != nil {
		t.Fatalf("a clean exit is no error, got %v", err)
	}
}

// The word on the brief line and the exit code come from one rule. A broken
// row with no snapshot used to print "unreachable" while exiting 2.
func TestStatusBriefWordFollowsTheExitCode(t *testing.T) {
	bad := statusRow{Name: "Unit", Level: statusBad, Text: "failed"}
	warn := statusRow{Name: "Proxies", Level: statusWarn, Text: "few"}
	ok := statusRow{Name: "Health", Level: statusOK, Text: "flowing"}
	snap := &NodeSnapshot{}
	for name, d := range map[string]statusData{
		"healthy":                 {Snapshot: snap, Rows: []statusRow{ok}},
		"warning":                 {Snapshot: snap, Rows: []statusRow{ok, warn}},
		"broken":                  {Snapshot: snap, Rows: []statusRow{warn, bad}},
		"unreachable":             {Rows: []statusRow{ok}},
		"broken and unreachable":  {Rows: []statusRow{bad}},
		"warning and unreachable": {Rows: []statusRow{warn}},
	} {
		want := map[int]string{0: "ok", 1: "warn", 2: "bad", 3: "unreachable"}[statusExitCode(d)]
		if got := strings.Fields(statusBriefLine(d))[0]; got != want {
			t.Errorf("%s: brief word %q, exit code says %q", name, got, want)
		}
	}
}

// A reading that succeeded and found no risk is a result, not a missing value.
func TestStatusUnitRowIsOkAfterANegativeSigpipeReading(t *testing.T) {
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, nil, true)
	orig := statusSigpipeStateFn
	t.Cleanup(func() { statusSigpipeStateFn = orig })
	for name, tc := range map[string]struct {
		state sigpipeState
		err   error
		want  statusLevel
	}{
		"no risk":        {sigpipeState{restart: "always"}, nil, statusOK},
		"risk":           {sigpipeState{restart: "no"}, nil, statusWarn},
		"failed reading": {sigpipeState{}, errors.New("no systemctl"), statusUnknown},
	} {
		statusSigpipeStateFn = func(Provider) (sigpipeState, error) { return tc.state, tc.err }
		var level statusLevel
		for _, row := range collectStatusData(p).Rows {
			if row.Name == "Unit" {
				level = row.Level
			}
		}
		if level != tc.want {
			t.Errorf("%s: unit row level %q, want %q", name, level, tc.want)
		}
	}
}

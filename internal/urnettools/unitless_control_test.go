package urnettools

// Tests for the unitless-provider control work: dedup of duplicate RUNNING
// rows sharing one state dir, --pid targeting, graceful stop of a provider
// running outside systemd, and log diagnostics for discarded output.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- dedupRunningProviders -------------------------------------------------

func TestDedupRunningProviders_TrackerFalsePositive(t *testing.T) {
	// LA1: real provider (exact known binary) + provider_tracker sharing one
	// state dir via the default-home attribution. Only the real provider row
	// may survive.
	real := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/home/user/.local/share/urnetwork-provider/bin/urnetwork", PID: 2094085, Running: true, Network: "mesocyclone"}
	tracker := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/home/user/provider_tracking/provider_tracker", PID: 2095016, Running: true, Network: "mesocyclone"}
	got := dedupRunningProviders([]Provider{real, tracker})
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(got), got)
	}
	if got[0].PID != real.PID {
		t.Errorf("survivor should be the exact-known-binary provider (pid %d), got pid %d", real.PID, got[0].PID)
	}
}

func TestDedupRunningProviders_UnitRowPreferred(t *testing.T) {
	// Same provider seen as a process row and a unit-backed row: keep the
	// unit row so logs/stop route through systemd.
	process := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/usr/bin/urnetwork", PID: 100, Running: true, Network: "net"}
	unit := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/usr/bin/urnetwork", PID: 100, Running: true, Unit: "urnetwork.service", Network: "net"}
	got := dedupRunningProviders([]Provider{process, unit})
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].Unit != "urnetwork.service" {
		t.Errorf("unit-backed row should survive, got %+v", got[0])
	}
}

func TestDedupRunningProviders_DistinctStateDirsKept(t *testing.T) {
	a := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/usr/bin/urnetwork", PID: 100, Running: true}
	b := Provider{User: "user", StateDir: "/home/user/.other", Binary: "/usr/bin/urnetwork", PID: 101, Running: true}
	got := dedupRunningProviders([]Provider{a, b})
	if len(got) != 2 {
		t.Fatalf("two distinct state dirs must both survive, got %d", len(got))
	}
}

func TestDedupRunningProviders_StoppedUnitAndRunningManualKept(t *testing.T) {
	// A stopped unit next to a running manual process legitimately shares
	// the state dir: both must stay visible (that is the systemd-failed +
	// orphan combination from LA1).
	stopped := Provider{User: "user", StateDir: "/home/user/.urnetwork", Unit: "urnetwork.service", Running: false}
	manual := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/opt/urnetwork", PID: 200, Running: true}
	got := dedupRunningProviders([]Provider{stopped, manual})
	if len(got) != 2 {
		t.Fatalf("stopped + manual rows must both survive, got %d", len(got))
	}
}

func TestDedupRunningProviders_OrderIndependent(t *testing.T) {
	// Survivor must not depend on input order (tracker listed first).
	tracker := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/opt/provider_tracker", PID: 2095016, Running: true}
	real := Provider{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/usr/bin/urnetwork", PID: 2094085, Running: true}
	got := dedupRunningProviders([]Provider{tracker, real})
	if len(got) != 1 || got[0].PID != real.PID {
		t.Fatalf("want the exact-known-binary provider regardless of order, got %+v", got)
	}
}

// --- --pid targeting -------------------------------------------------------

func TestParseTargetFlags_Pid(t *testing.T) {
	tgt, rest, err := parseTargetFlags([]string{"--pid", "4242"})
	if err != nil {
		t.Fatalf("--pid space form: %v", err)
	}
	if tgt.PID != 4242 || len(rest) != 0 {
		t.Errorf("space form: got %+v rest=%v", tgt, rest)
	}
	tgt, _, err = parseTargetFlags([]string{"--pid=4242"})
	if err != nil || tgt.PID != 4242 {
		t.Errorf("equals form: %+v err=%v", tgt, err)
	}
	if _, _, err := parseTargetFlags([]string{"--pid", "abc"}); err == nil {
		t.Error("non-numeric pid must error")
	}
	if _, _, err := parseTargetFlags([]string{"--pid", "0"}); err == nil {
		t.Error("pid 0 must error")
	}
	if _, _, err := parseTargetFlags([]string{"--pid", "1", "--user", "x"}); err == nil {
		t.Error("--pid combined with another selector must error")
	}
	if _, _, err := parseTargetFlags([]string{"--pid"}); err == nil {
		t.Error("missing --pid value must error")
	}
	if !hasExplicitTarget(Target{PID: 7}) {
		t.Error("PID target must count as explicit")
	}
}

func TestSelectTargetByPid(t *testing.T) {
	providers := []Provider{
		{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/usr/bin/urnetwork", PID: 100, Running: true},
		{User: "user", StateDir: "/home/user/.urnetwork", Binary: "/opt/provider_tracker", PID: 101, Running: true},
	}
	p, err := selectTarget(providers, Target{PID: 101})
	if err != nil {
		t.Fatalf("selectTarget: %v", err)
	}
	if p.PID != 101 {
		t.Errorf("want pid 101, got %+v", p)
	}
	if _, err := selectTarget(providers, Target{PID: 999}); err == nil {
		t.Error("unknown pid must error")
	}
}

// --- unitless stop ---------------------------------------------------------

func TestStopUnitlessProvider_Graceful(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process signaling is unix-only (no SIGTERM/SIGKILL semantics on Windows)")
	}
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	restore := swapGracePeriod(5 * time.Second)
	defer restore()

	p := Provider{User: "user", StateDir: "/tmp/x", Binary: "/usr/bin/urnetwork", PID: cmd.Process.Pid, Running: true}
	if err := stopUnitlessProvider(p, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Error("child still alive after graceful stop")
	}
}

func TestStopUnitlessProvider_ForceAfterHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process signaling is unix-only (no SIGTERM/SIGKILL semantics on Windows)")
	}
	// Child ignores SIGTERM (a la a wedged provider) via a self-exec'd
	// helper; without --force the stop must error and leave it alive; with
	// --force it must die.
	cmd := exec.Command(os.Args[0], "-test.run=TestIgnoreTermHelper")
	cmd.Env = append(os.Environ(), "IGNORE_TERM_BEHAVIOR=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// The child prints READY only AFTER installing signal.Ignore(SIGTERM)
	// — send our SIGTERM only then, so the ignore is in effect (a signal
	// before it lands would hit the default disposition and kill the child).
	rd := bufio.NewReader(stdout)
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, "READY") {
		t.Fatalf("child never became ready: %q err=%v", line, err)
	}
	restore := swapGracePeriod(1 * time.Second)
	defer restore()

	p := Provider{User: "user", StateDir: "/tmp/x", Binary: "/usr/bin/urnetwork", PID: cmd.Process.Pid, Running: true}
	err = stopUnitlessProvider(p, false)
	if err == nil {
		t.Fatal("want error when the process refuses to drain")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should point at --force, got: %v", err)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("child died without --force — graceful stop must not escalate")
	}
	if err := stopUnitlessProvider(p, true); err != nil {
		t.Fatalf("force stop: %v", err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Error("child still alive after SIGKILL")
	}
}

// TestIgnoreTermHelper is a self-exec'd test child that ignores SIGTERM and
// sleeps until killed. It only activates with IGNORE_TERM_BEHAVIOR=1; a bare
// run is a no-op PASS.
func TestIgnoreTermHelper(t *testing.T) {
	if os.Getenv("IGNORE_TERM_BEHAVIOR") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	fmt.Println("READY")
	os.Stdout.Sync()
	time.Sleep(time.Hour)
}

func TestStopUnitlessProvider_NotRunning(t *testing.T) {
	p := Provider{User: "user", StateDir: "/tmp/x", Running: false}
	if err := stopUnitlessProvider(p, false); err == nil {
		t.Error("stopping a non-running unitless provider must error")
	}
}

// --- unitless logs ---------------------------------------------------------

func TestLogsUnitlessProvider_DiscardedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/proc/<pid>/fd reads are unix-only")
	}
	// Classic manual launch: stdout -> /dev/null. logs must give a diagnosis,
	// not an empty stream or a journalctl failure.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	cmd := exec.Command("sleep", "300")
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	p := Provider{User: "user", StateDir: "/tmp/x", Binary: "/usr/bin/urnetwork", PID: cmd.Process.Pid, Running: true}
	err = logsUnitlessProvider(p, 50)
	if err == nil {
		t.Fatal("want an error explaining the missing log stream")
	}
	if !strings.Contains(err.Error(), "/dev/null") || !strings.Contains(err.Error(), "no log stream") {
		t.Errorf("diagnosis should mention /dev/null and the missing stream, got: %v", err)
	}
	if !stdoutDiscarded(cmd.Process.Pid) {
		t.Error("stdoutDiscarded should be true for a /dev/null stdout")
	}
}

// --- operational notes -----------------------------------------------------

func TestProviderOperationalNotes_Unitless(t *testing.T) {
	notes := providerOperationalNotes([]Provider{
		{User: "user", StateDir: "/tmp/x", Binary: "/usr/bin/urnetwork", PID: 123, Running: true},
	})
	found := false
	for _, n := range notes {
		if strings.Contains(n, "runs outside systemd") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a unitless note, got %v", notes)
	}
}

func TestProviderOperationalNotes_NoUnitlessNoNote(t *testing.T) {
	// A unit-backed running provider whose control socket is up must get no
	// notes; stopped rows never get notes.
	stateDir := t.TempDir()
	sockPath := stateDir + "/provider.sock"
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("cannot bind mock socket (path too long on this system): %v", err)
	}
	defer ln.Close()
	notes := providerOperationalNotes([]Provider{
		{User: "user", StateDir: stateDir, Unit: "urnetwork.service", PID: 123, Running: true},
		{User: "user", StateDir: stateDir, Unit: "urnetwork.service", Running: false},
	})
	if len(notes) != 0 {
		t.Errorf("managed/stopped providers should produce no notes, got %v", notes)
	}
}

// swapGracePeriod shrinks the drain window for tests and restores it after.
func swapGracePeriod(d time.Duration) func() {
	old := unitlessStopGracePeriod
	unitlessStopGracePeriod = d
	return func() { unitlessStopGracePeriod = old }
}

// --- get / show (read-only override view) -----------------------------------

func TestCmdGetRejectsValueForm(t *testing.T) {
	// get is read-only: a key+value form must error, never write.
	err := cmdGet([]string{"gomemlimit", "1G"})
	if err == nil {
		t.Fatal("get gomemlimit 1G must be rejected")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("want a read-only explanation, got: %v", err)
	}
}

func TestCmdGetReadFormsReachSelection(t *testing.T) {
	// Stub discovery empty so selection sees zero providers regardless of
	// what is running on the dev box; all read forms must then fail with
	// the selection error — not a flag parse error — proving target flags
	// and key shapes reached the set machinery intact.
	origP, origS := discoverProcessesFn, discoverStoppedFn
	discoverProcessesFn = func() []Provider { return nil }
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	defer func() { discoverProcessesFn, discoverStoppedFn = origP, origS }()
	for _, args := range [][]string{
		{},
		{"--unit", "urnetwork.service"},
		{"gomemlimit"},
		{"--pid", "42", "gomemlimit"},
	} {
		err := cmdGet(args)
		if err == nil {
			t.Errorf("get %v with no providers must error", args)
		}
		if strings.Contains(err.Error(), "read-only") && len(args) > 1 {
			t.Errorf("get %v: unexpected read-only rejection: %v", args, err)
		}
	}
}

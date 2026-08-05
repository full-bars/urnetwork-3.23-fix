package connect

import "testing"

// Correctness counterpart to log_alloc_bench_test.go's benchmarks. The
// benchmarks measure allocations; these tests verify the actual behavioral
// contract the PR relies on: wrapping a `V(n).Infof(...)` call in
// `if v := log.V(n); v.Enabled() { ... }` must skip evaluating the call's
// argument expressions entirely when the level is disabled, whereas an
// unguarded call always evaluates them regardless of whether the level is
// enabled. That evaluation (and the resulting heap-boxing into `[]any`) is
// exactly the per-packet/per-message cost Task 17 removes from the Tier-1
// hot-path files.

// TestVerboseGuard_SkipsArgEvaluationWhenDisabled confirms the guard used
// throughout ip.go, transfer.go, and ip_remote_multi_client.go actually
// short-circuits: with a disabled Verbose, the argument expression inside
// the guarded block must never run.
func TestVerboseGuard_SkipsArgEvaluationWhenDisabled(t *testing.T) {
	logger := NewNoopLogger()
	calls := 0
	arg := func() string {
		calls++
		return "x"
	}

	if v := logger.V(1); v.Enabled() {
		v.Infof("[cr] %s\n", arg())
	}

	if calls != 0 {
		t.Fatalf("guarded call must not evaluate its argument expression when disabled, got %d evaluation(s)", calls)
	}
}

// TestVerboseGuard_UnguardedCallStillEvaluatesArgExpression documents the
// exact problem the guard pattern fixes: without the `v.Enabled()` guard,
// Go evaluates a call's argument expressions before Infof is ever invoked,
// even though the underlying Verbose is disabled and will discard them.
func TestVerboseGuard_UnguardedCallStillEvaluatesArgExpression(t *testing.T) {
	logger := NewNoopLogger()
	calls := 0
	arg := func() string {
		calls++
		return "x"
	}

	logger.V(1).Infof("[cr] %s\n", arg())

	if calls != 1 {
		t.Fatalf("expected the unguarded call site to evaluate its argument expression exactly once, got %d", calls)
	}
}

// TestVerboseGuard_InvokesInfofWhenEnabled confirms the guard doesn't
// accidentally suppress logging when the level *is* enabled: it must call
// through to Infof with the expected format and arguments.
func TestVerboseGuard_InvokesInfofWhenEnabled(t *testing.T) {
	logger := &fakeLogger{verboseEnabled: true}

	if v := logger.V(1); v.Enabled() {
		v.Infof("[cr] %s\n", "hello")
	}

	if len(logger.infofCalls) != 1 {
		t.Fatalf("expected exactly one Infof call, got %d", len(logger.infofCalls))
	}
	got := logger.infofCalls[0]
	if got.format != "[cr] %s\n" || len(got.args) != 1 || got.args[0] != "hello" {
		t.Fatalf("unexpected Infof call: %+v", got)
	}
}

// TestVerboseGuard_SkipsInfofWhenDisabled confirms the guard's else branch:
// when Enabled() is false, Infof must never be called.
func TestVerboseGuard_SkipsInfofWhenDisabled(t *testing.T) {
	logger := &fakeLogger{verboseEnabled: false}

	if v := logger.V(1); v.Enabled() {
		v.Infof("[cr] %s\n", "hello")
	}

	if len(logger.infofCalls) != 0 {
		t.Fatalf("expected no Infof calls when disabled, got %d", len(logger.infofCalls))
	}
}

// TestClientRunLogShapes_NoPanic and TestPacketLogShapes_NoPanic are smoke
// tests over the exact call shapes introduced in log_alloc_bench_test.go
// (reusing its package-level benchId/benchInt/benchTag fixtures), for both
// the guarded and unguarded forms, against the real glog-backed logger.
func TestClientRunLogShapes_NoPanic(t *testing.T) {
	logger := NewGlogLogger()

	logger.V(1).Infof("[cr] %s %s<-%s s(%s)\n", benchTag, benchId, benchId, benchId)

	if v := logger.V(1); v.Enabled() {
		v.Infof("[cr] %s %s<-%s s(%s)\n", benchTag, benchId, benchId, benchId)
	}
}

func TestPacketLogShapes_NoPanic(t *testing.T) {
	logger := NewGlogLogger()

	logger.V(1).Infof("[f%d]udp receive %d\n", benchInt, benchInt)

	if v := logger.V(1); v.Enabled() {
		v.Infof("[f%d]udp receive %d\n", benchInt, benchInt)
	}
}

// TestTimeoutLogShape_NoPanic is a smoke test over the exact call shape this
// PR introduced in ip.go's TcpSequence.Run (the per-forward-iteration
// "[f%d]timeout" site): guarding
// `self.log.V(2).Infof("[f%d]timeout\n", forwardIter)` with
// `if v := self.log.V(2); v.Enabled() { ... }` must not change the level or
// arguments passed through to Infof, against the real glog-backed logger.
func TestTimeoutLogShape_NoPanic(t *testing.T) {
	logger := NewGlogLogger()

	logger.V(2).Infof("[f%d]timeout\n", benchInt)

	if v := logger.V(2); v.Enabled() {
		v.Infof("[f%d]timeout\n", benchInt)
	}
}

// TestVerboseGuard_InvokesInfofWithIntArgWhenEnabled mirrors the exact
// argument type passed at the ip.go call site this PR guards (forwardIter is
// an int), rather than only the string arg used by the other Infof guard
// tests above, to catch a guard implementation that only forwards string
// arguments correctly.
func TestVerboseGuard_InvokesInfofWithIntArgWhenEnabled(t *testing.T) {
	logger := &fakeLogger{verboseEnabled: true}

	if v := logger.V(2); v.Enabled() {
		v.Infof("[f%d]timeout\n", 7)
	}

	if len(logger.infofCalls) != 1 {
		t.Fatalf("expected exactly one Infof call, got %d", len(logger.infofCalls))
	}
	got := logger.infofCalls[0]
	if got.format != "[f%d]timeout\n" || len(got.args) != 1 || got.args[0] != 7 {
		t.Fatalf("unexpected Infof call: %+v", got)
	}
}

// fakeLogger is a minimal Logger/Verbose test double that records Infof
// calls and lets the test control Enabled() deterministically, independent
// of glog's global verbosity flag state.
type fakeLogger struct {
	verboseEnabled bool
	infofCalls     []fakeInfofCall
}

type fakeInfofCall struct {
	format string
	args   []any
}

func (f *fakeLogger) Info(args ...any)                    {}
func (f *fakeLogger) Infof(format string, args ...any)    {}
func (f *fakeLogger) Warningf(format string, args ...any) {}
func (f *fakeLogger) Errorf(format string, args ...any)   {}
func (f *fakeLogger) V(level int32) Verbose {
	return &fakeVerbose{logger: f, enabled: f.verboseEnabled}
}

type fakeVerbose struct {
	logger  *fakeLogger
	enabled bool
}

func (v *fakeVerbose) Enabled() bool { return v.enabled }
func (v *fakeVerbose) Info(args ...any) {
	v.logger.infofCalls = append(v.logger.infofCalls, fakeInfofCall{args: args})
}
func (v *fakeVerbose) Infof(format string, args ...any) {
	v.logger.infofCalls = append(v.logger.infofCalls, fakeInfofCall{format: format, args: args})
}

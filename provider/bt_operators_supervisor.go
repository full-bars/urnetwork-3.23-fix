package main

// The supervisor of "provide --all-operators". Ported from the sn miner's
// operators_supervisor.go (upstream 6608298e), adapted to the v33 provider's
// state model:
//
//   - It re-executes this binary as one "provide" child per listed operator
//     that has a jwt, with the operator's urls from the list.
//   - There is no URNETWORK_STATE_DIR in this binary: every state path is
//     keyed off HOME, so each child is handed the operator's directory
//     (operatorlist.DomainStateDir below the provider state directory) as
//     HOME, and the child's state directory is the .urnetwork inside it
//     (operatorJwtPath is the jwt there). An inherited HOME is dropped.
//   - An operator without a jwt is awaiting auth. Without --auto-register it
//     gets no child and its jwt is checked again every 5 minutes. With
//     --auto-register the hotkey signs in to the operator and writes the jwt.
//   - The hotkey wallet upkeep of sn's operator lane is not ported here
//     (Stage A): there is no hotkey wallet fan-out in this supervisor.
//
// The loop keeps the rest of the sn behavior unchanged:
//   - a child that exits restarts after a backoff, 30s doubling to 10m. A
//     child that ran for 10m starts the doubling over.
//   - the child of a delisted operator, or one whose urls or memory share
//     changed, gets SIGTERM and SIGKILL 60s later. The state directory stays.
//   - SIGINT, SIGTERM and SIGQUIT are forwarded to every child, and the
//     supervisor returns once all of them have exited and no sign-in runs.
//   - --port serves the supervisor's status, and is off at 0 (the default),
//     exactly as in sn.
//
// Only the Run loop touches operator state. Child exits and sign-ins reach it
// over channels; the status page reads a published snapshot.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/hotkeyauth"
	"github.com/urfoundation/sn/operatorlist"
)

const operatorRestartMinimumDelay = 30 * time.Second
const operatorRestartMaximumDelay = 10 * time.Minute
const operatorStopGrace = 60 * time.Second
const operatorAuthRecheckInterval = 5 * time.Minute

// After a child set a rejected network sign-in aside, the next automatic
// sign-in is at once for the first rejection in a while, then waits this long
// after the latest rejection, doubling with each one in a row up to the
// maximum. The set-aside files carry the times and the count, so the limit
// survives a supervisor restart (QuarantineNetworkToken).
const operatorAutomaticSignInMinimumDelay = time.Hour
const operatorAutomaticSignInMaximumDelay = 24 * time.Hour

// The supervisor's command line, read by newOperatorSupervisorSettings.
type operatorSupervisorSettings struct {
	// today's provider state directory; each operator's lies below it
	baseStateDir string
	executable   string
	// loaded from --hotkey_seed_file: signs in only with autoRegister
	hotkey       *crv4.Keypair
	autoRegister bool
	// divided evenly over the children; nonpositive passes none
	maxMemory               connect.ByteCount
	verbosity               int
	allowClientRegistration bool
	port                    int
}

// A started child: a real process, or a test's stand-in.
type operatorChildProcess interface {
	Pid() int
	Signal(os.Signal) error
	// Returns once the child has exited.
	Wait() error
}

// This binary, with a child's arguments and environment.
type operatorChildCommand struct {
	path string
	args []string
	env  []string
}

// Tests replace these, so they run no process and wait on no real timer.
type operatorSupervisorHooks struct {
	start  func(operatorChildCommand) (operatorChildProcess, error)
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	signIn func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error)
	// sees each next deadline (zero for none) just before the loop waits
	idle func(time.Time)
}

func defaultOperatorSupervisorHooks() operatorSupervisorHooks {
	return operatorSupervisorHooks{
		start:  startOperatorChild,
		now:    time.Now,
		after:  time.After,
		signIn: hotkeyauth.SignIn,
	}
}

// The refresher, or a test's list.
type operatorListSource interface {
	Snapshot() (*operatorlist.Snapshot, chan struct{})
}

// What a child runs with. A running child whose spec changes restarts.
type operatorChildSpec struct {
	apiUrl     string
	connectUrl string
	// "" passes no --max-memory
	maxMemory string
}

// The loop's record of one operator. It outlives a delisting until the
// operator's child has stopped and no sign-in is pending.
type operatorState struct {
	// the latest listing, kept after delisting
	operator operatorlist.Operator
	listed   bool
	// a jwt was found or written; counts toward the memory share
	credentialed bool
	signingIn    bool
	// while awaiting auth, when the jwt is checked again
	authCheckAt time.Time
	// the automatic sign-in limit last logged, so each change logs once
	signInLimitedUntil time.Time
	child              operatorChildProcess
	spec               operatorChildSpec
	startedAt          time.Time
	stopping           bool
	killAt             time.Time
	killed             bool
	restartAt          time.Time
	// consecutive runs shorter than the maximum backoff
	failures int
}

// From the goroutine that waited on a child.
type operatorChildExit struct {
	domain string
	child  operatorChildProcess
	err    error
}

// From a finished sign-in, after it wrote the jwt.
type operatorSignIn struct {
	domain  string
	created bool
	err     error
}

type operatorSupervisor struct {
	settings     operatorSupervisorSettings
	hooks        operatorSupervisorHooks
	log          io.Writer
	operators    map[string]*operatorState
	listSha256   string
	exits        chan operatorChildExit
	signIns      chan operatorSignIn
	signInCount  int
	shuttingDown bool
	status       atomic.Pointer[operatorSupervisorStatus]
}

func newOperatorSupervisor(settings operatorSupervisorSettings, hooks operatorSupervisorHooks, log io.Writer) *operatorSupervisor {
	self := &operatorSupervisor{
		settings:  settings,
		hooks:     hooks,
		log:       log,
		operators: map[string]*operatorState{},
		exits:     make(chan operatorChildExit),
		// one result can be in flight per listed operator, and the send must
		// never block a shutting-down supervisor: capacity is the operator
		// bound, so every completion drains through the loop's signInCount.
		signIns: make(chan operatorSignIn, operatorlist.MaximumOperators),
	}
	self.status.Store(&operatorSupervisorStatus{Operators: []operatorStatus{}})
	return self
}

// Supervises until a signal or ctx stops it and every child has exited.
func (self *operatorSupervisor) Run(ctx context.Context, list operatorListSource, signals <-chan os.Signal) {
	// sign-ins end with the supervisor
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	snapshot, update := list.Snapshot()
	self.applyList(snapshot)
	done := ctx.Done()
	for {
		now := self.hooks.now()
		next := self.reconcile(requestCtx, now)
		self.publishStatus()
		if self.shuttingDown && self.signInCount == 0 && !self.anyChild() {
			return
		}
		var timeout <-chan time.Time
		if !next.IsZero() {
			timeout = self.hooks.after(next.Sub(now))
		}
		if self.hooks.idle != nil {
			self.hooks.idle(next)
		}
		select {
		case <-update:
			snapshot, update = list.Snapshot()
			self.applyList(snapshot)
		case exit := <-self.exits:
			self.childExited(exit)
		case result := <-self.signIns:
			self.signedIn(result)
		case received := <-signals:
			cancelRequests()
			self.shutdown(received)
		case <-done:
			done = nil
			cancelRequests()
			self.shutdown(syscall.SIGTERM)
		case <-timeout:
		}
	}
}

// Marks which operators are listed and logs what changed.
func (self *operatorSupervisor) applyList(snapshot *operatorlist.Snapshot) {
	if snapshot == nil {
		fmt.Fprintln(self.log, "waiting for the operator list")
		return
	}
	if snapshot.Sha256 == self.listSha256 {
		return
	}
	self.listSha256 = snapshot.Sha256
	source := "fetched"
	if snapshot.Cached {
		source = "cached"
	}
	fmt.Fprintf(self.log, "operator list %s (%s): %d operators\n", snapshot.Sha256, source, len(snapshot.List.Operators))
	listed := map[string]bool{}
	for _, operator := range snapshot.List.Operators {
		listed[operator.Domain] = true
		state, ok := self.operators[operator.Domain]
		switch {
		case !ok:
			state = &operatorState{}
			self.operators[operator.Domain] = state
		case !state.listed:
			fmt.Fprintf(self.log, "operator %s is listed again\n", operator.Domain)
		case state.operator != operator:
			fmt.Fprintf(self.log, "operator %s changed its urls\n", operator.Domain)
		}
		state.operator = operator
		state.listed = true
	}
	for _, domain := range self.domains() {
		if state := self.operators[domain]; state.listed && !listed[domain] {
			state.listed = false
			fmt.Fprintf(self.log, "operator %s was delisted; its state directory is kept\n", domain)
		}
	}
}

// Moves every operator toward what the list and its credentials ask for, and
// returns the earliest deadline still ahead, or zero.
func (self *operatorSupervisor) reconcile(requestCtx context.Context, now time.Time) time.Time {
	domains := self.domains()
	if !self.shuttingDown {
		// credentials first, so the memory share counts every operator that
		// starts in this pass
		for _, domain := range domains {
			state := self.operators[domain]
			if state.listed && state.child == nil && !state.signingIn && !now.Before(state.restartAt) && !now.Before(state.authCheckAt) {
				self.checkCredentials(requestCtx, domain, state, now)
			}
		}
	}
	share := self.memoryShare()
	var next time.Time
	earliest := func(deadline time.Time) {
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	for _, domain := range domains {
		state := self.operators[domain]
		switch {
		case state.child != nil && state.stopping:
			if !state.killed && !now.Before(state.killAt) {
				fmt.Fprintf(self.log, "operator %s: provider (pid %d) did not stop within %s; killing it\n", domain, state.child.Pid(), operatorStopGrace)
				self.signal(domain, state, os.Kill)
				state.killed = true
			}
			if !state.killed {
				earliest(state.killAt)
			}
		case state.child != nil && !state.listed:
			fmt.Fprintf(self.log, "operator %s: stopping its provider (pid %d)\n", domain, state.child.Pid())
			self.stop(domain, state, syscall.SIGTERM, now)
			earliest(state.killAt)
		case state.child != nil && self.childSpec(state, share) != state.spec:
			fmt.Fprintf(self.log, "operator %s: restarting its provider (pid %d) for its new urls or memory share\n", domain, state.child.Pid())
			self.stop(domain, state, syscall.SIGTERM, now)
			earliest(state.killAt)
		case state.child != nil:
		case self.shuttingDown || state.signingIn:
		case !state.listed:
			delete(self.operators, domain)
		case now.Before(state.restartAt):
			earliest(state.restartAt)
		case !state.credentialed:
			earliest(state.authCheckAt)
		default:
			self.start(domain, state, self.childSpec(state, share), now)
			if state.child == nil {
				earliest(state.restartAt)
			}
		}
	}
	return next
}

// Finds the operator's jwt, or signs in for it with --auto-register, or logs
// that the operator awaits auth and schedules the next check.
func (self *operatorSupervisor) checkCredentials(requestCtx context.Context, domain string, state *operatorState, now time.Time) {
	jwtPath := operatorJwtPath(self.settings.baseStateDir, domain)
	if _, err := readOperatorJwt(jwtPath); err == nil {
		state.credentialed = true
		return
	}
	state.credentialed = false
	if !self.settings.autoRegister {
		fmt.Fprintf(self.log, "operator %s is awaiting auth: provider auth --operator=%s\n", domain, domain)
		state.authCheckAt = now.Add(operatorAuthRecheckInterval)
		return
	}
	quarantine, quarantined, err := LatestNetworkTokenQuarantine(jwtPath)
	if err != nil {
		// unknown rejections are never a reason to sign in at once
		fmt.Fprintf(self.log, "operator %s: its set-aside network sign-ins cannot be read: %v; checking again in %s\n", domain, err, operatorAuthRecheckInterval)
		state.authCheckAt = now.Add(operatorAuthRecheckInterval)
		return
	}
	if quarantined {
		if allowedAt := operatorAutomaticSignInAt(quarantine); now.Before(allowedAt) {
			if !state.signInLimitedUntil.Equal(allowedAt) {
				state.signInLimitedUntil = allowedAt
				fmt.Fprintln(self.log, operatorAutomaticSignInLimitedLog(domain, quarantine, allowedAt))
			}
			// a manual sign-in is still noticed at the usual recheck
			state.authCheckAt = now.Add(operatorAuthRecheckInterval)
			if allowedAt.Before(state.authCheckAt) {
				state.authCheckAt = allowedAt
			}
			return
		}
	}
	state.signInLimitedUntil = time.Time{}
	state.signingIn = true
	self.signInCount++
	apiUrl := state.operator.ApiUrl
	if quarantined {
		fmt.Fprintf(self.log, "operator %s: signing in again with the hotkey after %d rejected network sign-ins in a row\n", domain, quarantine.Consecutive)
	} else {
		fmt.Fprintf(self.log, "operator %s: signing in with the hotkey\n", domain)
	}
	go func() {
		result := operatorSignIn{domain: domain}
		network, err := self.hooks.signIn(requestCtx, hotkeyauth.Settings{ApiUrl: apiUrl, Hotkey: self.settings.hotkey})
		if err == nil {
			result.created = network.Created
			err = persistOperatorJwt(jwtPath, network.ByJwt)
		}
		result.err = err
		self.signIns <- result
	}()
}

// A failed sign-in is tried again at the next credential check.
func (self *operatorSupervisor) signedIn(result operatorSignIn) {
	self.signInCount--
	state, ok := self.operators[result.domain]
	if !ok {
		return
	}
	state.signingIn = false
	if result.err != nil {
		if !self.shuttingDown {
			fmt.Fprintf(self.log, "operator %s: hotkey sign-in failed: %v; trying again in %s\n", result.domain, result.err, operatorAuthRecheckInterval)
		}
		state.authCheckAt = self.hooks.now().Add(operatorAuthRecheckInterval)
		return
	}
	state.credentialed = true
	state.authCheckAt = time.Time{}
	if result.created {
		fmt.Fprintf(self.log, "operator %s: signed in with the hotkey and created its network\n", result.domain)
	} else {
		fmt.Fprintf(self.log, "operator %s: signed in with the hotkey\n", result.domain)
	}
}

// Splits --max-memory evenly over the operators that run, or will once signed
// in, in the format --max-memory reads back exactly. A child restarts into a
// new share, so the shares never add up to more than the limit for long.
func (self *operatorSupervisor) memoryShare() string {
	if self.settings.maxMemory <= 0 {
		return ""
	}
	count := 0
	for _, state := range self.operators {
		if state.listed && (state.credentialed || self.settings.autoRegister) {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return formatMemoryShare(max(1, self.settings.maxMemory/connect.ByteCount(count)))
}

// The largest unit that divides the count exactly, else bytes, so
// connect.ParseByteCount reads back the same count.
func formatMemoryShare(byteCount connect.ByteCount) string {
	for _, unit := range []struct {
		suffix    string
		byteCount connect.ByteCount
	}{
		{suffix: "gib", byteCount: 1024 * 1024 * 1024},
		{suffix: "mib", byteCount: 1024 * 1024},
		{suffix: "kib", byteCount: 1024},
	} {
		if byteCount%unit.byteCount == 0 {
			return fmt.Sprintf("%d%s", byteCount/unit.byteCount, unit.suffix)
		}
	}
	return fmt.Sprintf("%db", byteCount)
}

func (self *operatorSupervisor) childSpec(state *operatorState, share string) operatorChildSpec {
	return operatorChildSpec{apiUrl: state.operator.ApiUrl, connectUrl: state.operator.ConnectUrl, maxMemory: share}
}

// A child never gets --all-operators. A new operator directory has no provider
// client yet, so with --allow-client-registration or --auto-register every
// child may register one; the flag never replaces a retained identity.
func (self *operatorSupervisor) childArgs(spec operatorChildSpec) []string {
	args := []string{"provide", "--api_url=" + spec.apiUrl, "--connect_url=" + spec.connectUrl, "--port=0", "--operator-child"}
	if spec.maxMemory != "" {
		args = append(args, "--max-memory="+spec.maxMemory)
	}
	if self.settings.allowClientRegistration || self.settings.autoRegister {
		args = append(args, "--allow-client-registration")
	}
	if self.settings.autoRegister {
		// a rejected network sign-in is set aside and the child exits, so
		// this supervisor signs in again with the hotkey
		args = append(args, "--quarantine-rejected-sign-in")
	}
	if 0 < self.settings.verbosity {
		args = append(args, "-"+strings.Repeat("v", self.settings.verbosity))
	}
	return args
}

// The supervisor's environment with the operator's directory in place of any
// inherited home, so the child's ~/.urnetwork is the operator's own state
// directory. This binary has no URNETWORK_STATE_DIR, so HOME is the only
// switch: it is what os.UserHomeDir reads on unix, and USERPROFILE (with
// HOMEDRIVE/HOMEPATH) is what it reads on windows.
func operatorChildEnv(environ []string, directory string) []string {
	env := make([]string, 0, len(environ)+2)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+directory, "USERPROFILE="+directory)
}

// A child that cannot start counts as one that exited at once.
func (self *operatorSupervisor) start(domain string, state *operatorState, spec operatorChildSpec, now time.Time) {
	directory := operatorDirectory(self.settings.baseStateDir, domain)
	child, err := func() (operatorChildProcess, error) {
		// a child without its proxies would provide from this host directly
		if err := copyOperatorProxyFile(self.settings.baseStateDir, directory); err != nil {
			return nil, fmt.Errorf("proxy file: %w", err)
		}
		return self.hooks.start(operatorChildCommand{
			path: self.settings.executable,
			args: self.childArgs(spec),
			env:  operatorChildEnv(os.Environ(), directory),
		})
	}()
	if err != nil {
		delay := self.backoff(state, 0)
		state.restartAt = now.Add(delay)
		fmt.Fprintf(self.log, "operator %s: provider did not start: %v; trying again in %s\n", domain, err, delay)
		return
	}
	state.child = child
	state.spec = spec
	state.startedAt = now
	state.restartAt = time.Time{}
	fmt.Fprintf(self.log, "operator %s: provider started (pid %d) for %s\n", domain, child.Pid(), spec.apiUrl)
	go func() {
		err := child.Wait()
		self.exits <- operatorChildExit{domain: domain, child: child, err: err}
	}()
}

// The next restart delay. A run as long as the maximum delay starts the
// doubling over.
func (self *operatorSupervisor) backoff(state *operatorState, uptime time.Duration) time.Duration {
	if operatorRestartMaximumDelay <= uptime {
		state.failures = 0
	}
	state.failures++
	return min(operatorRestartMinimumDelay<<min(state.failures-1, 5), operatorRestartMaximumDelay)
}

// A child stopped on purpose starts again at once if still wanted; any other
// exit waits out the backoff.
func (self *operatorSupervisor) childExited(exit operatorChildExit) {
	state, ok := self.operators[exit.domain]
	if !ok || state.child != exit.child {
		return
	}
	state.child = nil
	// The thrash marker is one-shot evidence about THIS exit, so consume it
	// before the cause is judged: a thrashing child usually still has its JWT
	// and takes the restart path below, and a marker left behind must never
	// explain a later child that reuses the pid.
	thrashRestarted := operatorChildThrashRestarted(
		filepath.Join(operatorDirectory(self.settings.baseStateDir, exit.domain), operatorStateSubdirectory),
		exit.child.Pid())
	if state.stopping {
		state.stopping = false
		state.killed = false
		fmt.Fprintf(self.log, "operator %s: provider (pid %d) stopped\n", exit.domain, exit.child.Pid())
		return
	}
	if _, err := os.Stat(operatorJwtPath(self.settings.baseStateDir, exit.domain)); errors.Is(err, os.ErrNotExist) {
		// the child set a rejected network sign-in aside, or the jwt went
		// away: the credential check decides at once, without the crash
		// backoff, and signs in again with --auto-register
		state.credentialed = false
		state.restartAt = time.Time{}
		state.authCheckAt = time.Time{}
		if operatorChildQuarantined(exit.err, thrashRestarted) {
			fmt.Fprintf(self.log, "operator %s: provider (pid %d) set its rejected network sign-in aside; checking its credentials\n", exit.domain, exit.child.Pid())
		} else {
			fmt.Fprintf(self.log, "operator %s: provider (pid %d) exited without a network sign-in; checking its credentials\n", exit.domain, exit.child.Pid())
		}
		return
	}
	now := self.hooks.now()
	delay := self.backoff(state, now.Sub(state.startedAt))
	state.restartAt = now.Add(delay)
	cause := "exited"
	if exit.err != nil {
		cause = fmt.Sprintf("exited: %v", exit.err)
	}
	fmt.Fprintf(self.log, "operator %s: provider (pid %d) %s; restarting in %s\n", exit.domain, exit.child.Pid(), cause, delay)
}

// operatorChildThrashRestarted reports whether this child exited for a planned
// thrash restart, and consumes the evidence. Exit status 75 has two producers:
// the network sign-in quarantine (main.go providerQuarantinedExitCode) and the
// swap-thrash watchdog's supervised restart, which marks its exit for us with a
// one-shot thrash_exit.<pid> file immediately before it exits (thrash_cap.go
// markThrashExit; the liveness self-exit writes the same file).
//
// The pid in the name is the binding, not a timestamp or the record's
// existence: thrash_cap.json is written mid-life too (a liveness stall is
// recorded and can be undone while the child stays up, and a persist that timed
// out can complete late), so nothing about it can explain an exit. A clock step
// or 1s filesystem timestamp granularity would also break an mtime comparison in
// both directions.
//
// The marker is removed on read so it explains exactly one exit: a later child
// that reuses this pid must not inherit it.
func operatorChildThrashRestarted(stateDir string, pid int) bool {
	if stateDir == "" || pid <= 0 {
		return false
	}
	path := filepath.Join(stateDir, thrashExitMarkerName(pid))
	if _, err := os.Stat(path); err != nil {
		return false
	}
	// The stat saw it; a failed unlink only means it may linger, which the
	// next marker write sweeps, so it still explains this exit.
	_ = os.Remove(path)
	return true
}

// operatorChildQuarantined reports a child that exited because it set its
// rejected network sign-in aside (providerQuarantinedExitCode). A 75 that the
// thrash path marked for this pid is a planned memory restart, not a quarantine:
// the child then has no JWT for the ordinary reason that it exited before
// signing in again, and the caller must not claim otherwise.
//
// The distinction only selects the log line: both branches make the same state
// changes, and the automatic sign-in backoff comes from the set-aside files on
// disk, never from this. It tells the two apart for the operator; it does not
// change what the supervisor does.
func operatorChildQuarantined(err error, thrashRestarted bool) bool {
	if thrashRestarted {
		return false
	}
	var exited interface{ ExitCode() int }
	return errors.As(err, &exited) && exited.ExitCode() == providerQuarantinedExitCode
}

// Forwards a stop signal to every child and starts nothing after it.
func (self *operatorSupervisor) shutdown(received os.Signal) {
	if !self.shuttingDown {
		self.shuttingDown = true
		fmt.Fprintf(self.log, "%s: stopping every provider\n", received)
	}
	now := self.hooks.now()
	for _, domain := range self.domains() {
		if state := self.operators[domain]; state.child != nil {
			self.stop(domain, state, received, now)
		}
	}
}

// The first stop arms the kill deadline; later signals only forward.
func (self *operatorSupervisor) stop(domain string, state *operatorState, signal os.Signal, now time.Time) {
	self.signal(domain, state, signal)
	if !state.stopping {
		state.stopping = true
		state.killAt = now.Add(operatorStopGrace)
	}
}

func (self *operatorSupervisor) signal(domain string, state *operatorState, signal os.Signal) {
	// a child that exited before its exit is received is already done
	if err := state.child.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Fprintf(self.log, "operator %s: provider (pid %d) %s: %v\n", domain, state.child.Pid(), signal, err)
	}
}

func (self *operatorSupervisor) domains() []string {
	return slices.Sorted(maps.Keys(self.operators))
}

func (self *operatorSupervisor) anyChild() bool {
	for _, state := range self.operators {
		if state.child != nil {
			return true
		}
	}
	return false
}

// Copies the base proxy file over the operator's through a temporary file and
// a rename, with the base file's mode. The operator's copy is in the child's
// state directory, where the child reads its proxies from. Without a base
// proxy file the operator's directory is left as it is.
func copyOperatorProxyFile(base string, directory string) (returnErr error) {
	source, err := os.Open(filepath.Join(base, "proxy"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(directory, operatorStateSubdirectory)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(stateDir, ".proxy.tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filepath.Join(stateDir, "proxy"))
}

type operatorExecChild struct {
	command *exec.Cmd
}

func (self *operatorExecChild) Pid() int {
	return self.command.Process.Pid
}

func (self *operatorExecChild) Signal(signal os.Signal) error {
	return self.command.Process.Signal(signal)
}

func (self *operatorExecChild) Wait() error {
	return self.command.Wait()
}

// Children write to the supervisor's stdout and stderr directly, so their
// diagnostics reach the same journal unchanged.
func startOperatorChild(command operatorChildCommand) (operatorChildProcess, error) {
	child := exec.Command(command.path, command.args...)
	child.Env = command.env
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return nil, err
	}
	return &operatorExecChild{command: child}, nil
}

// Published after every pass of the loop; never changed once published.
type operatorSupervisorStatus struct {
	OperatorListSha256 string           `json:"operator_list_sha256,omitempty"`
	Operators          []operatorStatus `json:"operators"`
}

type operatorStatus struct {
	Domain string `json:"domain"`
	// running, stopping, restarting, signing in, awaiting auth, delisted or stopped
	State string `json:"state"`
	Pid   int    `json:"pid,omitempty"`
}

func (self *operatorSupervisor) publishStatus() {
	status := &operatorSupervisorStatus{OperatorListSha256: self.listSha256, Operators: []operatorStatus{}}
	for _, domain := range self.domains() {
		state := self.operators[domain]
		entry := operatorStatus{Domain: domain}
		if state.child != nil {
			entry.Pid = state.child.Pid()
		}
		switch {
		case state.child != nil && state.stopping:
			entry.State = "stopping"
		case state.child != nil:
			entry.State = "running"
		case self.shuttingDown:
			entry.State = "stopped"
		case !state.listed:
			entry.State = "delisted"
		case state.signingIn:
			entry.State = "signing in"
		case !state.restartAt.IsZero():
			entry.State = "restarting"
		default:
			entry.State = "awaiting auth"
		}
		status.Operators = append(status.Operators, entry)
	}
	self.status.Store(status)
}

// The process status the single-operator provider serves at --port, with
// each operator's state in place of the provider diagnostics.
func (self *operatorSupervisor) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	status := self.status.Load()
	host, _ := Host()
	responseJson, err := json.Marshal(struct {
		Version string `json:"version,omitempty"`
		Status  string `json:"status"`
		Host    string `json:"host"`
		*operatorSupervisorStatus
	}{
		Version:                  RequireVersion(),
		Status:                   "ok",
		Host:                     host,
		operatorSupervisorStatus: status,
	})
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(responseJson)
}

// Serves the status at --port until the returned server is closed; nil when
// the port is 0.
func (self *operatorSupervisor) serveStatus(port int) (*http.Server, error) {
	if port <= 0 {
		return nil, nil
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: self, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(self.log, "operator status server: %v\n", serveErr)
		}
	}()
	return server, nil
}

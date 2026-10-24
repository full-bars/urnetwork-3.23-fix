package main

// The supervisor runs here on its hooks alone: no process starts and no real
// timer fires. Its log lines and idle points share one ordered journal, so a
// test waits for the loop to settle after exactly the event it caused.
// Ported from the sn miner's operators_supervisor_test.go (upstream
// 4ca8316e) minus the hotkey sign-in, memory-share and status-server parts,
// which the v33 grammar does not carry; the state directory assertions read
// HOME instead of URNETWORK_STATE_DIR.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/hotkeyauth"
	"github.com/urfoundation/sn/operatorlist"
)

// Bounds a wait that only a broken supervisor would reach.
const testOperatorWait = 30 * time.Second

var testOperatorEpoch = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

type testOperatorJournal struct {
	stateLock sync.Mutex
	entries   []string
	partial   string
	update    chan struct{}
}

func (self *testOperatorJournal) addWithLock(entry string) {
	self.entries = append(self.entries, entry)
	close(self.update)
	self.update = make(chan struct{})
}

// The supervisor's log; each complete line is one entry.
func (self *testOperatorJournal) Write(raw []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.partial += string(raw)
	for {
		index := strings.IndexByte(self.partial, '\n')
		if index < 0 {
			return len(raw), nil
		}
		self.addWithLock("log " + self.partial[:index])
		self.partial = self.partial[index+1:]
	}
}

func (self *testOperatorJournal) idle(next time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if next.IsZero() {
		self.addWithLock("idle")
	} else {
		self.addWithLock("idle " + next.Sub(testOperatorEpoch).String())
	}
}

// Waits for the first entry from index on that matches, and returns the
// index after it.
func (self *testOperatorJournal) wait(t *testing.T, from int, want string) int {
	t.Helper()
	return self.waitMatch(t, from, want, func(entry string) bool { return entry == want })
}

func (self *testOperatorJournal) waitPrefix(t *testing.T, from int, prefix string) int {
	t.Helper()
	return self.waitMatch(t, from, prefix+"...", func(entry string) bool { return strings.HasPrefix(entry, prefix) })
}

func (self *testOperatorJournal) waitMatch(t *testing.T, from int, what string, match func(string) bool) int {
	t.Helper()
	timeout := time.After(testOperatorWait)
	for {
		var update chan struct{}
		var entries []string
		found := -1
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for index := from; index < len(self.entries); index++ {
				if match(self.entries[index]) {
					found = index
					break
				}
			}
			update = self.update
			entries = append([]string(nil), self.entries...)
		}()
		if 0 <= found {
			return found + 1
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatalf("no %q after entry %d; journal:\n%s", what, from, strings.Join(entries, "\n"))
		}
	}
}

func (self *testOperatorJournal) length() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.entries)
}

// String is every complete entry, for a test that counts a line.
func (self *testOperatorJournal) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.entries, "\n")
}

type testOperatorClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []testOperatorTimer
}

type testOperatorTimer struct {
	deadline time.Time
	fire     chan time.Time
}

func (self *testOperatorClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testOperatorClock) After(delay time.Duration) <-chan time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	fire := make(chan time.Time, 1)
	if deadline := self.now.Add(delay); deadline.After(self.now) {
		self.timers = append(self.timers, testOperatorTimer{deadline: deadline, fire: fire})
	} else {
		fire <- self.now
	}
	return fire
}

// Moves the clock to the epoch plus offset and fires every timer then due.
func (self *testOperatorClock) set(offset time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = testOperatorEpoch.Add(offset)
	pending := self.timers[:0]
	for _, timer := range self.timers {
		if timer.deadline.After(self.now) {
			pending = append(pending, timer)
		} else {
			timer.fire <- self.now
		}
	}
	self.timers = pending
}

// A list the test publishes, as the refresher would.
type testOperatorList struct {
	value *connect.MonitorValue[*operatorlist.Snapshot]
}

func (self testOperatorList) Snapshot() (*operatorlist.Snapshot, chan struct{}) {
	return self.value.Get()
}

type testOperatorChild struct {
	pid     int
	command operatorChildCommand
	// the operator's proxy file as the child would have read it
	proxy     []byte
	proxyMode os.FileMode
	signals   chan os.Signal
	exit      chan error
}

func (self *testOperatorChild) Pid() int {
	return self.pid
}

func (self *testOperatorChild) Signal(signal os.Signal) error {
	self.signals <- signal
	return nil
}

func (self *testOperatorChild) Wait() error {
	return <-self.exit
}

// The homes the child runs with; the supervisor hands each child exactly one.
func (self *testOperatorChild) homeDirs() []string {
	var directories []string
	for _, entry := range self.command.env {
		if directory, ok := strings.CutPrefix(entry, "HOME="); ok {
			directories = append(directories, directory)
		}
	}
	return directories
}

func (self *testOperatorChild) domain() string {
	return filepath.Base(self.homeDirs()[0])
}

func (self *testOperatorChild) option(name string) string {
	for _, arg := range self.command.args {
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value
		}
	}
	return ""
}

type testOperatorFixture struct {
	t          *testing.T
	base       string
	settings   operatorSupervisorSettings
	clock      *testOperatorClock
	journal    *testOperatorJournal
	list       *connect.MonitorValue[*operatorlist.Snapshot]
	signals    chan os.Signal
	starts     chan *testOperatorChild
	signIn     func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error)
	supervisor *operatorSupervisor
	done       chan struct{}

	stateLock sync.Mutex
	children  []*testOperatorChild
}

func newTestOperatorFixture(t *testing.T) *testOperatorFixture {
	home := operatorTestHome(t)
	self := &testOperatorFixture{
		t:        t,
		base:     filepath.Join(home, ".urnetwork"),
		settings: operatorSupervisorSettings{baseStateDir: filepath.Join(home, ".urnetwork"), executable: "/synthetic/bin/provider"},
		clock:    &testOperatorClock{now: testOperatorEpoch},
		journal:  &testOperatorJournal{update: make(chan struct{})},
		list:     connect.NewMonitorValue[*operatorlist.Snapshot](nil),
		signals:  make(chan os.Signal, 4),
		starts:   make(chan *testOperatorChild, 64),
	}
	return self
}

// Writes the operator's jwt the way the operator would.
func (self *testOperatorFixture) authenticate(domain string) {
	writeOperatorJwt(self.t, self.base, domain, "synthetic-jwt-"+domain)
}

func (self *testOperatorFixture) publish(operators ...operatorlist.Operator) {
	list, err := operatorlist.Parse([]byte(testOperatorListYaml(self.t, operators...)))
	if err != nil {
		self.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprint(list)))
	self.list.Set(&operatorlist.Snapshot{List: list, Sha256: "sha256:" + hex.EncodeToString(sum[:]), FetchedAt: testOperatorEpoch})
}

func (self *testOperatorFixture) start(command operatorChildCommand) (operatorChildProcess, error) {
	child := &testOperatorChild{command: command, signals: make(chan os.Signal, 16), exit: make(chan error, 1)}
	if directories := child.homeDirs(); len(directories) == 1 {
		if info, err := os.Stat(filepath.Join(directories[0], operatorStateSubdirectory, "proxy")); err == nil {
			child.proxyMode = info.Mode().Perm()
			child.proxy, _ = os.ReadFile(filepath.Join(directories[0], operatorStateSubdirectory, "proxy"))
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.children = append(self.children, child)
		child.pid = len(self.children)
	}()
	self.starts <- child
	return child, nil
}

func (self *testOperatorFixture) run() {
	signIn := self.signIn
	if signIn == nil {
		signIn = func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error) {
			return nil, errors.New("synthetic sign-in")
		}
	}
	hooks := operatorSupervisorHooks{
		start:  self.start,
		now:    self.clock.Now,
		after:  self.clock.After,
		signIn: signIn,
		idle:   self.journal.idle,
	}
	self.supervisor = newOperatorSupervisor(self.settings, hooks, self.journal)
	ctx, cancel := context.WithCancel(context.Background())
	self.done = make(chan struct{})
	go func() {
		defer close(self.done)
		self.supervisor.Run(ctx, testOperatorList{value: self.list}, self.signals)
	}()
	// a failed test still joins the supervisor: it stops, and every child exits
	self.t.Cleanup(func() {
		cancel()
		self.stateLock.Lock()
		children := append([]*testOperatorChild(nil), self.children...)
		self.stateLock.Unlock()
		for _, child := range children {
			select {
			case child.exit <- nil:
			default:
			}
		}
		select {
		case <-self.done:
		case <-time.After(testOperatorWait):
			self.t.Error("supervisor did not return")
		}
	})
}

func (self *testOperatorFixture) nextStart() *testOperatorChild {
	self.t.Helper()
	return testOperatorReceive(self.t, self.starts, "a provider start")
}

func (self *testOperatorFixture) noStart() {
	self.t.Helper()
	select {
	case child := <-self.starts:
		self.t.Fatalf("unexpected start of %s: %v", child.domain(), child.command.args)
	default:
	}
}

// Sends a stop signal, expects every child to get it, lets them exit, and
// waits for the supervisor to return.
func (self *testOperatorFixture) stop(signal os.Signal, children ...*testOperatorChild) {
	self.t.Helper()
	self.signals <- signal
	for _, child := range children {
		if received := testOperatorReceive(self.t, child.signals, child.domain()+" stop signal"); received != signal {
			self.t.Fatalf("%s got %v, want %v", child.domain(), received, signal)
		}
		child.exit <- nil
	}
	testOperatorReceive(self.t, self.done, "supervisor return")
	self.noStart()
}

func testOperatorReceive[T any](t *testing.T, channel <-chan T, what string) T {
	t.Helper()
	select {
	case value, ok := <-channel:
		if !ok {
			var zero T
			return zero
		}
		return value
	case <-time.After(testOperatorWait):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func testNoSignal(t *testing.T, child *testOperatorChild) {
	t.Helper()
	select {
	case received := <-child.signals:
		t.Fatalf("%s got unexpected %v", child.domain(), received)
	default:
	}
}

func TestOperatorSupervisorStartsOneProviderPerListedOperator(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	// a network saved with choose_network in the provider state directory
	// must not reach any child
	if err := writeNetworkConfig("https://api.saved.example", "wss://connect.saved.example"); err != nil {
		t.Fatal(err)
	}
	// nor must an inherited home
	t.Setenv("HOME", "/synthetic/inherited")
	fixture.authenticate("alpha.example")
	fixture.authenticate("beta.example")
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	fixture.run()
	alpha := fixture.nextStart()
	beta := fixture.nextStart()
	for _, child := range []*testOperatorChild{alpha, beta} {
		domain := child.domain()
		want := []string{"provide", "--api_url=https://api." + domain, "--connect_url=wss://connect." + domain, "--port=0", "--operator-child"}
		if child.command.path != "/synthetic/bin/provider" || !reflect.DeepEqual(child.command.args, want) {
			t.Fatalf("%s child = %s %v, want %v", domain, child.command.path, child.command.args, want)
		}
		if homes := child.homeDirs(); !reflect.DeepEqual(homes, []string{operatorDirectory(fixture.base, domain)}) {
			t.Fatalf("%s homes = %v", domain, homes)
		}
	}
	if alpha.domain() != "alpha.example" || beta.domain() != "beta.example" {
		t.Fatalf("started %s and %s", alpha.domain(), beta.domain())
	}
	fixture.stop(syscall.SIGTERM, alpha, beta)
}

func TestOperatorSupervisorPassesVerbosity(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.settings.verbosity = 2
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()
	want := []string{"provide", "--api_url=https://api.alpha.example", "--connect_url=wss://connect.alpha.example", "--port=0", "--operator-child", "-vv"}
	if !reflect.DeepEqual(alpha.command.args, want) {
		t.Fatalf("child args = %v, want %v", alpha.command.args, want)
	}
	fixture.stop(syscall.SIGTERM, alpha)
}

func TestOperatorSupervisorStopsADelistedOperatorAndKeepsItsState(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.authenticate("beta.example")
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	fixture.run()
	alpha := fixture.nextStart()
	beta := fixture.nextStart()

	from := fixture.journal.length()
	fixture.publish(testListedOperator("alpha.example"))
	if received := testOperatorReceive(t, beta.signals, "delisted stop"); received != syscall.SIGTERM {
		t.Fatalf("delisted child got %v", received)
	}
	at := fixture.journal.wait(t, from, "idle 1m0s")
	// the grace period has not passed
	testNoSignal(t, beta)
	fixture.clock.set(operatorStopGrace)
	if received := testOperatorReceive(t, beta.signals, "delisted kill"); received != os.Kill {
		t.Fatalf("delisted child got %v after the grace period", received)
	}
	beta.exit <- nil
	fixture.journal.wait(t, at, "log operator beta.example: provider (pid 2) stopped")
	if _, err := readOperatorJwt(operatorJwtPath(fixture.base, "beta.example")); err != nil {
		t.Fatalf("delisted operator lost its state: %v", err)
	}
	testNoSignal(t, alpha)
	fixture.noStart()
	fixture.stop(syscall.SIGTERM, alpha)
}

func TestOperatorSupervisorRestartsAProviderWhoseUrlsChanged(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()

	moved := testListedOperator("alpha.example")
	moved.ApiUrl = "https://api-two.alpha.example"
	fixture.publish(moved)
	if received := testOperatorReceive(t, alpha.signals, "url change stop"); received != syscall.SIGTERM {
		t.Fatalf("changed child got %v", received)
	}
	alpha.exit <- nil
	// a deliberate restart waits for no backoff
	restarted := fixture.nextStart()
	if restarted.option("--api_url") != "https://api-two.alpha.example" || restarted.option("--connect_url") != "wss://connect.alpha.example" {
		t.Fatalf("restarted child args = %v", restarted.command.args)
	}
	fixture.stop(syscall.SIGTERM, restarted)
}

func TestOperatorSupervisorRestartsAnExitedProviderWithBackoff(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	child := fixture.nextStart()
	var offset time.Duration
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		from := fixture.journal.length()
		child.exit <- errors.New("exit status 1")
		fixture.journal.wait(t, from, "idle "+(offset+delay).String())
		fixture.noStart()
		offset += delay
		fixture.clock.set(offset)
		child = fixture.nextStart()
	}
	// a run as long as the cap starts the doubling over
	offset += operatorRestartMaximumDelay
	fixture.clock.set(offset)
	from := fixture.journal.length()
	child.exit <- nil
	fixture.journal.wait(t, from, "idle "+(offset+30*time.Second).String())
	fixture.noStart()
	fixture.clock.set(offset + 30*time.Second)
	child = fixture.nextStart()
	fixture.stop(syscall.SIGTERM, child)
}

func TestOperatorSupervisorForwardsStopSignalsAndWaitsForEveryChild(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.authenticate("beta.example")
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	fixture.run()
	alpha := fixture.nextStart()
	beta := fixture.nextStart()

	fixture.signals <- syscall.SIGINT
	for _, child := range []*testOperatorChild{alpha, beta} {
		if received := testOperatorReceive(t, child.signals, child.domain()+" interrupt"); received != syscall.SIGINT {
			t.Fatalf("%s got %v", child.domain(), received)
		}
	}
	from := fixture.journal.length()
	alpha.exit <- nil
	at := fixture.journal.wait(t, from, "log operator alpha.example: provider (pid 1) stopped")
	fixture.journal.wait(t, at, "idle 1m0s")
	select {
	case <-fixture.done:
		t.Fatal("supervisor returned before every child exited")
	default:
	}
	// a second signal is forwarded too, and the grace period still ends in a kill
	from = fixture.journal.length()
	fixture.signals <- syscall.SIGTERM
	if received := testOperatorReceive(t, beta.signals, "second signal"); received != syscall.SIGTERM {
		t.Fatalf("second signal reached beta as %v", received)
	}
	fixture.journal.wait(t, from, "idle 1m0s")
	fixture.clock.set(operatorStopGrace)
	if received := testOperatorReceive(t, beta.signals, "shutdown kill"); received != os.Kill {
		t.Fatalf("beta got %v after the grace period", received)
	}
	beta.exit <- nil
	testOperatorReceive(t, fixture.done, "supervisor return")
	fixture.noStart()
}

func TestOperatorSupervisorCopiesTheProxyFileBeforeStarting(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	proxy := []byte(`{"auths":null,"servers":{"proxy.example:1080":""}}`)
	proxyPath := filepath.Join(fixture.base, "proxy")
	if err := os.MkdirAll(fixture.base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyPath, proxy, 0640); err != nil {
		t.Fatal(err)
	}
	// independent of the test's umask
	if err := os.Chmod(proxyPath, 0640); err != nil {
		t.Fatal(err)
	}
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()
	if string(alpha.proxy) != string(proxy) || alpha.proxyMode != 0640 {
		t.Fatalf("child started with proxy %q mode %v", alpha.proxy, alpha.proxyMode)
	}
	entries, err := os.ReadDir(filepath.Join(operatorDirectory(fixture.base, "alpha.example"), operatorStateSubdirectory))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".proxy.tmp") {
			t.Fatalf("temporary proxy file left behind: %s", entry.Name())
		}
	}
	fixture.stop(syscall.SIGTERM, alpha)

	// without a base proxy file an operator's own is left alone
	empty := t.TempDir()
	directory := filepath.Join(empty, "operators", "alpha.example")
	if err := copyOperatorProxyFile(empty, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, operatorStateSubdirectory, "proxy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proxy file appeared without a base copy: %v", err)
	}
}

func TestOperatorSupervisorAwaitsAuthThenStartsTheOperator(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	fixture.run()
	// one operator awaiting auth never holds back another
	alpha := fixture.nextStart()
	awaiting := "log operator beta.example is awaiting auth: provider auth --operator=beta.example"
	at := fixture.journal.wait(t, 0, awaiting)
	at = fixture.journal.wait(t, at, "idle 5m0s")
	fixture.noStart()

	fixture.clock.set(operatorAuthRecheckInterval)
	at = fixture.journal.wait(t, at, awaiting)
	at = fixture.journal.wait(t, at, "idle 10m0s")
	fixture.noStart()

	fixture.authenticate("beta.example")
	fixture.clock.set(2 * operatorAuthRecheckInterval)
	beta := fixture.nextStart()
	if beta.domain() != "beta.example" {
		t.Fatalf("started %s after auth", beta.domain())
	}
	testNoSignal(t, alpha)
	fixture.stop(syscall.SIGTERM, alpha, beta)
}

// A child that exits after its jwt went away is rechecked at once, without
// the crash backoff, and starts again as soon as a jwt is written.
func TestOperatorSupervisorRechecksACredentialWhenTheJwtGoesAway(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()

	if err := os.Remove(operatorJwtPath(fixture.base, "alpha.example")); err != nil {
		t.Fatal(err)
	}
	from := fixture.journal.length()
	alpha.exit <- errors.New("exit status 1")
	at := fixture.journal.wait(t, from, "log operator alpha.example: provider (pid 1) exited without a network sign-in; checking its credentials")
	at = fixture.journal.wait(t, at, "log operator alpha.example is awaiting auth: provider auth --operator=alpha.example")
	fixture.journal.wait(t, at, "idle 5m0s")
	fixture.noStart()

	fixture.authenticate("alpha.example")
	fixture.clock.set(operatorAuthRecheckInterval)
	restarted := fixture.nextStart()
	fixture.stop(syscall.SIGTERM, restarted)
}

// A fresh operator directory has no provider client, so auto mode lets every
// child register one even when the command line did not.
func TestOperatorSupervisorAutoRegisterAllowsClientRegistration(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.settings.autoRegister = true
	fixture.settings.hotkey = testOperatorHotkey(t, 3)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()
	if !slices.Contains(alpha.command.args, "--allow-client-registration") || !slices.Contains(alpha.command.args, "--quarantine-rejected-sign-in") {
		t.Fatalf("auto mode child args = %v", alpha.command.args)
	}
	fixture.stop(syscall.SIGTERM, alpha)
}

func TestOperatorSupervisorPassesClientRegistration(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.settings.allowClientRegistration = true
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	alpha := fixture.nextStart()
	if !slices.Contains(alpha.command.args, "--allow-client-registration") || slices.Contains(alpha.command.args, "--quarantine-rejected-sign-in") {
		t.Fatalf("child args = %v", alpha.command.args)
	}
	fixture.stop(syscall.SIGTERM, alpha)
}

func TestOperatorSupervisorSharesMaxMemoryOverItsProviders(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.settings.maxMemory = 3 * 1024 * 1024 * 1024
	domains := []string{"alpha.example", "beta.example", "gamma.example"}
	for _, domain := range domains {
		fixture.authenticate(domain)
	}
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"), testListedOperator("gamma.example"))
	fixture.run()
	var children []*testOperatorChild
	for range domains {
		child := fixture.nextStart()
		if child.option("--max-memory") != "1gib" {
			t.Fatalf("%s share = %v", child.domain(), child.command.args)
		}
		children = append(children, child)
	}

	// gamma leaves; the two left restart into half each
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	for _, child := range children {
		if received := testOperatorReceive(t, child.signals, child.domain()+" stop"); received != syscall.SIGTERM {
			t.Fatalf("%s got %v", child.domain(), received)
		}
		child.exit <- nil
	}
	alpha := fixture.nextStart()
	beta := fixture.nextStart()
	for _, child := range []*testOperatorChild{alpha, beta} {
		if child.option("--max-memory") != "1536mib" {
			t.Fatalf("%s share after delisting = %v", child.domain(), child.command.args)
		}
	}
	fixture.stop(syscall.SIGTERM, alpha, beta)
}

func TestFormatMemoryShareReadsBackExactly(t *testing.T) {
	for _, c := range []struct {
		byteCount connect.ByteCount
		want      string
	}{
		{byteCount: 1024 * 1024 * 1024, want: "1gib"},
		{byteCount: 1536 * 1024 * 1024, want: "1536mib"},
		{byteCount: 3 * 1024, want: "3kib"},
		{byteCount: 1024 * 1024 * 1024 / 3, want: "357913941b"},
		{byteCount: 1, want: "1b"},
	} {
		formatted := formatMemoryShare(c.byteCount)
		parsed, err := connect.ParseByteCount(formatted)
		if formatted != c.want || err != nil || parsed != c.byteCount {
			t.Errorf("%d formats as %q and reads back as %d, %v; want %q", c.byteCount, formatted, parsed, err, c.want)
		}
	}
}

func testOperatorHotkey(t *testing.T, fill byte) *crv4.Keypair {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = fill
	}
	var fixed [32]byte
	copy(fixed[:], seed)
	hotkey, err := crv4.KeypairFromSeed(fixed)
	if err != nil {
		t.Fatal(err)
	}
	return hotkey
}

func TestOperatorSupervisorAutoRegisterWritesTheJwt(t *testing.T) {
	hotkey := testOperatorHotkey(t, 5)
	fixture := newTestOperatorFixture(t)
	fixture.settings.autoRegister = true
	fixture.settings.hotkey = hotkey
	fixture.signIn = func(_ context.Context, settings hotkeyauth.Settings) (*hotkeyauth.Network, error) {
		if settings.Hotkey != hotkey || settings.ApiUrl != "https://api.local.example" {
			t.Errorf("sign-in settings = %+v", settings)
		}
		return &hotkeyauth.Network{ByJwt: "jwt:created", Created: true}, nil
	}
	fixture.publish(testListedOperator("local.example"))
	fixture.run()
	at := fixture.journal.wait(t, 0, "log operator local.example: signing in with the hotkey")
	fixture.journal.wait(t, at, "log operator local.example: signed in with the hotkey and created its network")
	child := fixture.nextStart()
	if child.domain() != "local.example" || !slices.Contains(child.command.args, "--allow-client-registration") {
		t.Fatalf("auto-registered child = %v", child.command.args)
	}
	if jwt, err := readOperatorJwt(operatorJwtPath(fixture.base, "local.example")); err != nil || jwt != "jwt:created" {
		t.Fatalf("operator jwt = %q, %v", jwt, err)
	}
	fixture.stop(syscall.SIGTERM, child)
}

func TestOperatorSupervisorStatusShowsEachOperator(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"), testListedOperator("beta.example"))
	fixture.run()
	alpha := fixture.nextStart()
	// published before the loop waits on beta's recheck
	fixture.journal.wait(t, 0, "idle 5m0s")
	recorder := httptest.NewRecorder()
	fixture.supervisor.ServeHTTP(recorder, httptest.NewRequest("GET", "/", nil))
	var status struct {
		Status    string           `json:"status"`
		Operators []operatorStatus `json:"operators"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	want := []operatorStatus{{Domain: "alpha.example", State: "running", Pid: 1}, {Domain: "beta.example", State: "awaiting auth"}}
	if status.Status != "ok" || !reflect.DeepEqual(status.Operators, want) {
		t.Fatalf("status = %s", recorder.Body.String())
	}
	fixture.stop(syscall.SIGTERM, alpha)
}

// The exit of a child that set its rejected network sign-in aside.
type testOperatorExit struct{ code int }

func (self testOperatorExit) Error() string { return fmt.Sprintf("exit status %d", self.code) }

func (self testOperatorExit) ExitCode() int { return self.code }

// Sets the operator's network sign-in aside as its child does when the
// registration call rejects it.
func testOperatorQuarantine(t *testing.T, base string, domain string, at time.Time) NetworkTokenQuarantine {
	t.Helper()
	jwtPath := operatorJwtPath(base, domain)
	token, err := readOperatorJwt(jwtPath)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, ok, err := QuarantineNetworkToken(jwtPath, token, at)
	if err != nil || !ok {
		t.Fatalf("quarantine: ok=%t err=%v", ok, err)
	}
	return quarantine
}

// A child that set its rejected sign-in aside is followed at once, without
// the crash backoff, by an automatic sign-in with the hotkey and a new child.
func TestOperatorSupervisorSignsInAgainAfterAQuarantine(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.settings.autoRegister = true
	fixture.settings.hotkey = testOperatorHotkey(t, 7)
	var signIns atomic.Int64
	fixture.signIn = func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error) {
		signIns.Add(1)
		return &hotkeyauth.Network{ByJwt: "jwt:after-quarantine"}, nil
	}
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	first := fixture.nextStart()
	if !slices.Contains(first.command.args, "--quarantine-rejected-sign-in") {
		t.Fatalf("auto mode child args = %v", first.command.args)
	}
	testOperatorQuarantine(t, fixture.base, "alpha.example", testOperatorEpoch)
	from := fixture.journal.length()
	first.exit <- testOperatorExit{code: providerQuarantinedExitCode}
	at := fixture.journal.wait(t, from, "log operator alpha.example: provider (pid 1) set its rejected network sign-in aside; checking its credentials")
	at = fixture.journal.wait(t, at, "log operator alpha.example: signing in again with the hotkey after 1 rejected network sign-ins in a row")
	fixture.journal.wait(t, at, "log operator alpha.example: signed in with the hotkey")
	second := fixture.nextStart()
	if signIns.Load() != 1 {
		t.Fatalf("sign-ins = %d", signIns.Load())
	}
	if jwt, err := readOperatorJwt(operatorJwtPath(fixture.base, "alpha.example")); err != nil || jwt != "jwt:after-quarantine" {
		t.Fatalf("operator jwt = %q, %v", jwt, err)
	}
	fixture.stop(syscall.SIGTERM, second)
}

// A child that exited 75 after marking its planned thrash restart is not
// reported as a network-sign-in quarantine, even though the JWT is gone for the
// ordinary reason that the child stopped.
func TestOperatorSupervisorDoesNotReportAThrashRestartAsAQuarantine(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	first := fixture.nextStart()
	if err := os.Remove(operatorJwtPath(fixture.base, "alpha.example")); err != nil {
		t.Fatal(err)
	}
	// The thrash path writes the one-shot marker for its own pid immediately
	// before the exit; the supervisor's synthetic child is pid 1.
	marker := filepath.Join(operatorDirectory(fixture.base, "alpha.example"), operatorStateSubdirectory, thrashExitMarkerName(first.pid))
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("pid 1 exited for a planned thrash restart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	from := fixture.journal.length()
	first.exit <- testOperatorExit{code: providerQuarantinedExitCode}
	fixture.journal.wait(t, from, "log operator alpha.example: provider (pid 1) exited without a network sign-in; checking its credentials")
	if journal := fixture.journal.String(); strings.Contains(journal, "set its rejected network sign-in aside") {
		t.Fatalf("a thrash restart was reported as a quarantine:\n%s", journal)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the thrash exit marker was not consumed: %v", err)
	}
	fixture.stop(syscall.SIGTERM)
}

// The masking case the mtime heuristic got wrong: a thrash record written while
// the child was still running (the liveness watchdog's record/undo, or a bounded
// persist that completed late) is not evidence about this exit, so a real
// quarantine after it is still reported as one.
func TestOperatorSupervisorDoesNotMaskAQuarantineWithAMidLifeThrashRecord(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	fixture.authenticate("alpha.example")
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	first := fixture.nextStart()
	// Mid-life: the record is written without any exit, so its mtime falls
	// inside the child's life (startedAt is the fixture's pinned clock).
	record := filepath.Join(operatorDirectory(fixture.base, "alpha.example"), operatorStateSubdirectory, thrashCapFileName)
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte(`{"cap":10,"restarts":[1]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	itsLife := testOperatorEpoch.Add(time.Minute)
	if err := os.Chtimes(record, itsLife, itsLife); err != nil {
		t.Fatal(err)
	}
	// The real sign-in quarantine: the JWT is set aside and the child exits 75
	// without ever marking a thrash restart.
	testOperatorQuarantine(t, fixture.base, "alpha.example", testOperatorEpoch)
	from := fixture.journal.length()
	first.exit <- testOperatorExit{code: providerQuarantinedExitCode}
	fixture.journal.wait(t, from, "log operator alpha.example: provider (pid 1) set its rejected network sign-in aside; checking its credentials")
	if journal := fixture.journal.String(); strings.Contains(journal, "exited without a network sign-in") {
		t.Fatalf("a mid-life thrash record masked a real quarantine:\n%s", journal)
	}
	fixture.stop(syscall.SIGTERM)
}

// Rejections in a row limit automatic sign-ins: the limit is logged once per
// change with the manual command, a restarted supervisor reads the same limit
// from the set-aside files, and the sign-in runs when the limit ends.
func TestOperatorSupervisorLimitsAutomaticSignInsAfterRejectionsInARow(t *testing.T) {
	fixture := newTestOperatorFixture(t)
	jwtPath := operatorJwtPath(fixture.base, "alpha.example")
	for i, at := range []time.Time{testOperatorEpoch.Add(-30 * time.Minute), testOperatorEpoch.Add(-5 * time.Minute)} {
		token := fmt.Sprintf("rejected-%d", i)
		if err := persistOperatorJwt(jwtPath, token); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := QuarantineNetworkToken(jwtPath, token, at); err != nil || !ok {
			t.Fatalf("quarantine %d: ok=%t err=%v", i, ok, err)
		}
	}
	allowedAt := testOperatorEpoch.Add(55 * time.Minute)
	limited := "log operator alpha.example: automatic sign-in is limited after 2 rejected network sign-ins in a row; the next is at " + allowedAt.UTC().Format(time.RFC3339) + ". Run `provider auth --operator=alpha.example` to sign in now"
	var signIns atomic.Int64
	signIn := func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error) {
		signIns.Add(1)
		return &hotkeyauth.Network{ByJwt: "jwt:after-limit"}, nil
	}
	fixture.settings.autoRegister = true
	fixture.settings.hotkey = testOperatorHotkey(t, 9)
	fixture.signIn = signIn
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	at := fixture.journal.wait(t, 0, limited)
	at = fixture.journal.wait(t, at, "idle 5m0s")
	fixture.clock.set(operatorAuthRecheckInterval)
	at = fixture.journal.wait(t, at, "idle 10m0s")
	if count := strings.Count(fixture.journal.String(), "automatic sign-in is limited"); count != 1 || signIns.Load() != 0 {
		t.Fatalf("limit logged %d times, %d sign-ins", count, signIns.Load())
	}
	fixture.stop(syscall.SIGTERM)

	restarted := newTestOperatorFixture(t)
	restarted.base = fixture.base
	restarted.settings = fixture.settings
	restarted.signIn = signIn
	restarted.clock.set(10 * time.Minute)
	restarted.publish(testListedOperator("alpha.example"))
	restarted.run()
	at = restarted.journal.wait(t, 0, limited)
	at = restarted.journal.wait(t, at, "idle 15m0s")
	restarted.noStart()
	restarted.clock.set(55 * time.Minute)
	at = restarted.journal.wait(t, at, "log operator alpha.example: signing in again with the hotkey after 2 rejected network sign-ins in a row")
	restarted.journal.wait(t, at, "log operator alpha.example: signed in with the hotkey")
	child := restarted.nextStart()
	if signIns.Load() != 1 {
		t.Fatalf("sign-ins = %d", signIns.Load())
	}
	restarted.stop(syscall.SIGTERM, child)
}

func TestOperatorAutomaticSignInDoublesUpToADay(t *testing.T) {
	for consecutive, want := range map[int]time.Duration{1: 0, 2: time.Hour, 3: 2 * time.Hour, 4: 4 * time.Hour, 6: 16 * time.Hour, 7: 24 * time.Hour, 40: 24 * time.Hour} {
		quarantine := NetworkTokenQuarantine{Time: testOperatorEpoch, Consecutive: consecutive}
		if got := operatorAutomaticSignInAt(quarantine).Sub(testOperatorEpoch); got != want {
			t.Errorf("%d in a row: wait %s, want %s", consecutive, got, want)
		}
	}
}

// A real child process's exit status carries the quarantine.
func TestOperatorChildQuarantinedReadsTheExitStatus(t *testing.T) {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", providerQuarantinedExitCode)).Run()
	if !operatorChildQuarantined(err, false) {
		t.Fatalf("exit %d was not a quarantine: %v", providerQuarantinedExitCode, err)
	}
	for _, other := range []error{nil, errors.New("exit status 75"), exec.Command("sh", "-c", "exit 1").Run()} {
		if operatorChildQuarantined(other, false) {
			t.Fatalf("%v read as a quarantine", other)
		}
	}
}

// TestOperatorChildQuarantinedDoesNotClaimAThrashRestart pins the shared exit
// code: 75 with this process's one-shot exit marker is a planned memory
// restart, while a 75 with no marker (or only another process's) remains a
// quarantine. The thrash record is deliberately present and recent: it is
// written mid-life, so it must not explain an exit.
func TestOperatorChildQuarantinedDoesNotClaimAThrashRestart(t *testing.T) {
	exit75 := exec.Command("sh", "-c", fmt.Sprintf("exit %d", providerQuarantinedExitCode)).Run()
	const pid = 4242

	stateDir := t.TempDir()
	// A mid-life persist: the liveness watchdog writes thrash_cap.json (and its
	// undo writes it again) while the child is still running, and a bounded
	// persist that timed out can complete after the escalation moved on. Its
	// mtime is inside the child's life and still says nothing about the exit.
	record := filepath.Join(stateDir, thrashCapFileName)
	if err := os.WriteFile(record, []byte(`{"cap":10,"restarts":[1]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	itsLife := testOperatorEpoch.Add(time.Minute)
	if err := os.Chtimes(record, itsLife, itsLife); err != nil {
		t.Fatal(err)
	}
	if operatorChildThrashRestarted(stateDir, pid) {
		t.Fatal("a thrash record written mid-life was read as this child's exit marker")
	}
	if !operatorChildQuarantined(exit75, false) {
		t.Fatalf("exit %d after only a mid-life record was not read as a quarantine", providerQuarantinedExitCode)
	}

	// The marker the exiting process writes binds the exit to that process.
	marker := filepath.Join(stateDir, thrashExitMarkerName(pid))
	if err := os.WriteFile(marker, []byte("pid 4242 exited for a planned thrash restart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !operatorChildThrashRestarted(stateDir, pid) {
		t.Fatal("the exit marker was not read as a thrash restart")
	}
	if operatorChildQuarantined(exit75, true) {
		t.Fatalf("exit %d with the thrash marker was reported as a quarantine", providerQuarantinedExitCode)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the exit marker was not consumed: %v", err)
	}
	if operatorChildThrashRestarted(stateDir, pid) {
		t.Fatal("the exit marker explained more than the one exit it names")
	}

	// Another process's marker is not this child's evidence.
	if err := os.WriteFile(filepath.Join(stateDir, thrashExitMarkerName(pid+1)), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if operatorChildThrashRestarted(stateDir, pid) {
		t.Fatal("another process's exit marker was read as this child's")
	}
	if !operatorChildQuarantined(exit75, false) {
		t.Fatalf("exit %d with another process's marker was not a quarantine", providerQuarantinedExitCode)
	}
}

// The writer and the reader agree on the marker: markThrashExit names this
// process, and nothing else reads as this process's exit.
func TestMarkThrashExitNamesTheExitingProcess(t *testing.T) {
	home := withTempHome(t)
	if err := markThrashExit(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, ".urnetwork")
	if _, err := os.Stat(filepath.Join(stateDir, thrashExitMarkerName(os.Getpid()))); err != nil {
		t.Fatalf("markThrashExit did not write this process's marker: %v", err)
	}
	if operatorChildThrashRestarted(stateDir, os.Getpid()+1) {
		t.Fatal("the marker was read for a process that did not write it")
	}
	if !operatorChildThrashRestarted(stateDir, os.Getpid()) {
		t.Fatal("the supervisor does not read the marker markThrashExit writes")
	}
	if err := markThrashExit(); err != nil {
		t.Fatal(err)
	}
	if !operatorChildThrashRestarted(stateDir, os.Getpid()) {
		t.Fatal("a second exit marker was not written")
	}
}

// Under systemd or a container start script no supervisor consumes the marker,
// so the next exit clears the ones earlier lives left behind instead of
// collecting a file per restart forever.
func TestMarkThrashExitClearsEarlierMarkers(t *testing.T) {
	home := withTempHome(t)
	stateDir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(stateDir, thrashExitMarkerName(999999))
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := markThrashExit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a marker from an earlier life survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, thrashExitMarkerName(os.Getpid()))); err != nil {
		t.Fatalf("the current marker is missing: %v", err)
	}
}

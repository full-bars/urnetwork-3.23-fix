package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The activation post must go direct: the platform publishes the address it
// sees, so a post that crossed an extender or a proxy would list the wrong host.
func TestExtenderActivationStrategyUsesNoExtender(t *testing.T) {
	settings := extenderActivationStrategySettings()
	if len(settings.ExtenderNetworks) != 0 || len(settings.ExtenderHostnames) != 0 {
		t.Fatalf("strategy carries extender inputs: networks=%v hostnames=%v", settings.ExtenderNetworks, settings.ExtenderHostnames)
	}
	if settings.MaxExtenderCount != 0 || settings.ExpandExtenderProfileCount != 0 {
		t.Fatalf("strategy may grow extenders: max=%d expand=%d", settings.MaxExtenderCount, settings.ExpandExtenderProfileCount)
	}
	if !settings.DisableManualExtenders {
		t.Fatal("manual extenders are not disabled")
	}
	if !settings.DisableVless {
		t.Fatal("vless is not disabled: it is a proxying strategy")
	}
}

// A token the watcher renewed must be the one the next activation sends.
func TestJwtHolderFollowsRenewal(t *testing.T) {
	h := newJwtHolder("first")
	if got := h.Get(); got != "first" {
		t.Fatalf("Get = %q, want first", got)
	}
	h.Set("renewed")
	if got := h.Get(); got != "renewed" {
		t.Fatalf("after renewal Get = %q, want renewed", got)
	}
	var zero jwtHolder
	if got := zero.Get(); got != "" {
		t.Fatalf("zero holder Get = %q, want empty", got)
	}
}

// The pacing must survive a restart: it is what stops a rebooted fleet posting
// every family at once.
func TestExtenderPacingFileStoreRoundTrip(t *testing.T) {
	store := extenderPacingFileStore{path: filepath.Join(t.TempDir(), extenderPacingFileName)}
	record, err := store.LoadPacing()
	if err != nil || record != nil {
		t.Fatalf("a missing file must load as (nil, nil), got (%v, %v)", record, err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := &connect.ExtenderActivationPacingRecord{
		Families: map[int]connect.ExtenderActivationPacing{
			4: {LastAttemptAt: now, NextAllowedAttemptAt: now.Add(time.Hour)},
		},
	}
	if err := store.SavePacing(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadPacing()
	if err != nil || got == nil {
		t.Fatalf("load: (%v, %v)", got, err)
	}
	if !got.Families[4].NextAllowedAttemptAt.Equal(want.Families[4].NextAllowedAttemptAt) {
		t.Fatalf("next attempt = %v, want %v", got.Families[4].NextAllowedAttemptAt, want.Families[4].NextAllowedAttemptAt)
	}
}

func TestExtenderPacingFileStoreRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), extenderPacingFileName)
	if err := atomicWriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := (extenderPacingFileStore{path: path}).LoadPacing()
	if err == nil || record != nil {
		t.Fatalf("a corrupt file must be an error so the loop starts as a new process, got (%v, %v)", record, err)
	}
}

// The loop's own outcome lines reach the disk events log; its five-minute
// reminder (any family, including the shared-post "IPv0" wording) goes through
// the ordinary log only, which still reaches the RAM log and the bounded
// important log but not the events log, so a long failure cannot fill it.
func TestExtenderActivatorLoggerRouting(t *testing.T) {
	var mu sync.Mutex
	var important []string
	importantLogHook = func(line string) {
		mu.Lock()
		important = append(important, line)
		mu.Unlock()
	}
	t.Cleanup(func() { importantLogHook = nil })

	log := extenderActivatorLogger{}
	log.Infof("🌉✅ [extender] IPv4 is now activated on try %d.\n", 1)
	log.Infof("🌉 [extender] IPv4 ❌  still failing\n")
	log.Infof("🌉 [extender] IPv6 ⛔  still refused\n")
	log.Infof("🌉 [extender] IPv0 ⏳  nothing tried yet\n")
	log.Errorf("[extender]activate boom\n")

	mu.Lock()
	defer mu.Unlock()
	if len(important) != 2 {
		t.Fatalf("important lines = %d (%q), want 2: the outcome and the error, not the reminder", len(important), important)
	}
	if !strings.Contains(important[0], "now activated on try 1") {
		t.Errorf("first important line = %q", important[0])
	}
}

func TestExtenderManagerFollowsSettingAndDirect(t *testing.T) {
	withTempHome(t)
	prev := globalControlState
	state := newControlState()
	globalControlState = state
	t.Cleanup(func() { globalControlState = prev })

	var lines []string
	var mu sync.Mutex
	importantLogHook = func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}
	t.Cleanup(func() { importantLogHook = nil })

	direct := true
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return direct }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx

	// on by default, direct on: the role runs
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role is not running although the extender is on by default and direct is enabled")
	}
	waitExtenderRoleReady(t, role)

	// `extender off` stops it and says why
	if err := state.set(extenderControlKey, "off"); err != nil {
		t.Fatal(err)
	}
	m.Reconcile("test")
	if m.Role() != nil {
		t.Fatal("role still running after `extender off`")
	}
	if got := m.OffReason(); !strings.Contains(got, "urnet-tools extender on") {
		t.Fatalf("off reason %q does not tell the operator how to turn it back on", got)
	}

	// `extender on` starts it again
	if err := state.set(extenderControlKey, "on"); err != nil {
		t.Fatal(err)
	}
	m.Reconcile("test")
	if m.Role() == nil {
		t.Fatal("role did not restart after `extender on`")
	}

	// direct disabled stops it, with the reason named
	direct = false
	m.Reconcile("test")
	if m.Role() != nil {
		t.Fatal("role still running with direct providing disabled")
	}
	if got := m.OffReason(); !strings.Contains(got, "direct") {
		t.Fatalf("off reason %q does not mention direct providing", got)
	}

	m.stop("test over")
	mu.Lock()
	joined := strings.Join(lines, "")
	mu.Unlock()
	for _, want := range []string{"started", "stopping"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log never said %q:\n%s", want, joined)
		}
	}
}

// The activation attaches to a running role, is idempotent per slot, and is
// closed with the role: it never outlives the listeners it vouches for.
func TestExtenderRoleActivationAttachesAndCloses(t *testing.T) {
	withTempHome(t)
	role, err := startExtenderRole(context.Background(), testExtenderRoleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	waitExtenderRoleReady(t, role)

	slotCtx, cancelSlot := context.WithCancel(context.Background())
	defer cancelSlot()
	inputs := &extenderActivationInputs{
		ctx:            slotCtx,
		apiUrl:         "http://127.0.0.1:1",
		byJwt:          newJwtHolder("jwt").Get,
		clientStrategy: newExtenderActivationStrategy(slotCtx),
	}
	role.StartActivation(inputs)
	first := role.currentActivator()
	if first == nil {
		t.Fatal("no activator after StartActivation")
	}
	role.StartActivation(inputs)
	if role.currentActivator() != first {
		t.Fatal("a second attach for the same slot replaced the activator")
	}
	role.StopActivation(&extenderActivationInputs{})
	if role.currentActivator() != first {
		t.Fatal("stopping another slot's activation ended this one")
	}
	role.StopActivation(inputs)
	if role.currentActivator() != nil {
		t.Fatal("activator still attached after StopActivation")
	}

	role.StartActivation(inputs)
	closeExtenderRole(t, role)
	if role.currentActivator() != nil {
		t.Fatal("role closed but its activator is still attached")
	}
}

// A role that never started says why at startup, once, and does not repeat the
// line at every periodic check.
func TestExtenderManagerAnnouncesOffAtStartupOnce(t *testing.T) {
	withTempHome(t)
	prev := globalControlState
	state := newControlState()
	globalControlState = state
	t.Cleanup(func() { globalControlState = prev })
	if err := state.set(extenderControlKey, "off"); err != nil {
		t.Fatal(err)
	}

	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { importantLogHook = nil })

	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx

	m.Reconcile("startup")
	m.Reconcile("periodic check")
	m.Reconcile("periodic check")
	if m.Role() != nil {
		t.Fatal("role started although the extender is off")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "off:") || !strings.Contains(lines[0], "urnet-tools extender on") {
		t.Fatalf("want one off line naming how to turn it on, got %q", lines)
	}
}

// The real role and the real activation loop against a fake platform: the token
// posted is the live one, a renewed token reaches the next post, a refusal is
// shown with its reason and a next-try time, and the post offers the carriers
// that are actually bound.
func TestExtenderActivationAgainstAFakePlatform(t *testing.T) {
	withTempHome(t)

	var mu sync.Mutex
	var auths []string
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			fmt.Fprint(w, `{"client_address":"203.0.113.9:4321"}`)
		case "/network/extender-activate":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			auths = append(auths, r.Header.Get("Authorization"))
			bodies = append(bodies, string(body))
			mu.Unlock()
			fmt.Fprint(w, `{"activated":false,"error":"the tcp carrier did not answer the challenge"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := testExtenderRoleConfig(t)
	config.configureActivator = func(s *connect.ExtenderActivatorSettings) {
		s.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
		s.MinBackoff = 50 * time.Millisecond
		s.MaxBackoff = 100 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	role, err := startExtenderRole(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeExtenderRole(t, role)
	waitExtenderRoleReady(t, role)

	m := newExtenderManager()
	m.ctx = ctx
	m.role = role

	jwt := newJwtHolder("jwt-first")
	role.StartActivation(&extenderActivationInputs{
		ctx:            ctx,
		apiUrl:         server.URL,
		byJwt:          jwt.Get,
		clientStrategy: newExtenderActivationStrategy(ctx),
	})

	// the refusal reaches the status with its reason
	deadline := time.Now().Add(20 * time.Second)
	var family extenderFamilyReading
	for time.Now().Before(deadline) {
		for _, f := range m.Reading().Families {
			if f.IpVersion == 4 && f.State == extenderFamilyRefused {
				family = f
			}
		}
		if family.State != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if family.State != extenderFamilyRefused {
		t.Fatalf("IPv4 never showed the platform's refusal: %+v", m.Reading().Families)
	}
	if !strings.Contains(family.Detail, "tcp carrier did not answer") {
		t.Errorf("the refusal's reason is missing: %q", family.Detail)
	}
	if family.NextAttemptUnixMs == 0 {
		t.Error("a refused family must say when it tries next")
	}

	// the first post carried the first token and the carriers that are bound
	mu.Lock()
	firstAuth, firstBody := auths[0], bodies[0]
	mu.Unlock()
	if firstAuth != "Bearer jwt-first" {
		t.Errorf("first post authorization = %q, want the live token", firstAuth)
	}
	for _, want := range []string{`"tcp"`, `"quic"`, `"dns"`} {
		if !strings.Contains(firstBody, want) {
			t.Errorf("the post did not offer %s: %s", want, firstBody)
		}
	}

	// a renewed token reaches the next post
	jwt.Set("jwt-renewed")
	role.currentActivator().Reactivate()
	for time.Now().Before(deadline) {
		mu.Lock()
		last := auths[len(auths)-1]
		mu.Unlock()
		if last == "Bearer jwt-renewed" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no post carried the renewed token; authorizations seen: %q", auths)
}

// A start that keeps failing (a corrupt identity file, say) is retried every poll,
// but the log says it once. Writing a line per retry would put thousands a day in
// the disk events log.
func TestExtenderManagerRepeatedStartFailureLogsOnce(t *testing.T) {
	withTempHome(t)
	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { importantLogHook = nil })

	attempts := 0
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.startRole = func(context.Context, extenderRoleConfig) (*extenderRole, error) {
		attempts++
		return nil, fmt.Errorf("identity file is corrupt")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx

	for i := 0; i < 5; i++ {
		m.Reconcile("periodic check")
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5: a failed start must be retried at every check", attempts)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "FAILED to start") || !strings.Contains(lines[0], "identity file is corrupt") {
		t.Fatalf("five failed attempts must log one FAILED line, got %d: %q", len(lines), lines)
	}
}

// The platform's probe can take close to its own 10 s budget and a busy answer can
// queue, so the activation post must be allowed at least the activator's own
// request budget (60 s). The default 15 s transport timeout cut it short.
func TestExtenderActivationStrategyAllowsTheActivatorsRequestBudget(t *testing.T) {
	settings := extenderActivationStrategySettings()
	if got := settings.ConnectSettings.RequestTimeout; got < 60*time.Second {
		t.Fatalf("request timeout = %s, want at least the activator's 60s budget", got)
	}
}

// A slot that ended before its activation could be installed must never get one.
// Detach can run between the manager saving the inputs and the role installing the
// activator; an activator for a dead slot would post with a frozen token for as
// long as the role lives.
func TestExtenderRoleDoesNotInstallAnActivationForAnEndedSlot(t *testing.T) {
	withTempHome(t)
	role, err := startExtenderRole(context.Background(), testExtenderRoleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closeExtenderRole(t, role)
	waitExtenderRoleReady(t, role)

	slotCtx, endSlot := context.WithCancel(context.Background())
	endSlot()
	role.StartActivation(&extenderActivationInputs{
		ctx:            slotCtx,
		apiUrl:         "http://127.0.0.1:1",
		byJwt:          newJwtHolder("jwt").Get,
		clientStrategy: newExtenderActivationStrategy(slotCtx),
	})
	if role.currentActivator() != nil {
		t.Fatal("an activator was installed for a slot that had already ended")
	}
}

// Attach, detach and replacement race freely in production (a direct slot is
// toggled while another attaches). Whatever the interleaving, no loop may be left
// running that nothing tracks: afterwards the role holds at most the last live
// slot's activator, and closing the role leaves none.
func TestExtenderRoleActivationAttachDetachRace(t *testing.T) {
	withTempHome(t)
	role, err := startExtenderRole(context.Background(), testExtenderRoleConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	waitExtenderRoleReady(t, role)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			inputs := &extenderActivationInputs{
				ctx:            ctx,
				apiUrl:         "http://127.0.0.1:1",
				byJwt:          newJwtHolder("jwt").Get,
				clientStrategy: newExtenderActivationStrategy(ctx),
			}
			role.StartActivation(inputs)
			if i%2 == 0 {
				cancel()
				role.StopActivation(inputs)
			} else {
				defer cancel()
			}
		}(i)
	}
	wg.Wait()

	closeExtenderRole(t, role)
	if role.currentActivator() != nil {
		t.Fatal("an activator survived the role's close")
	}
}

// Counts must survive the role stopping: `extender off` closes the role and the
// final sample is taken after the drain, so requests that finish while it closes
// still count. The role's own snapshot must keep them after close too.
func TestExtenderRoleKeepsItsCountsAfterClose(t *testing.T) {
	withTempHome(t)
	config := testExtenderRoleConfig(t)
	var mu sync.Mutex
	var tcpAddr string
	config.listen = func(network string, address string) (net.Listener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			mu.Lock()
			tcpAddr = l.Addr().String()
			mu.Unlock()
		}
		return l, err
	}
	role, err := startExtenderRole(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	waitExtenderRoleReady(t, role)
	mu.Lock()
	addr := tcpAddr
	mu.Unlock()

	// an ordinary https request to the carrier: counted as the proxy service
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + addr + "/")
	if err == nil {
		resp.Body.Close()
	}
	if got := role.snapshot().ServiceCounts["proxy"]; got != 1 {
		t.Fatalf("before close: proxy count = %d, want 1 (err=%v)", got, err)
	}
	// an ordinary web request is the proxy service but is never served
	if served := role.snapshot().ServedCounts; len(served) != 0 {
		t.Fatalf("an ordinary web request was counted as served: %v", served)
	}
	closeExtenderRole(t, role)
	if got := role.snapshot().ServiceCounts["proxy"]; got != 1 {
		t.Fatalf("after close: proxy count = %d, want 1: the count was lost when the role stopped", got)
	}
	if served := role.snapshot().ServedCounts; len(served) != 0 {
		t.Fatalf("after close: served = %v, want none", served)
	}
}

// Two slots attach at nearly the same time (a direct slot is restarted while the
// last one's attach is still in flight). Whatever the order, the activation the
// role runs must be the one the manager tracks: otherwise the older slot's loop
// keeps running with nothing that will ever stop it when the newer slot ends.
// The hook holds the first attach between saving its inputs and installing its
// activator, which is where a second attach could otherwise slip in.
func TestExtenderManagerAttachOrderingKeepsTheRoleAndManagerAgreeing(t *testing.T) {
	withTempHome(t)
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	defer m.stop("test over")
	waitExtenderRoleReady(t, role)

	newInputs := func() *extenderActivationInputs {
		slotCtx, endSlot := context.WithCancel(ctx)
		t.Cleanup(endSlot)
		return &extenderActivationInputs{
			ctx:            slotCtx,
			apiUrl:         "http://127.0.0.1:1",
			byJwt:          newJwtHolder("jwt").Get,
			clientStrategy: newExtenderActivationStrategy(slotCtx),
		}
	}
	a, b := newInputs(), newInputs()

	held := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	m.attachHook = func() {
		// only the FIRST attach is held (sync.Once would make the second block
		// inside the hook until the first returned, which hides the race)
		if !first.CompareAndSwap(false, true) {
			return
		}
		close(held)
		// B tries to attach while A is held. A correct manager keeps B out
		// until A is done, so this wait times out; a racy one lets B finish.
		select {
		case <-release:
		case <-time.After(300 * time.Millisecond):
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.AttachActivation(a) }()
	<-held
	go func() { defer wg.Done(); m.AttachActivation(b) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	m.mu.Lock()
	tracked := m.inputs
	m.mu.Unlock()
	role.mu.Lock()
	running := role.activatorInputs
	role.mu.Unlock()
	if running != tracked {
		t.Fatalf("the role runs one slot's activation but the manager tracks another (running %p, tracked %p): the older loop would never be stopped", running, tracked)
	}
}

// A replacement whose slot ends while it is being attached must not leave the
// manager tracking nothing while the role still runs the previous slot's loop.
func TestExtenderManagerCancelledReplacementKeepsThePreviousSlot(t *testing.T) {
	withTempHome(t)
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	defer m.stop("test over")
	waitExtenderRoleReady(t, role)

	newInputs := func() (*extenderActivationInputs, context.CancelFunc) {
		slotCtx, endSlot := context.WithCancel(ctx)
		t.Cleanup(endSlot)
		return &extenderActivationInputs{
			ctx:            slotCtx,
			apiUrl:         "http://127.0.0.1:1",
			byJwt:          newJwtHolder("jwt").Get,
			clientStrategy: newExtenderActivationStrategy(slotCtx),
		}, endSlot
	}
	a, _ := newInputs()
	b, endB := newInputs()
	m.AttachActivation(a)

	m.attachHook = func() { endB() }
	m.AttachActivation(b)
	m.attachHook = nil

	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	tracked := m.inputs
	m.mu.Unlock()
	role.mu.Lock()
	running := role.activatorInputs
	role.mu.Unlock()
	if running != a || tracked != a {
		t.Fatalf("slot A is live and should own the loop (running %p, tracked %p, A %p)", running, tracked, a)
	}
}

func TestExtenderManagerReplacementCancelledDuringRootKeyLoad(t *testing.T) {
	withTempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig {
		config := testExtenderRoleConfig(t)
		config.fetchHello = func(ctx context.Context, inputs *extenderActivationInputs) ([]byte, error) {
			if inputs.apiUrl == "http://replacement.invalid" {
				close(entered)
				<-ctx.Done()
			}
			return nil, errors.New("unavailable")
		}
		return config
	}
	m.ctx = ctx
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	defer m.stop("test over")
	waitExtenderRoleReady(t, role)
	newInputs := func(url string) (*extenderActivationInputs, context.CancelFunc) {
		slotCtx, end := context.WithCancel(ctx)
		return &extenderActivationInputs{ctx: slotCtx, apiUrl: url, byJwt: newJwtHolder("jwt").Get, clientStrategy: newExtenderActivationStrategy(slotCtx)}, end
	}
	a, endA := newInputs("http://127.0.0.1:1")
	defer endA()
	b, endB := newInputs("http://replacement.invalid")
	defer endB()
	m.AttachActivation(a)
	done := make(chan struct{})
	go func() { m.AttachActivation(b); close(done) }()
	<-entered
	endB()
	<-done
	m.mu.Lock()
	tracked := m.inputs
	m.mu.Unlock()
	if tracked != a || !role.activationOwnedBy(a) {
		t.Fatal("cancelled replacement displaced the live slot's activation")
	}
}

// A replacement whose slot ends right AFTER its activation was installed (A's loop
// is already closed) must still be stopped: nothing may run for a dead slot.
func TestExtenderManagerReplacementCancelledAfterInstallIsStopped(t *testing.T) {
	withTempHome(t)
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	defer m.stop("test over")
	waitExtenderRoleReady(t, role)

	newInputs := func() (*extenderActivationInputs, context.CancelFunc) {
		slotCtx, endSlot := context.WithCancel(ctx)
		t.Cleanup(endSlot)
		return &extenderActivationInputs{
			ctx:            slotCtx,
			apiUrl:         "http://127.0.0.1:1",
			byJwt:          newJwtHolder("jwt").Get,
			clientStrategy: newExtenderActivationStrategy(slotCtx),
		}, endSlot
	}
	a, _ := newInputs()
	b, endB := newInputs()
	m.AttachActivation(a)

	m.attachAfterStartHook = func() { endB() }
	m.AttachActivation(b)
	m.attachAfterStartHook = nil

	deadline := time.Now().Add(5 * time.Second)
	for role.activationOwnedBy(b) || role.activationOwnedBy(a) {
		if time.Now().After(deadline) {
			t.Fatal("an activation is still running for an ended slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	tracked := m.inputs
	m.mu.Unlock()
	if tracked != nil {
		t.Fatalf("the manager tracks %p after the only live replacement ended", tracked)
	}
}

// An attach for a slot that has already ended is not tracked at all.
func TestExtenderManagerDoesNotTrackAnEndedSlot(t *testing.T) {
	withTempHome(t)
	m := newExtenderManager()
	slotCtx, endSlot := context.WithCancel(context.Background())
	endSlot()
	m.AttachActivation(&extenderActivationInputs{ctx: slotCtx})
	m.mu.Lock()
	tracked := m.inputs
	m.mu.Unlock()
	if tracked != nil {
		t.Fatal("the manager tracks inputs of a slot that had already ended")
	}
}

func TestExtenderActivatorSettingsSpreadTheRestartPostOverThirtyMinutes(t *testing.T) {
	withTempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &extenderRole{ctx: ctx}
	settings := r.activatorSettings(&extenderActivationInputs{apiUrl: "http://127.0.0.1:1"})
	if settings.ReplacedPostJitter != 30*time.Minute {
		t.Fatalf("restart post spread is %s, want 30m", settings.ReplacedPostJitter)
	}
}

func TestParseRootKeysReadsTheHelloAnswer(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"extender_root_public_keys":["` + hex.EncodeToString(pub) + `"]}`)
	keySet, err := parseRootKeys(body)
	if err != nil || keySet.Len() != 1 {
		t.Fatalf("want one key, got %v err %v", keySet, err)
	}
	if _, err := parseRootKeys([]byte(`{}`)); err == nil {
		t.Fatal("an answer with no keys was accepted")
	}
	if _, err := parseRootKeys([]byte(`not json`)); err == nil {
		t.Fatal("garbage was accepted")
	}
	if _, err := parseRootKeys([]byte(`{"extender_root_public_keys":["zz"]}`)); err == nil {
		t.Fatal("a bad key was accepted")
	}
}

func TestExtenderRoleLoadsRootKeysAndRetriesOnFailure(t *testing.T) {
	withTempHome(t)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"extender_root_public_keys":["` + hex.EncodeToString(pub) + `"]}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &extenderRole{ctx: ctx, cancel: cancel}
	r.config.fetchHello = func(ctx context.Context, inputs *extenderActivationInputs) ([]byte, error) { return good, nil }
	slotCtx, endSlot := context.WithCancel(ctx)
	defer endSlot()
	inputs := &extenderActivationInputs{ctx: slotCtx, apiUrl: "http://127.0.0.1:1", byJwt: func() string { return "jwt" }}
	go r.loadRootKeys(inputs)
	deadline := time.Now().Add(5 * time.Second)
	for r.rootKeyDirectory(inputs.apiUrl).RootKeys().Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("root keys were never installed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if extenderFixedRenewalInterval >= 24*time.Hour {
		t.Fatalf("fixed renewal %s must be inside the record's 24 hours", extenderFixedRenewalInterval)
	}
}

// The directory must accept the platform's own records and reject another space's.
func TestRootKeyDirectoryAcceptsOnlyThePlatformSpace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &extenderRole{ctx: ctx}
	directory := r.rootKeyDirectory("https://api.bringyour.com")
	if !connect.ExtenderNetworkHostAllowed("bringyour.com", "bringyour.com") {
		t.Fatal("test premise: the platform space host is allowed by name")
	}
	if got := r.rootKeyDirectory("https://api.other.example"); got != directory {
		t.Fatal("the directory is created once")
	}
}

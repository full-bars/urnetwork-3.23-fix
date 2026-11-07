package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
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
// reminder stays in the ramlog so a long failure does not fill the disk log.
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
	for _, want := range []string{"starting", "stopping"} {
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

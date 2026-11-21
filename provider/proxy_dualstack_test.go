package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type fakePlatformTransport struct {
	// onSetAuth, when set, runs after the call is recorded and may block
	onSetAuth func(auth *connect.ClientAuth)
	mu        sync.Mutex
	auths     []*connect.ClientAuth
	kicks     int
	closes    int
	started   *connect.ClientAuth
	notifyCh  chan struct{}
}

func (self *fakePlatformTransport) IsConnected() bool { return true }
func (self *fakePlatformTransport) ConnectedNotify() <-chan struct{} {
	return self.notifyCh
}
func (self *fakePlatformTransport) SetAuth(auth *connect.ClientAuth) {
	self.mu.Lock()
	self.auths = append(self.auths, auth)
	hook := self.onSetAuth
	self.mu.Unlock()
	if hook != nil {
		hook(auth)
	}
}
func (self *fakePlatformTransport) Kick() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.kicks++
}
func (self *fakePlatformTransport) Close() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.closes++
}
func (self *fakePlatformTransport) counts() (auths int, kicks int, closes int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.auths), self.kicks, self.closes
}

func v6Result(status proxyIPv6Status, udp string, at time.Time, interval time.Duration) proxyIPv6Result {
	return proxyIPv6Result{Status: status, Source: "measured", UDP: udp, TCPExit: "2001:db8::1", UDPExit: "2001:db8::1", At: at, Interval: interval}
}

func TestProxyIPv6TransportWantedPolicy(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	for _, test := range []struct {
		name string
		udp  proxyUDPStatus
		v6   proxyIPv6Result
		want bool
	}{
		{"dual stack and dual protocol", proxyUDPOK, v6Result(proxyIPv6Yes, "yes", recent, 12*time.Hour), true},
		{"tcp only proxy with no udp relay", proxyUDPNoRelay, v6Result(proxyIPv6Yes, "unknown", recent, 12*time.Hour), true},
		{"ipv4 udp works but ipv6 udp does not", proxyUDPOK, v6Result(proxyIPv6Yes, "unknown", recent, 12*time.Hour), false},
		{"ipv4 udp works and ipv6 udp exit mismatches", proxyUDPOK, v6Result(proxyIPv6Yes, "mismatch", recent, 12*time.Hour), false},
		{"ipv6 no", proxyUDPOK, v6Result(proxyIPv6No, "unknown", recent, 24*time.Hour), false},
		{"ipv6 unknown", proxyUDPOK, v6Result(proxyIPv6Unknown, "unknown", recent, time.Hour), false},
		{"never measured", proxyUDPOK, proxyIPv6Result{}, false},
		{"udp exit mismatch of the ipv4 relay", proxyUDPExitMismatch, v6Result(proxyIPv6Yes, "yes", recent, 12*time.Hour), false},
		{"udp check unknown", proxyUDPUnknown, v6Result(proxyIPv6Yes, "yes", recent, 12*time.Hour), false},
		{"stale result", proxyUDPOK, v6Result(proxyIPv6Yes, "yes", now.Add(-25*time.Hour), 12*time.Hour), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyIPv6TransportWanted(proxyUDPResult{Status: test.udp}, test.v6, now); got != test.want {
				t.Fatalf("wanted = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProxyIPv6GateNeedsTwoNewResultsToStartAndToStop(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	// one observation is one completed check, so each new result carries its
	// own completion stamp, as the sweep hands it over
	ok := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	yesAt := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour) }
	noAt := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6No, "unknown", at, 24*time.Hour) }

	// the first qualifying result does not start it, polling again does not count
	if start, stop := gate.observe(t0, ok(t0), yesAt(t0)); start || stop {
		t.Fatal("started on the first result")
	}
	for i := 1; i <= 5; i++ {
		if start, stop := gate.observe(t0.Add(time.Duration(i)*time.Minute), ok(t0), yesAt(t0)); start || stop {
			t.Fatalf("polling the same result changed the gate at poll %d", i)
		}
	}
	// the second new qualifying result starts it, once
	t1 := t0.Add(13 * time.Hour)
	if start, _ := gate.observe(t1, ok(t1), yesAt(t1)); !start {
		t.Fatal("did not start on the second qualifying result")
	}
	if start, stop := gate.observe(t1.Add(time.Minute), ok(t1), yesAt(t1)); start || stop {
		t.Fatal("the same result started it again")
	}
	// one bad result keeps it running, the second stops it, once
	t2 := t1.Add(13 * time.Hour)
	if start, stop := gate.observe(t2, ok(t2), noAt(t2)); start || stop {
		t.Fatal("one bad result changed the gate")
	}
	t3 := t2.Add(25 * time.Hour)
	if _, stop := gate.observe(t3, ok(t3), noAt(t3)); !stop {
		t.Fatal("did not stop on the second bad result")
	}
	if start, stop := gate.observe(t3.Add(time.Minute), ok(t3), noAt(t3)); start || stop {
		t.Fatal("the same bad result stopped it again")
	}
}

// A running transport whose IPv6 record is still fresh stops when two new IPv4
// UDP checks disqualify it. Polling the same check again does not count.
func TestProxyIPv6GateStopsOnNewDisqualifyingUdpChecks(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	v6 := v6Result(proxyIPv6Yes, "yes", t0, 12*time.Hour)
	ok := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	bad := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPExitMismatch, CheckedAt: at} }

	// start it on two qualifying IPv6 results
	t1 := t0.Add(13 * time.Hour)
	v6b := v6Result(proxyIPv6Yes, "yes", t1, 12*time.Hour)
	gate.observe(t0, ok(t0), v6)
	if start, _ := gate.observe(t1, ok(t1), v6b); !start {
		t.Fatal("did not start")
	}
	// the IPv6 record stays the same and fresh. One new bad UDP check does not stop it
	u1 := t1.Add(time.Hour)
	if _, stop := gate.observe(u1, bad(u1), v6b); stop {
		t.Fatal("one disqualifying UDP check stopped it")
	}
	// polling the same check again does not count
	for i := 1; i <= 5; i++ {
		if _, stop := gate.observe(u1.Add(time.Duration(i)*time.Minute), bad(u1), v6b); stop {
			t.Fatalf("polling the same UDP check stopped it at poll %d", i)
		}
	}
	// a check that qualifies in between ends the failure streak: bad, ok, bad
	// is not two consecutive failures
	uOk := u1.Add(30 * time.Minute)
	if _, stop := gate.observe(uOk, ok(uOk), v6b); stop {
		t.Fatal("a qualifying UDP check stopped it")
	}
	uBad := uOk.Add(30 * time.Minute)
	if _, stop := gate.observe(uBad, bad(uBad), v6b); stop {
		t.Fatal("an isolated failure after a qualifying check stopped it")
	}
	// the second new bad check stops it, once
	u2 := uBad.Add(time.Hour)
	if _, stop := gate.observe(u2, bad(u2), v6b); !stop {
		t.Fatal("two new disqualifying UDP checks did not stop it")
	}
	if _, stop := gate.observe(u2.Add(time.Minute), bad(u2), v6b); stop {
		t.Fatal("the same check stopped it again")
	}
}

func TestProxyIPv6GateDoesNotFlap(t *testing.T) {
	var gate proxyIPv6Gate
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		at = at.Add(13 * time.Hour)
		var v6 proxyIPv6Result
		if i%2 == 0 {
			v6 = v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour)
		} else {
			v6 = v6Result(proxyIPv6No, "unknown", at, 24*time.Hour)
		}
		if start, stop := gate.observe(at, proxyUDPResult{Status: proxyUDPOK, CheckedAt: at}, v6); start || stop {
			t.Fatalf("an alternating proxy changed state at result %d", i)
		}
	}
}

func TestProxyIPv6GateStopsAStaleResultAtOnce(t *testing.T) {
	var gate proxyIPv6Gate
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ok := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	r1 := v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour)
	gate.observe(at, ok(at), r1)
	r2 := v6Result(proxyIPv6Yes, "yes", at.Add(13*time.Hour), 24*time.Hour)
	if start, _ := gate.observe(at.Add(13*time.Hour), ok(at.Add(13*time.Hour)), r2); !start {
		t.Fatal("did not start")
	}
	// the proxy is no longer measured: its result passes twice its interval
	stale := r2.At.Add(2*r2.Interval + time.Minute)
	if _, stop := gate.observe(stale, ok(at.Add(13*time.Hour)), r2); !stop {
		t.Fatal("a stale result did not stop the transport")
	}
}

func TestProxiedPlatformTransportKeepsTheLegacyTransportApart(t *testing.T) {
	base := &fakePlatformTransport{}
	auth1 := &connect.ClientAuth{ByJwt: "one"}
	auth2 := &connect.ClientAuth{ByJwt: "two"}
	transport := newProxiedPlatformTransport(base, auth1)

	var started *fakePlatformTransport
	var startedAuth *connect.ClientAuth
	build := func(auth *connect.ClientAuth) providerPlatformTransport {
		started = &fakePlatformTransport{}
		startedAuth = auth
		return started
	}
	if transport.hasExtra() {
		t.Fatal("an extra transport exists before it was started")
	}
	// a credential renewed before the start reaches the extra transport
	transport.SetAuth(auth2)
	if !transport.startExtra(build) || !transport.hasExtra() {
		t.Fatal("the extra transport did not start")
	}
	if startedAuth != auth2 {
		t.Fatal("the extra transport started with an old credential")
	}
	if transport.startExtra(build) {
		t.Fatal("a second extra transport started")
	}
	// later renewals and kicks go to both
	auth3 := &connect.ClientAuth{ByJwt: "three"}
	transport.SetAuth(auth3)
	transport.Kick()
	if a, k, _ := base.counts(); a != 2 || k != 1 {
		t.Fatalf("legacy transport saw %d auths and %d kicks, want 2 and 1", a, k)
	}
	if a, k, _ := started.counts(); a != 1 || k != 1 {
		t.Fatalf("extra transport saw %d auths and %d kicks, want 1 and 1", a, k)
	}
	// stopping closes only the extra transport
	if !transport.stopExtra() {
		t.Fatal("stopExtra reported nothing to stop")
	}
	if _, _, c := started.counts(); c != 1 {
		t.Fatalf("extra transport closed %d times", c)
	}
	if _, _, c := base.counts(); c != 0 {
		t.Fatal("stopping the extra transport closed the legacy one")
	}
	if transport.stopExtra() {
		t.Fatal("stopExtra stopped twice")
	}
	// closing the identity's transport closes the legacy one and any extra one
	transport.startExtra(build)
	extra := started
	transport.Close()
	if _, _, c := base.counts(); c != 1 {
		t.Fatalf("legacy transport closed %d times", c)
	}
	if _, _, c := extra.counts(); c != 1 {
		t.Fatalf("the extra transport closed %d times on Close", c)
	}
	// a builder that declines starts nothing
	if transport.startExtra(func(*connect.ClientAuth) providerPlatformTransport { return nil }) {
		t.Fatal("a declining builder started a transport")
	}
}

func controllerFixture(t *testing.T) (*proxiedIPv6Controller, *fakePlatformTransport, *int, *proxyUDPCheckStore, *connect.ProxySettings) {
	t.Helper()
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	store := stubUdpCheckStore(proxyUDPOK)
	base := &fakePlatformTransport{}
	transport := newProxiedPlatformTransport(base, &connect.ClientAuth{ByJwt: "jwt"})
	builds := 0
	controller := &proxiedIPv6Controller{
		store:     store,
		key:       settings.Key(),
		transport: transport,
		build: func(*connect.ClientAuth) providerPlatformTransport {
			builds++
			return &fakePlatformTransport{}
		},
	}
	// a test that ends with the extra transport running must not leak its
	// increment of the running gauge into later tests
	t.Cleanup(func() { transport.stopExtra() })
	return controller, base, &builds, store, settings
}

func TestProxiedIPv6ControllerStartsAndStopsOnMeasurements(t *testing.T) {
	controller, base, builds, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	runningBefore := proxyIPv6TransportsRunning.Load()

	// nothing measured yet
	controller.step(t0)
	if controller.transport.hasExtra() {
		t.Fatal("started with no measurement")
	}
	set := func(at time.Time, v6 proxyIPv6Result) {
		storeWithEntry(store, settings, proxyUDPOK, at, v6)
	}
	set(t0, v6Result(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	if controller.transport.hasExtra() {
		t.Fatal("started on the first result")
	}
	t1 := t0.Add(13 * time.Hour)
	set(t1, v6Result(proxyIPv6Yes, "yes", t1, 12*time.Hour))
	controller.step(t1)
	if !controller.transport.hasExtra() || *builds != 1 {
		t.Fatalf("not started after two results (builds %d)", *builds)
	}
	if got := proxyIPv6TransportsRunning.Load() - runningBefore; got != 1 {
		t.Fatalf("running gauge moved by %d, want 1", got)
	}
	t2 := t1.Add(13 * time.Hour)
	set(t2, v6Result(proxyIPv6No, "unknown", t2, 24*time.Hour))
	controller.step(t2)
	t3 := t2.Add(25 * time.Hour)
	set(t3, v6Result(proxyIPv6No, "unknown", t3, 24*time.Hour))
	controller.step(t3)
	if controller.transport.hasExtra() {
		t.Fatal("still running after two bad results")
	}
	if got := proxyIPv6TransportsRunning.Load() - runningBefore; got != 0 {
		t.Fatalf("running gauge moved by %d after the stop, want 0", got)
	}
	if _, _, c := base.counts(); c != 0 {
		t.Fatal("the legacy transport was touched")
	}
}

func TestProxiedIPv6ControllerKillSwitchStopsAndBlocks(t *testing.T) {
	controller, _, builds, store, settings := controllerFixture(t)
	enabled := true
	controller.enabled = func() bool { return enabled }
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("did not start")
	}
	enabled = false
	controller.step(t0.Add(27 * time.Hour))
	if controller.transport.hasExtra() {
		t.Fatal("the kill switch did not stop the transport")
	}
	// with the switch off, new qualifying results start nothing
	for i := 2; i < 6; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if controller.transport.hasExtra() {
		t.Fatal("started while the kill switch was off")
	}
	// back on: it needs two new results again
	enabled = true
	for i := 6; i < 8; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("did not start again after the switch came back on")
	}
	if *builds != 2 {
		t.Fatalf("builds = %d, want 2", *builds)
	}
}

func TestProxiedIPv6ControllerDecliningBuilderTriesAgain(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	attempts := 0
	controller.build = func(*connect.ClientAuth) providerPlatformTransport {
		attempts++
		return nil
	}
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	// new measurements 0..3: the build is tried on the second and on the fourth
	// only, because a decline needs two new results before the next attempt
	wantAttempts := []int{0, 1, 1, 2}
	for i := 0; i < 4; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
		if attempts != wantAttempts[i] {
			t.Fatalf("after measurement %d the builder ran %d times, want %d", i, attempts, wantAttempts[i])
		}
		// a poll of the same result changes nothing
		controller.step(at.Add(time.Minute))
		if attempts != wantAttempts[i] {
			t.Fatalf("a repeated poll of measurement %d ran the builder (%d, want %d)", i, attempts, wantAttempts[i])
		}
	}
	if controller.transport.hasExtra() {
		t.Fatal("a declined build left a transport")
	}
}

// An IPv6 record that expires while a UDP check runs still stops the transport.
func TestControllerStopsOnAnExpiredIPv6RecordDuringAUDPCheck(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var last time.Time
	for i := 0; i < 2; i++ {
		last = t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, last, v6Result(proxyIPv6Yes, "yes", last, 12*time.Hour))
		controller.step(last)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("did not start")
	}
	store.mu.Lock()
	store.entries[settings.Key()] = &proxyUDPCheckEntry{done: make(chan struct{}), ipv6: v6Result(proxyIPv6Yes, "yes", last, 12*time.Hour)}
	store.mu.Unlock()
	controller.step(last.Add(23 * time.Hour))
	if !controller.transport.hasExtra() {
		t.Fatal("stopped while the IPv6 record was still fresh")
	}
	controller.step(last.Add(25 * time.Hour))
	if controller.transport.hasExtra() {
		t.Fatal("the expired IPv6 record left the transport running during a UDP check")
	}
}

// A UDP result past its TTL is no evidence, even when the IPv6 result is fresh.
func TestControllerIgnoresAnExpiredUDPResult(t *testing.T) {
	controller, _, builds, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	udpAt := t0.Add(-30 * time.Hour)
	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, udpAt, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if controller.transport.hasExtra() || *builds != 0 {
		t.Fatalf("an expired UDP result started a transport (builds %d)", *builds)
	}
}

func TestProxyDualstackSettingDefaultsOn(t *testing.T) {
	state := newControlState()
	if !proxyDualstackEnabledIn(state) || !proxyDualstackEnabledIn(nil) {
		t.Fatal("proxy_dualstack is not on by default")
	}
	if err := state.set("proxy_dualstack", "off"); err != nil {
		t.Fatal(err)
	}
	if proxyDualstackEnabledIn(state) {
		t.Fatal("proxy_dualstack did not turn off")
	}
	if err := validateControlValue("proxy_dualstack", "maybe"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
}

func TestProxyIPv6TransportMetricsAreCountsOnly(t *testing.T) {
	var b strings.Builder
	writeProxyIPv6TransportMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"urnet_proxy_ipv6_transports ",
		`urnet_proxy_ipv6_transport_changes_total{change="start"} `,
		`urnet_proxy_ipv6_transport_changes_total{change="stop"} `,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "192.0.2") || strings.Contains(out, "proxy=") {
		t.Fatalf("metrics carry a proxy label:\n%s", out)
	}
}

func TestProxiedIPv6BuilderOnlyForTheProductionPlatformName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ConnectSettings.ProxySettings = proxySettings
	platformSettings := connect.DefaultPlatformTransportSettings()
	routeManager := connect.NewRouteManager(ctx, "test")

	for _, url := range []string{"ws://127.0.0.1:9", "https://localhost", "wss://connect.example.org", "https://[2001:db8::1]"} {
		if newProxiedIPv6Builder(ctx, clientStrategySettings, routeManager, url, platformSettings) != nil {
			t.Fatalf("a builder exists for %q", url)
		}
	}
	build := newProxiedIPv6Builder(ctx, clientStrategySettings, routeManager, "https://connect.bringyour.com", platformSettings)
	if build == nil {
		t.Fatal("no builder for the production platform name")
	}
	// the builder does not touch the identity's own settings
	if platformSettings.IpFamily != 0 {
		t.Fatal("the builder changed the identity's transport settings")
	}
	transport := build(&connect.ClientAuth{ByJwt: "jwt"})
	if transport == nil {
		t.Fatal("the builder returned no transport")
	}
	// the proxy at 127.0.0.1:1 refuses at once, the transport closes cleanly
	transport.Close()
}

// A credential renewed while the extra transport is being built reaches it
// before startExtra returns.
func TestStartExtraAppliesACredentialRenewedDuringTheBuild(t *testing.T) {
	base := &fakePlatformTransport{}
	authA := &connect.ClientAuth{ByJwt: "a"}
	authB := &connect.ClientAuth{ByJwt: "b"}
	transport := newProxiedPlatformTransport(base, authA)
	built := &fakePlatformTransport{}
	building := make(chan struct{})
	release := make(chan struct{})
	build := func(auth *connect.ClientAuth) providerPlatformTransport {
		if auth != authA {
			t.Errorf("built with %v, want the credential at the start", auth)
		}
		close(building)
		<-release
		return built
	}
	started := make(chan bool, 1)
	go func() { started <- transport.startExtra(build) }()
	<-building
	transport.SetAuth(authB)
	close(release)
	if !<-started {
		t.Fatal("the extra transport did not start")
	}
	built.mu.Lock()
	got := append([]*connect.ClientAuth(nil), built.auths...)
	built.mu.Unlock()
	if len(got) != 1 || got[0] != authB {
		t.Fatalf("the extra transport saw %v, want the renewed credential", got)
	}
	transport.stopExtra()
}

// A Close that runs while the extra transport is being built closes what was
// built and installs nothing.
func TestCloseDuringTheBuildClosesTheBuiltTransport(t *testing.T) {
	base := &fakePlatformTransport{}
	transport := newProxiedPlatformTransport(base, &connect.ClientAuth{ByJwt: "a"})
	built := &fakePlatformTransport{}
	building := make(chan struct{})
	release := make(chan struct{})
	build := func(*connect.ClientAuth) providerPlatformTransport {
		close(building)
		<-release
		return built
	}
	runningBefore := proxyIPv6TransportsRunning.Load()
	started := make(chan bool, 1)
	go func() { started <- transport.startExtra(build) }()
	<-building
	transport.Close()
	close(release)
	if <-started {
		t.Fatal("a closed wrapper accepted the extra transport")
	}
	if _, _, c := built.counts(); c != 1 {
		t.Fatalf("the built transport closed %d times, want 1", c)
	}
	if transport.hasExtra() {
		t.Fatal("the closed wrapper holds an extra transport")
	}
	if proxyIPv6TransportsRunning.Load() != runningBefore {
		t.Fatal("the running gauge moved for a transport that never ran")
	}
	if transport.startExtra(func(*connect.ClientAuth) providerPlatformTransport { return &fakePlatformTransport{} }) {
		t.Fatal("a closed wrapper started an extra transport")
	}
}

// Closing the wrapper counts the stop of a running extra transport.
func TestCloseAccountsTheRunningExtraTransport(t *testing.T) {
	transport := newProxiedPlatformTransport(&fakePlatformTransport{}, &connect.ClientAuth{ByJwt: "a"})
	runningBefore := proxyIPv6TransportsRunning.Load()
	stopsBefore := proxyIPv6TransportStops.Load()
	startsBefore := proxyIPv6TransportStarts.Load()
	if !transport.startExtra(func(*connect.ClientAuth) providerPlatformTransport { return &fakePlatformTransport{} }) {
		t.Fatal("did not start")
	}
	if got := proxyIPv6TransportsRunning.Load() - runningBefore; got != 1 {
		t.Fatalf("running gauge moved by %d after the start, want 1", got)
	}
	transport.Close()
	if got := proxyIPv6TransportsRunning.Load() - runningBefore; got != 0 {
		t.Fatalf("running gauge moved by %d after Close, want 0", got)
	}
	if proxyIPv6TransportStarts.Load()-startsBefore != 1 || proxyIPv6TransportStops.Load()-stopsBefore != 1 {
		t.Fatal("the start or the stop was not counted once")
	}
}

// A running extra transport whose measurement disappeared stops.
func TestControllerStopsWhenTheMeasurementIsGone(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("did not start")
	}
	store.mu.Lock()
	delete(store.entries, settings.Key())
	store.mu.Unlock()
	controller.step(t0.Add(30 * time.Hour))
	if controller.transport.hasExtra() {
		t.Fatal("the extra transport kept running with no measurement")
	}
	// the streaks start over: one new result does not start it again
	at := t0.Add(31 * time.Hour)
	storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
	controller.step(at)
	if controller.transport.hasExtra() {
		t.Fatal("started again on one result")
	}
}

// Two credentials renewed around the publication reach the extra transport in
// order: the older one never arrives after the newer one.
func TestCredentialsReachTheExtraTransportInOrder(t *testing.T) {
	authA := &connect.ClientAuth{ByJwt: "a"}
	authB := &connect.ClientAuth{ByJwt: "b"}
	authC := &connect.ClientAuth{ByJwt: "c"}
	transport := newProxiedPlatformTransport(&fakePlatformTransport{}, authA)
	inFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	built := &fakePlatformTransport{}
	first := true
	built.onSetAuth = func(*connect.ClientAuth) {
		if first {
			first = false
			close(inFirst)
			<-releaseFirst
		}
	}
	building := make(chan struct{})
	release := make(chan struct{})
	build := func(*connect.ClientAuth) providerPlatformTransport {
		close(building)
		<-release
		return built
	}
	started := make(chan bool, 1)
	go func() { started <- transport.startExtra(build) }()
	<-building
	transport.SetAuth(authB) // completes during the build
	close(release)
	<-inFirst // startExtra is forwarding B to the extra transport
	renewed := make(chan struct{})
	go func() { transport.SetAuth(authC); close(renewed) }() // must wait for B
	select {
	case <-renewed:
		t.Fatal("a newer credential was forwarded while the older one was still being applied")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if !<-started {
		t.Fatal("the extra transport did not start")
	}
	<-renewed
	built.mu.Lock()
	got := append([]*connect.ClientAuth(nil), built.auths...)
	built.mu.Unlock()
	if len(got) != 2 || got[0] != authB || got[1] != authC {
		t.Fatalf("the extra transport saw %v, want B then C", got)
	}
	transport.stopExtra()
}

// A poll during a running UDP check changes nothing: the in-flight entry has no
// completed UDP result, and reading it as unknown would stop a good transport.
func TestControllerIgnoresAnInFlightUDPCheck(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(i) * 13 * time.Hour)
		storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
		controller.step(at)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("did not start")
	}
	// a new IPv6 result arrives while a UDP check is in flight
	at := t0.Add(26 * time.Hour)
	store.mu.Lock()
	store.entries[settings.Key()] = &proxyUDPCheckEntry{done: make(chan struct{}), ipv6: v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour)}
	store.mu.Unlock()
	for i := 0; i < 3; i++ {
		controller.step(at.Add(time.Duration(i) * time.Minute))
	}
	if !controller.transport.hasExtra() {
		t.Fatal("a poll during a running UDP check stopped the transport")
	}
	// the check completes with ok: the new result counts normally
	storeWithEntry(store, settings, proxyUDPOK, at, v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour))
	controller.step(at.Add(5 * time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("the transport stopped after the check completed with ok")
	}
}

// A running transport whose identity record is recreated with no ipv6
// measurement keeps no evidence, the same as an identity with no entry, so it
// stops on that poll instead of waiting for two disqualifying udp checks.
func TestProxiedIPv6ControllerStopsOnAZeroIPv6Record(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 12*time.Hour))
	controller.step(t0.Add(time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh measurements did not start the transport")
	}

	storeWithEntry(store, settings, proxyUDPOK, t0.Add(2*time.Minute), proxyIPv6Result{})
	controller.step(t0.Add(2 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("a zero ipv6 record left the transport running")
	}
	if controller.gate.running {
		t.Fatal("the gate still reports the transport running")
	}
}

// Switching the switch off must not make an already counted result eligible
// again: after a re-enable the cached result is not recounted, so the start
// streak still needs two new measurements.
func TestProxiedIPv6ControllerDoesNotRecountACachedResultAfterReEnable(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	enabled := true
	controller.enabled = func() bool { return enabled }
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	// the first result is counted, and is not enough on its own
	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	if controller.transport.hasExtra() {
		t.Fatal("one measurement started the transport")
	}

	// the switch goes off and comes back on; the same result is still cached
	enabled = false
	controller.step(t0.Add(time.Minute))
	enabled = true
	controller.step(t0.Add(2 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("the cached result was counted again after the re-enable")
	}

	// two genuinely new measurements still start it
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(3*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(3*time.Minute), 12*time.Hour))
	controller.step(t0.Add(3 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("the first new result started the transport")
	}
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(4*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(4*time.Minute), 12*time.Hour))
	controller.step(t0.Add(4 * time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two new results after the re-enable did not start the transport")
	}
}

// The extra transport builds its own strategy, so it owns it: stopping the
// extra transport releases the strategy it was given.
func TestProxiedIPv6BuilderOwnsItsStrategy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platformSettings := &connect.PlatformTransportSettings{}
	build := newProxiedIPv6Builder(
		ctx,
		&connect.ClientStrategySettings{},
		&connect.RouteManager{},
		"wss://connect.bringyour.com",
		platformSettings,
	)
	if build == nil {
		t.Fatal("the builder declined")
	}

	var built *connect.ClientStrategy
	previous := newProxiedIPv6Strategy
	newProxiedIPv6Strategy = func(ctx context.Context, settings *connect.ClientStrategySettings, ipFamily int) *connect.ClientStrategy {
		built = connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
		return built
	}
	t.Cleanup(func() { newProxiedIPv6Strategy = previous })

	extra := build(&connect.ClientAuth{ByJwt: "jwt"})
	if extra == nil {
		t.Fatal("the builder returned no transport")
	}
	if built == nil {
		t.Fatal("the builder built no strategy")
	}
	if built.Closed() {
		t.Fatal("the strategy was closed before the transport ran")
	}
	extra.Close()
	if !built.Closed() {
		t.Fatal("closing the extra transport left its own strategy open")
	}
}

// One bad sweep publishes its udp result and its ipv6 record separately. A poll
// that lands between the two publications must count that failure once, so a
// single bad sweep cannot stop the transport.
func TestProxyIPv6GateCountsOneBadSweepOnce(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	good := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	bad := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPExitMismatch, CheckedAt: at} }
	yes := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour) }

	gate.observe(t0, good(t0), yes(t0))
	t1 := t0.Add(13 * time.Hour)
	if start, _ := gate.observe(t1, good(t1), yes(t1)); !start {
		t.Fatal("two good checks did not start the transport")
	}

	t2 := t1.Add(13 * time.Hour)
	if _, stop := gate.observe(t2, bad(t2), yes(t1)); stop {
		t.Fatal("the first publication of one bad sweep stopped the transport")
	}
	if _, stop := gate.observe(t2.Add(time.Second), bad(t2), yes(t2)); stop {
		t.Fatal("one bad sweep stopped the transport when a poll landed between its publications")
	}
	t3 := t2.Add(13 * time.Hour)
	if _, stop := gate.observe(t3, bad(t3), yes(t3)); !stop {
		t.Fatal("a second bad sweep did not stop the transport")
	}
}

// The udp result is the evidence the transport runs on. When it expires the
// transport stops on that poll, the same as when the identity has no
// measurement at all.
func TestProxiedIPv6ControllerStopsWhenTheUdpEvidenceExpires(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 12*time.Hour))
	controller.step(t0.Add(time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh measurements did not start the transport")
	}

	// the ipv6 record is still fresh (72h interval, so twice it outlives the
	// udp result's own TTL), and only the udp result is past its TTL
	now := t0.Add(time.Minute + proxyUDPOKTTL + time.Minute)
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 72*time.Hour))
	controller.step(now)
	if controller.transport.hasExtra() {
		t.Fatal("expired udp evidence left the transport running")
	}
}

// While a udp check runs the poll reads the in-flight branch, which must apply
// the same zero-record rule: a record with no ipv6 measurement is no evidence,
// so a running transport stops on that poll.
func TestProxiedIPv6ControllerStopsInFlightOnAZeroIPv6Record(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 12*time.Hour))
	controller.step(t0.Add(time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh measurements did not start the transport")
	}

	// a check is running and the record it left carries no ipv6 measurement
	store.mu.Lock()
	store.entries[settings.Key()] = &proxyUDPCheckEntry{done: make(chan struct{}), at: t0, ipv6: proxyIPv6Result{}}
	store.mu.Unlock()
	controller.step(t0.Add(2 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("an in-flight check with a zero ipv6 record left the transport running")
	}
}

// One ipv6 failure must count once. A new ok udp check that leaves the same
// ipv6 record in place does not count it again, so the stop still needs two
// fresh observations.
func TestProxyIPv6GateDoesNotCountAnIpv6FailureTwice(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ok := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	yes := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour) }
	no := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6No, "unknown", at, 24*time.Hour) }

	gate.observe(t0, ok(t0), yes(t0))
	t1 := t0.Add(13 * time.Hour)
	if start, _ := gate.observe(t1, ok(t1), yes(t1)); !start {
		t.Fatal("two good checks did not start the transport")
	}

	// one ipv6 failure
	t2 := t1.Add(13 * time.Hour)
	if _, stop := gate.observe(t2, ok(t2), no(t2)); stop {
		t.Fatal("one ipv6 failure stopped the transport")
	}
	// a new ok udp check with the same ipv6 record must not count it again
	t3 := t2.Add(13 * time.Hour)
	if _, stop := gate.observe(t3, ok(t3), no(t2)); stop {
		t.Fatal("a udp check counted the same ipv6 failure again")
	}
	// a second ipv6 failure still stops it
	t4 := t3.Add(13 * time.Hour)
	if _, stop := gate.observe(t4, ok(t4), no(t4)); !stop {
		t.Fatal("two ipv6 failures did not stop the transport")
	}
}

// The start streak counts new ipv6 measurements, not udp checks: a proxy is
// started on what was measured, so many qualifying udp checks under one
// measurement must not start it, and a second measurement must.
func TestProxyIPv6GateStartsOnlyOnNewIpv6Measurements(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ok := func(at time.Time) proxyUDPResult { return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at} }
	yes := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour) }

	gate.observe(t0, ok(t0), yes(t0))
	for i := 1; i <= 4; i++ {
		at := t0.Add(time.Duration(i) * 30 * time.Minute)
		if start, _ := gate.observe(at, ok(at), yes(t0)); start {
			t.Fatalf("udp check %d started the transport with no new ipv6 measurement", i)
		}
	}
	t1 := t0.Add(13 * time.Hour)
	if start, _ := gate.observe(t1, ok(t1), yes(t1)); !start {
		t.Fatal("a second ipv6 measurement did not start the transport")
	}
}

// A record that disappears takes the pending start streak with it: after one
// qualifying result, an entry recreated without an ipv6 record resets the
// streak, so one fresh result cannot start the transport.
func TestProxiedIPv6ControllerDropsAPendingStartOnAZeroRecord(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)

	// the entry is recreated without an ipv6 measurement, transport stopped
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), proxyIPv6Result{})
	controller.step(t0.Add(time.Minute))

	// one fresh measurement now must not be enough
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(2*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(2*time.Minute), 12*time.Hour))
	controller.step(t0.Add(2 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("one fresh result started the transport on a streak that survived a zero record")
	}
	// and two do start it
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(3*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(3*time.Minute), 12*time.Hour))
	controller.step(t0.Add(3 * time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh results after the zero record did not start the transport")
	}
}

// Disabling dual stack clears the streaks but keeps every stamp that says a
// result was already counted, so a UDP-only check after re-enable cannot make
// the cached ipv6 result count again.
func TestProxiedIPv6ControllerKeepsTheCountedIpv6ResultAcrossASwitch(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	enabled := true
	controller.enabled = func() bool { return enabled }
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)

	enabled = false
	controller.step(t0.Add(time.Minute))
	enabled = true

	// a udp-only check runs before any new ipv6 measurement
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(2*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0.Add(2 * time.Minute))
	// one fresh measurement must not be enough
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(3*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(3*time.Minute), 12*time.Hour))
	controller.step(t0.Add(3 * time.Minute))
	if controller.transport.hasExtra() {
		t.Fatal("the cached ipv6 result counted again after the re-enable")
	}
	// two fresh measurements still start it
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(4*time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(4*time.Minute), 12*time.Hour))
	controller.step(t0.Add(4 * time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh measurements after the re-enable did not start the transport")
	}
}

// A refresh that starts after the carried udp result expired must not keep a
// running transport up: the evidence is gone even though the check is in
// flight, while a fresh ipv6 record stays.
func TestProxiedIPv6ControllerStopsOnExpiredUdpDuringARecheck(t *testing.T) {
	controller, _, _, store, settings := controllerFixture(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	storeWithEntry(store, settings, proxyUDPOK, t0, measuredIPv6(proxyIPv6Yes, "yes", t0, 12*time.Hour))
	controller.step(t0)
	storeWithEntry(store, settings, proxyUDPOK, t0.Add(time.Minute), measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 12*time.Hour))
	controller.step(t0.Add(time.Minute))
	if !controller.transport.hasExtra() {
		t.Fatal("two fresh measurements did not start the transport")
	}

	// a recheck is running while the result it carries has expired
	now := t0.Add(time.Minute + proxyUDPOKTTL + time.Minute)
	store.mu.Lock()
	store.entries[settings.Key()] = &proxyUDPCheckEntry{
		done:        make(chan struct{}),
		result:      proxyUDPResult{Status: proxyUDPOK},
		at:          t0.Add(time.Minute),
		completedAt: t0.Add(time.Minute),
		ipv6:        measuredIPv6(proxyIPv6Yes, "yes", t0.Add(time.Minute), 72*time.Hour),
	}
	store.mu.Unlock()
	controller.step(now)
	if controller.transport.hasExtra() {
		t.Fatal("expired udp evidence kept the transport up during a recheck")
	}
}

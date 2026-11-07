package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// An activator that has not started its loop, for the tests of what applyPacing makes
// of a stored record. Nothing here reads a clock or the network.
func newTestPacingLoadActivator(
	publicKey []byte,
	directory *ExtenderDirectory,
	now time.Time,
	previous bool,
) *ExtenderActivator {
	settings := DefaultExtenderActivatorSettings()
	settings.PublicKey = publicKey
	settings.Directory = directory
	settings.Now = func() time.Time { return now }
	settings.postJitterForTest = func(time.Duration) time.Duration { return 10 * time.Minute }
	if previous {
		settings.PreviousState = &ExtenderActivatorState{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	self := &ExtenderActivator{
		ctx:           ctx,
		cancel:        cancel,
		log:           loggerOrDefault(nil),
		settings:      settings,
		changeMonitor: NewMonitorValue[uint64](0),
		families:               map[int]*ExtenderFamilyActivationStatus{4: {IpVersion: 4}},
		schedules:              map[int]*extenderActivateSchedule{4: {backoff: settings.MinBackoff}},
		pacing:                 map[int]ExtenderActivationPacing{},
		unverifiedPacing:       map[int]ExtenderActivationPacing{},
		pacingReady:            make(chan struct{}),
		lastPeriodicLogTimes:   map[int]time.Time{},
		firstFailureHintLogged: map[int]bool{},
	}
	return self
}

type testPacingLoadFixture struct {
	now       time.Time
	publicKey []byte
	directory *ExtenderDirectory
	fixture   *testActivatorFixture
}

// A fixture only for its directory, operator and key; its activator is closed.
func newTestPacingLoadFixture(t *testing.T) *testPacingLoadFixture {
	t.Helper()
	fixture := newTestActivatorFixtureWithPrepare(t, nil, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
	})
	fixture.activator.Close()
	return &testPacingLoadFixture{
		now:       fixture.clock.Now(),
		publicKey: fixture.publicKey,
		directory: fixture.directory,
		fixture:   fixture,
	}
}

func (self *testPacingLoadFixture) record(pacing ExtenderActivationPacing) *ExtenderActivationPacingRecord {
	return &ExtenderActivationPacingRecord{
		PublicKey: self.publicKey,
		Families:  map[int]ExtenderActivationPacing{4: pacing},
	}
}

func (self *testPacingLoadFixture) activatedPacing(expire time.Time) ExtenderActivationPacing {
	return ExtenderActivationPacing{
		State:                ExtenderPacingActivated,
		RecordExpireTime:     expire,
		Ip:                   netip.MustParseAddr(testActivateIpv4),
		LastAttemptAt:        self.now.Add(-time.Hour),
		NextAllowedAttemptAt: self.now.Add(19 * time.Hour),
	}
}

func (self *testPacingLoadFixture) plant(t *testing.T, expire time.Time, ips ...string) {
	t.Helper()
	applyTestOperatorRecord(t, self.directory, self.fixture.operator, self.publicKey, self.now, expire, ips...)
}

// A stored record the directory does not hold is no activation (F2): the family is
// unactivated and due at once, whatever the store says of its expiry.
func TestExtenderActivatorLoadClearsARecordTheDirectoryLacks(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	expire := f.now.Add(25 * time.Hour)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(f.activatedPacing(expire)), nil)

	if family := self.families[4]; family.Activated {
		t.Fatalf("family = %+v, expected unactivated", family)
	}
	pacing := self.pacing[4]
	if pacing.State != ExtenderPacingUnactivated || !pacing.RecordExpireTime.IsZero() || pacing.Ip.IsValid() {
		t.Fatalf("pacing = %+v, expected cleared", pacing)
	}
	if next := self.schedules[4].nextActivateTime; !next.IsZero() {
		t.Fatalf("next post at %s, expected at once", next)
	}
}

// The directory holds a record, but not the one stored: an earlier expiry, or an address
// the stored one was not kept for. (A LATER record for the same key is the other
// family's newer post: see TestExtenderActivatorLoadKeepsAFamilyWhoseRecordWasSuperseded.)
func TestExtenderActivatorLoadClearsARecordThatDoesNotMatch(t *testing.T) {
	cases := []struct {
		name  string
		plant func(f *testPacingLoadFixture, t *testing.T, expire time.Time)
	}{
		{"an earlier expiry", func(f *testPacingLoadFixture, t *testing.T, expire time.Time) {
			f.plant(t, expire.Add(-time.Hour), testActivateIpv4)
		}},
		{"another address", func(f *testPacingLoadFixture, t *testing.T, expire time.Time) {
			f.plant(t, expire, "198.51.100.99")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestPacingLoadFixture(t)
			expire := f.now.Add(25 * time.Hour)
			c.plant(f, t, expire)
			self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
			self.applyPacing(f.record(f.activatedPacing(expire)), nil)
			if self.families[4].Activated || self.pacing[4].State != ExtenderPacingUnactivated {
				t.Fatalf("family = %+v, pacing = %+v, expected unactivated", self.families[4], self.pacing[4])
			}
			if next := self.schedules[4].nextActivateTime; !next.IsZero() {
				t.Fatalf("next post at %s, expected at once", next)
			}
		})
	}
}

// A hold is the operator's word on when to come back, and it outlives the record it
// was set beside.
func TestExtenderActivatorLoadKeepsAHoldWhenItClearsTheRecord(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	pacing := f.activatedPacing(f.now.Add(25 * time.Hour))
	pacing.State = ExtenderPacingRenewalRefused
	pacing.Hold = true
	pacing.NextAllowedAttemptAt = f.now.Add(2 * time.Hour)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(pacing), nil)
	if self.families[4].Activated {
		t.Fatal("family stands activated on a record the directory lacks")
	}
	if next := self.schedules[4].nextActivateTime; !next.Equal(f.now.Add(2 * time.Hour)) {
		t.Fatalf("next post at %s, expected the hold", next)
	}
}

// With no directory the record cannot be judged, and the record stands: the post
// every restart makes will verify it.
func TestExtenderActivatorLoadTrustsARecordWithNoDirectory(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	self := newTestPacingLoadActivator(f.publicKey, nil, f.now, false)
	self.applyPacing(f.record(f.activatedPacing(f.now.Add(25*time.Hour))), nil)
	if !self.families[4].Activated {
		t.Fatalf("family = %+v, expected activated", self.families[4])
	}
	if next := self.schedules[4].nextActivateTime; !next.Equal(f.now.Add(10 * time.Minute)) {
		t.Fatalf("next post at %s, expected the jittered post", next)
	}
}

// A record the directory holds stands, and its family posts at the jittered time.
func TestExtenderActivatorLoadKeepsARecordTheDirectoryHolds(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	expire := f.now.Add(25 * time.Hour)
	f.plant(t, expire, testActivateIpv4)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(f.activatedPacing(expire)), nil)
	if !self.families[4].Activated || self.pacing[4].State != ExtenderPacingActivated {
		t.Fatalf("family = %+v, pacing = %+v", self.families[4], self.pacing[4])
	}
	if next := self.schedules[4].nextActivateTime; !next.Equal(f.now.Add(10 * time.Minute)) {
		t.Fatalf("next post at %s, expected the jittered post", next)
	}
}

// What the standing post offered comes back with the record, so a carrier the new
// process serves that the record lacks is still seen as off it.
func TestExtenderActivatorLoadRestoresThePostedCarriers(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	expire := f.now.Add(25 * time.Hour)
	f.plant(t, expire, testActivateIpv4)
	pacing := f.activatedPacing(expire)
	pacing.PostedCarriers = []string{ExtenderCarrierTcp}
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(pacing), nil)
	schedule := self.schedules[4]
	if !schedule.missesACarrier([]string{ExtenderCarrierTcp, ExtenderCarrierQuic}, nil) {
		t.Fatal("a carrier off the record went unseen after the load")
	}
	if schedule.missesACarrier([]string{ExtenderCarrierTcp}, nil) {
		t.Fatal("the carriers of the record read as off it")
	}
}

// A time saved by a clock that was ahead is brought back to what this clock can have
// written (F3): the family is not silent for as long as the stored time says.
func TestExtenderActivatorLoadClampsTimesFromAClockThatWasAhead(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(ExtenderActivationPacing{
		State:                ExtenderPacingFailed,
		LastAttemptAt:        f.now.Add(400 * 24 * time.Hour),
		NextAllowedAttemptAt: f.now.Add(400 * 24 * time.Hour),
	}), nil)
	pacing := self.pacing[4]
	if !pacing.NextAllowedAttemptAt.Equal(f.now.Add(24 * time.Hour)) {
		t.Fatalf("next allowed = %s, expected a day ahead", pacing.NextAllowedAttemptAt.Sub(f.now))
	}
	if !pacing.LastAttemptAt.Equal(f.now) {
		t.Fatalf("last attempt = %s, expected now", pacing.LastAttemptAt.Sub(f.now))
	}
	if next := self.schedules[4].nextActivateTime; !next.Equal(f.now.Add(24 * time.Hour)) {
		t.Fatalf("next post in %s, expected a day", next.Sub(f.now))
	}
}

// A store that blocks its load until released (F4).
type testBlockingPacingStore struct {
	testPacingStore
	release  chan struct{}
	returned chan struct{}
	late     *ExtenderActivationPacingRecord
}

func (self *testBlockingPacingStore) LoadPacing() (*ExtenderActivationPacingRecord, error) {
	<-self.release
	defer close(self.returned)
	return self.late, nil
}

// An activator whose store answers after the timeout starts ready and posts fresh; the
// late answer is dropped.
func TestExtenderActivatorLoadTimeoutDropsALateAnswer(t *testing.T) {
	store := &testBlockingPacingStore{
		testPacingStore: *newTestPacingStore(),
		release:         make(chan struct{}),
		returned:        make(chan struct{}),
	}
	releaseOnce := sync.Once{}
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(release)
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			store.late = &ExtenderActivationPacingRecord{
				PublicKey: fixture.publicKey,
				Families: map[int]ExtenderActivationPacing{4: {
					State:                ExtenderPacingRefused,
					NextAllowedAttemptAt: fixture.clock.Now().Add(24 * time.Hour),
					Hold:                 true,
				}},
			}
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
			settings.PacingLoadTimeout = 20 * time.Millisecond
		},
	)
	select {
	case <-fixture.activator.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the activator was not ready after the load timeout")
	}
	// it posts as a new process does, though the store said to hold a day
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()

	release()
	select {
	case <-store.returned:
	case <-time.After(30 * time.Second):
		t.Fatal("the store did not return")
	}
	fixture.step(time.Minute)
	if pacing := fixture.activator.Pacing()[4]; pacing.State != ExtenderPacingActivated || pacing.Hold {
		t.Fatalf("pacing = %+v, expected the post's, not the late answer's", pacing)
	}
}

// A store that fails its load starts the activator as a new process does, and the
// activator is ready.
func TestExtenderActivatorLoadErrorStartsFresh(t *testing.T) {
	store := &testReleasablePacingStore{testPacingStore: *newTestPacingStore()}
	store.release()
	fixture := newTestActivatorFixtureWithPrepare(t, nil, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.PacingStore = store
	})
	select {
	case <-fixture.activator.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the activator was not ready after a failed load")
	}
	fixture.waitPass()
	fixture.waitPost()
}

// Ready is closed from the start with no store.
func TestExtenderActivatorReadyWithNoStore(t *testing.T) {
	fixture := newTestActivatorFixture(t, nil)
	select {
	case <-fixture.activator.Ready():
	default:
		t.Fatal("an activator with no store is not ready")
	}
}

// A store released under the activator, as a role's is when it is replaced: its load and
// its saves answer with an error, which is logged, and the pacing stands in memory.
type testReleasablePacingStore struct {
	testPacingStore
	releasedLock sync.Mutex
	released     bool
}

var errTestPacingStoreReleased = errors.New("the pacing store is released")

func (self *testReleasablePacingStore) release() {
	self.releasedLock.Lock()
	defer self.releasedLock.Unlock()
	self.released = true
}

func (self *testReleasablePacingStore) isReleased() bool {
	self.releasedLock.Lock()
	defer self.releasedLock.Unlock()
	return self.released
}

func (self *testReleasablePacingStore) LoadPacing() (*ExtenderActivationPacingRecord, error) {
	if self.isReleased() {
		return nil, errTestPacingStoreReleased
	}
	return self.testPacingStore.LoadPacing()
}

func (self *testReleasablePacingStore) SavePacing(record *ExtenderActivationPacingRecord) error {
	if self.isReleased() {
		return errTestPacingStoreReleased
	}
	return self.testPacingStore.SavePacing(record)
}

func TestExtenderActivatorReleasedStoreDuringReplacement(t *testing.T) {
	for _, c := range []struct {
		name     string
		helloDue bool
	}{
		{name: "address check due", helloDue: true},
		{name: "address check not due", helloDue: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &testReleasablePacingStore{testPacingStore: *newTestPacingStore()}
			fixture := newTestActivatorFixtureActivated(t, func(settings *ExtenderActivatorSettings) {
				settings.PacingStore = store
			})
			fixture.step(time.Minute)
			_, savesBefore := store.family(4)

			// A pass can finish without a hello when the carried address check is not due.
			passEnded := make(chan time.Duration, 64)
			store.release()
			fixture.replaceActivator(func(settings *ExtenderActivatorSettings) {
				settings.postJitterForTest = func(time.Duration) time.Duration { return 30 * time.Minute }
				settings.ResumeCheckTimeout = 0
				settings.PassAfter = func(wait time.Duration) <-chan time.Time {
					passEnded <- wait
					return nil
				}
				settings.PreviousState.nextAddressCheckTime = fixture.clock.Now()
				if !c.helloDue {
					settings.PreviousState.nextAddressCheckTime = fixture.clock.Now().Add(time.Minute)
				}
			})
			select {
			case <-fixture.activator.Ready():
			case <-time.After(30 * time.Second):
				t.Fatal("the replacement was not ready with its store released")
			}
			if status := fixture.activator.Status().Family(4); !status.Activated {
				t.Fatalf("v4 status = %+v, expected the record carried over", status)
			}
			select {
			case <-passEnded:
			case <-time.After(30 * time.Second):
				t.Fatal("the replacement did not finish its first pass")
			}
			select {
			case <-fixture.operator.hellos:
				if !c.helloDue {
					t.Fatal("an address check ran before its deadline")
				}
			default:
				if c.helloDue {
					t.Fatal("the due address check did not run")
				}
			}
			fixture.step(30 * time.Minute)
			fixture.waitPost()
			fixture.waitPost()
			// A later pass proves the one that posted has finished.
			fixture.step(time.Minute)
			if pacing := fixture.activator.Pacing()[4]; pacing.State != ExtenderPacingActivated {
				t.Fatalf("pacing = %+v, expected activated in memory", pacing)
			}
			if _, saves := store.family(4); saves != savesBefore {
				t.Fatalf("saves = %d, expected none after the release (%d)", saves, savesBefore)
			}
		})
	}
}

// A host with both families posts twice and the directory keeps the newest record. The
// family that posted first finds a LATER record that still lists its address: its
// activation stands, and it adopts that record's expiry.
func TestExtenderActivatorLoadKeepsAFamilyWhoseRecordWasSuperseded(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	stored := f.now.Add(25 * time.Hour)
	newer := stored.Add(10 * time.Second)
	f.plant(t, newer, testActivateIpv4)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(f.activatedPacing(stored)), nil)
	if !self.families[4].Activated || self.pacing[4].State != ExtenderPacingActivated {
		t.Fatalf("family = %+v, pacing = %+v, expected the activation to stand", self.families[4], self.pacing[4])
	}
	if got := self.pacing[4].RecordExpireTime; got.UnixMilli() != newer.UnixMilli() {
		t.Fatalf("expiry = %s, expected the directory's newer %s", got, newer)
	}
}

// A newer record keeps the family only when it positively lists the stored address.
func TestExtenderActivatorLoadClearsAFamilyWhenTheNewerRecordDoesNotListItsAddress(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	stored := f.now.Add(25 * time.Hour)
	f.plant(t, stored.Add(10*time.Second), "198.51.100.99")
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.applyPacing(f.record(f.activatedPacing(stored)), nil)
	if self.families[4].Activated || self.pacing[4].State != ExtenderPacingUnactivated {
		t.Fatalf("family = %+v, pacing = %+v, expected the family to be cleared", self.families[4], self.pacing[4])
	}
}

// The restart spread never runs past half of what is left of the record.
func TestExtenderActivatorLoadBoundsTheRestartSpreadByTheRecordLifetime(t *testing.T) {
	f := newTestPacingLoadFixture(t)
	// 20 minutes left, spread 1 hour: the draw is bounded to 10 minutes
	expire := f.now.Add(20 * time.Minute)
	f.plant(t, expire, testActivateIpv4)
	self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
	self.settings.ReplacedPostJitter = time.Hour
	self.settings.postJitterForTest = func(maxJitter time.Duration) time.Duration { return maxJitter }
	self.applyPacing(f.record(f.activatedPacing(expire)), nil)
	next := self.schedules[4].nextActivateTime
	if wait := next.Sub(f.now); wait < 0 || 10*time.Minute <= wait {
		t.Fatalf("next post in %s, expected under 10m (half of the 20m left)", wait)
	}
}

func TestExtenderDirectoryStoredRecordRequiresRootKeys(t *testing.T) {
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	seeded, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
	})
	publicKey := newTestExtenderKey(t)
	expire := clock.Now().Add(24 * time.Hour)
	record := signTestRecord(
		t,
		rootPrivateKey,
		publicKey,
		clock.Now(),
		expire,
		testExtenderAddress(testActivateIpv4),
	)
	if _, err := seeded.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	seeded.Close()

	openUnverified := func() *ExtenderDirectory {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		settings := DefaultExtenderDirectorySettings()
		settings.Now = clock.Now
		settings.NetworkHosts = []string{testExtenderNetworkHost}
		settings.Store = store
		directory := NewExtenderDirectory(ctx, settings)
		t.Cleanup(directory.Close)
		return directory
	}

	unverified := openUnverified()
	if _, _, ok := unverified.ActiveRecordOf(publicKey); ok {
		t.Fatal("stored record reported as active before root keys loaded")
	}
	if unverified.IsActiveKey(publicKey) {
		t.Fatal("stored key reported as active before root keys loaded")
	}
	if unverified.CurrentRecordNamesFamily(publicKey, 4) {
		t.Fatal("stored record named family before root keys loaded")
	}

	unverified.SetRootKeys(NewExtenderRootKeySet(newTestExtenderKey(t)))
	if _, _, ok := unverified.ActiveRecordOf(publicKey); ok {
		t.Fatal("unverifiable stored record survived SetRootKeys")
	}
	if unverified.CurrentRecordNamesFamily(publicKey, 4) {
		t.Fatal("unverifiable stored record named family after SetRootKeys")
	}

	verified := openUnverified()
	_, changed := verified.ChangeMonitor().Get()
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	verified.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
	select {
	case <-changed:
	default:
		t.Fatal("first SetRootKeys did not notify ChangeMonitor when all stored records verified")
	}
	if gotExpire, ips, ok := verified.ActiveRecordOf(publicKey); !ok || gotExpire.UnixMilli() != expire.UnixMilli() || len(ips) != 1 {
		t.Fatalf("ActiveRecordOf after SetRootKeys = (%s, %v, %v)", gotExpire, ips, ok)
	}
	if !verified.IsActiveKey(publicKey) || !verified.CurrentRecordNamesFamily(publicKey, 4) {
		t.Fatal("verified stored record not active after SetRootKeys")
	}
}

func TestExtenderActivatorLoadWaitsForRootKeys(t *testing.T) {
	t.Run("adopts a valid stored record once root keys load", func(t *testing.T) {
		f := newTestPacingLoadFixture(t)
		expire := f.now.Add(25 * time.Hour)
		f.plant(t, expire, testActivateIpv4)
		f.directory.SetRootKeys(nil)

		self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
		stored := f.activatedPacing(expire)
		self.applyPacing(f.record(stored), nil)

		if self.families[4].Activated || !self.families[4].ExpireTime.IsZero() {
			t.Fatalf("family = %+v, expected unactivated while root keys are missing", self.families[4])
		}
		pacing := self.pacing[4]
		if pacing.State != ExtenderPacingUnactivated || !pacing.RecordExpireTime.IsZero() {
			t.Fatalf("pacing = %+v, expected unverified record withheld", pacing)
		}
		if !pacing.NextAllowedAttemptAt.Equal(stored.NextAllowedAttemptAt) {
			t.Fatalf("next allowed = %s, expected stored hold %s", pacing.NextAllowedAttemptAt, stored.NextAllowedAttemptAt)
		}
		if next := self.schedules[4].nextActivateTime; !next.Equal(stored.NextAllowedAttemptAt) {
			t.Fatalf("next post at %s, expected stored hold %s", next, stored.NextAllowedAttemptAt)
		}

		rootPublicKey := f.fixture.operator.rootPrivateKey.Public().(ed25519.PublicKey)
		f.directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
		self.resolveUnverifiedPacing(f.now)

		if !self.families[4].Activated || !self.families[4].ExpireTime.Equal(expire) {
			t.Fatalf("family after root keys loaded = %+v, expected activated", self.families[4])
		}
		if self.pacing[4].State != ExtenderPacingActivated || !self.pacing[4].RecordExpireTime.Equal(expire) {
			t.Fatalf("pacing after root keys loaded = %+v, expected activated", self.pacing[4])
		}
		if next := self.schedules[4].nextActivateTime; !next.Equal(f.now.Add(10 * time.Minute)) {
			t.Fatalf("next post at %s, expected the jittered restart post", next)
		}
	})

	t.Run("clears an unverifiable stored record once root keys load", func(t *testing.T) {
		f := newTestPacingLoadFixture(t)
		expire := f.now.Add(25 * time.Hour)
		f.plant(t, expire, testActivateIpv4)
		f.directory.SetRootKeys(nil)

		self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
		self.applyPacing(f.record(f.activatedPacing(expire)), nil)

		f.directory.SetRootKeys(NewExtenderRootKeySet(newTestExtenderKey(t)))
		self.resolveUnverifiedPacing(f.now)

		if self.families[4].Activated {
			t.Fatalf("family = %+v, expected unactivated after forged record dropped", self.families[4])
		}
		pacing := self.pacing[4]
		if pacing.State != ExtenderPacingUnactivated || !pacing.RecordExpireTime.IsZero() || !pacing.NextAllowedAttemptAt.IsZero() {
			t.Fatalf("pacing = %+v, expected cleared", pacing)
		}
		if next := self.schedules[4].nextActivateTime; !next.IsZero() {
			t.Fatalf("next post at %s, expected due at once", next)
		}
	})

	t.Run("clears an expired activated pacing entry", func(t *testing.T) {
		f := newTestPacingLoadFixture(t)
		expired := f.activatedPacing(f.now.Add(-time.Hour))
		self := newTestPacingLoadActivator(f.publicKey, f.directory, f.now, false)
		self.applyPacing(f.record(expired), nil)

		if self.families[4].Activated {
			t.Fatalf("family = %+v, expected unactivated", self.families[4])
		}
		pacing := self.pacing[4]
		if pacing.State != ExtenderPacingUnactivated || !pacing.NextAllowedAttemptAt.IsZero() {
			t.Fatalf("pacing = %+v, expected expired record cleared", pacing)
		}
		if next := self.schedules[4].nextActivateTime; !next.IsZero() {
			t.Fatalf("next post at %s, expected due at once", next)
		}
	})
}

func TestExtenderActivatorSlowAndFailedRootKeyLoads(t *testing.T) {
	t.Run("slow root key load verifies and adopts stored activation", func(t *testing.T) {
		store := newTestPacingStore()
		var startTime, expire, renewal time.Time
		fixture := newTestActivatorFixtureWithPrepare(
			t,
			func(fixture *testActivatorFixture) {
				startTime = fixture.clock.Now()
				expire = startTime.Add(25 * time.Hour)
				renewal = startTime.Add(19 * time.Hour)
				applyTestOperatorRecord(
					t,
					fixture.directory,
					fixture.operator,
					fixture.publicKey,
					startTime,
					expire,
					testActivateIpv4,
				)
				fixture.directory.SetRootKeys(nil)
				store.record = &ExtenderActivationPacingRecord{
					PublicKey: fixture.publicKey,
					Families: map[int]ExtenderActivationPacing{4: {
						State:                ExtenderPacingActivated,
						RecordExpireTime:     expire,
						Ip:                   netip.MustParseAddr(testActivateIpv4),
						LastAttemptAt:        startTime.Add(-time.Hour),
						NextAllowedAttemptAt: renewal,
					}},
				}
			},
			func(settings *ExtenderActivatorSettings) {
				settings.ApiUrlV6 = ""
				settings.PacingStore = store
				settings.postJitterForTest = func(time.Duration) time.Duration { return 10 * time.Minute }
			},
		)
		fixture.waitPass()
		status := fixture.activator.Status().Family(4)
		if status.Activated || status.PacingState != ExtenderPacingUnactivated || !status.RecordExpireTime.IsZero() {
			t.Fatalf("status before root keys load = %+v, expected unactivated", status)
		}
		if !status.NextAttemptTime.Equal(renewal) {
			t.Fatalf("next attempt = %s, expected persisted hold %s", status.NextAttemptTime, renewal)
		}

		// A network change while root keys are missing must not spend the post budget early.
		fixture.networkChanged()
		fixture.step(time.Minute)
		if count := fixture.operator.postCount(4); count != 0 {
			t.Fatalf("posts while root keys missing = %d, expected none", count)
		}

		// Root keys arrive slightly later and verify the stored record.
		version, _ := fixture.activator.ChangeMonitor().Get()
		fixture.clock.advance(time.Minute)
		loadedAt := fixture.clock.Now()
		rootPublicKey := fixture.operator.rootPrivateKey.Public().(ed25519.PublicKey)
		fixture.directory.SetRootKeys(NewExtenderRootKeySet(rootPublicKey))
		status = fixture.waitOutcome(version).Family(4)
		if !status.Activated || status.PacingState != ExtenderPacingActivated || !status.ExpireTime.Equal(expire) {
			t.Fatalf("status after root keys loaded = %+v, expected verified activation adopted", status)
		}
		fixture.step(9 * time.Minute)
		if count := fixture.operator.postCount(4); count != 0 {
			t.Fatalf("posts before jittered restart time = %d, expected none", count)
		}

		fixture.clock.advance(time.Minute)
		fixture.wake()
		post := fixture.waitPost()
		if !post.postTime.Equal(loadedAt.Add(10 * time.Minute)) {
			t.Fatalf("restart post at %s, expected %s", post.postTime, loadedAt.Add(10*time.Minute))
		}
	})

	t.Run("slow root key load drops forged record and posts immediately", func(t *testing.T) {
		store := newTestPacingStore()
		var startTime, expire, renewal time.Time
		fixture := newTestActivatorFixtureWithPrepare(
			t,
			func(fixture *testActivatorFixture) {
				startTime = fixture.clock.Now()
				expire = startTime.Add(25 * time.Hour)
				renewal = startTime.Add(19 * time.Hour)
				applyTestOperatorRecord(
					t,
					fixture.directory,
					fixture.operator,
					fixture.publicKey,
					startTime,
					expire,
					testActivateIpv4,
				)
				fixture.directory.SetRootKeys(nil)
				store.record = &ExtenderActivationPacingRecord{
					PublicKey: fixture.publicKey,
					Families: map[int]ExtenderActivationPacing{4: {
						State:                ExtenderPacingActivated,
						RecordExpireTime:     expire,
						Ip:                   netip.MustParseAddr(testActivateIpv4),
						LastAttemptAt:        startTime.Add(-time.Hour),
						NextAllowedAttemptAt: renewal,
					}},
				}
			},
			func(settings *ExtenderActivatorSettings) {
				settings.ApiUrlV6 = ""
				settings.PacingStore = store
			},
		)
		fixture.waitPass()
		if count := fixture.operator.postCount(4); count != 0 {
			t.Fatalf("posts before root keys loaded = %d, expected none", count)
		}

		// Root keys arrive and reject the stored record's signature.
		fixture.directory.SetRootKeys(NewExtenderRootKeySet(newTestExtenderKey(t)))
		post := fixture.waitPost()
		if !post.postTime.Equal(startTime) {
			t.Fatalf("post after forged record dropped at %s, expected %s", post.postTime, startTime)
		}
	})

	t.Run("missing root keys hold until NextAllowedAttemptAt then post", func(t *testing.T) {
		store := newTestPacingStore()
		var startTime, expire, holdUntil time.Time
		fixture := newTestActivatorFixtureWithPrepare(
			t,
			func(fixture *testActivatorFixture) {
				startTime = fixture.clock.Now()
				expire = startTime.Add(25 * time.Hour)
				holdUntil = startTime.Add(2 * time.Hour)
				applyTestOperatorRecord(
					t,
					fixture.directory,
					fixture.operator,
					fixture.publicKey,
					startTime,
					expire,
					testActivateIpv4,
				)
				fixture.directory.SetRootKeys(nil)
				store.record = &ExtenderActivationPacingRecord{
					PublicKey: fixture.publicKey,
					Families: map[int]ExtenderActivationPacing{4: {
						State:                ExtenderPacingActivated,
						RecordExpireTime:     expire,
						Ip:                   netip.MustParseAddr(testActivateIpv4),
						LastAttemptAt:        startTime.Add(-time.Hour),
						NextAllowedAttemptAt: holdUntil,
					}},
				}
			},
			func(settings *ExtenderActivatorSettings) {
				settings.ApiUrlV6 = ""
				settings.PacingStore = store
			},
		)
		fixture.waitPass()
		fixture.networkChanged()
		fixture.step(time.Hour)
		fixture.step(59 * time.Minute)
		if status := fixture.activator.Status().Family(4); status.Activated {
			t.Fatalf("status with missing root keys = %+v, expected unactivated", status)
		}
		if count := fixture.operator.postCount(4); count != 0 {
			t.Fatalf("posts before NextAllowedAttemptAt = %d, expected none", count)
		}

		fixture.clock.advance(time.Minute)
		fixture.wake()
		post := fixture.waitPost()
		if !post.postTime.Equal(holdUntil) {
			t.Fatalf("post after hold elapsed at %s, expected %s", post.postTime, holdUntil)
		}
	})
}

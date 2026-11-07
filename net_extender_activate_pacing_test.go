package connect

import (
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// testPacingStore keeps the pacing in memory and counts the saves.
type testPacingStore struct {
	stateLock sync.Mutex
	record    *ExtenderActivationPacingRecord
	saves     int
	saved     chan struct{}
}

func newTestPacingStore() *testPacingStore {
	return &testPacingStore{saved: make(chan struct{}, 64)}
}

// Waits for one save, which is after the pacing it wrote was recorded.
func (self *testPacingStore) waitSave(t *testing.T) {
	t.Helper()
	select {
	case <-self.saved:
	case <-time.After(30 * time.Second):
		t.Fatal("the pacing was not saved")
	}
}

func (self *testPacingStore) LoadPacing() (*ExtenderActivationPacingRecord, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.record, nil
}

func (self *testPacingStore) SavePacing(record *ExtenderActivationPacingRecord) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *record
	copied.Families = map[int]ExtenderActivationPacing{}
	for key, pacing := range record.Families {
		copied.Families[key] = pacing
	}
	self.record = &copied
	self.saves += 1
	select {
	case self.saved <- struct{}{}:
	default:
	}
	return nil
}

func (self *testPacingStore) family(key int) (ExtenderActivationPacing, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.record == nil {
		return ExtenderActivationPacing{}, self.saves
	}
	return self.record.Families[key], self.saves
}

// The instant NextAllowedAttemptAt is reached an attempt is allowed, one nanosecond
// earlier it is not; a record is valid until its expiry, not at it.
func TestExtenderActivationPacingClockBoundary(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pacing := ExtenderActivationPacing{NextAllowedAttemptAt: at, RecordExpireTime: at}
	if pacing.AttemptAllowed(at.Add(-time.Nanosecond)) {
		t.Fatal("an attempt one nanosecond early is allowed")
	}
	if !pacing.AttemptAllowed(at) {
		t.Fatal("an attempt at the instant is refused")
	}
	if !pacing.RecordValid(at.Add(-time.Nanosecond)) || pacing.RecordValid(at) {
		t.Fatal("the record is valid until its expiry, not at it")
	}
	if (ExtenderActivationPacing{}).RecordValid(at) {
		t.Fatal("no record is valid")
	}
}

// A success stops the posts until the renewal point (75% of the lifetime or an
// hour before expiry, whichever is later), and the renewal posts there, not before.
func TestExtenderActivatorSuccessStopsUntilTheRenewalPoint(t *testing.T) {
	store := newTestPacingStore()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) { operator.setRecordLifetime(14 * 24 * time.Hour) },
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
		},
	)
	startTime := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()
	store.waitSave(t)

	// the test operator's record lives two weeks
	renewal := startTime.Add(14*24*time.Hour - time.Hour)
	pacing, saves := store.family(4)
	if saves == 0 || pacing.State != ExtenderPacingActivated ||
		pacing.RecordExpireTime != startTime.Add(14*24*time.Hour) ||
		pacing.LastAttemptAt != startTime || !pacing.NextAllowedAttemptAt.Equal(renewal) || pacing.Hold {
		t.Fatalf("pacing after the success = %+v (%d saves)", pacing, saves)
	}
	status := fixture.activator.Status().Family(4)
	if status.PacingState != ExtenderPacingActivated || !status.NextAttemptTime.Equal(renewal) ||
		status.RecordExpireTime != pacing.RecordExpireTime || status.LastAttemptTime != startTime {
		t.Fatalf("status = %+v", status)
	}

	// well past the old day-long tick, and to a minute before the renewal point
	fixture.step(25 * time.Hour)
	fixture.step(renewal.Sub(fixture.clock.Now()) - time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts before the renewal point = %d, expected one", count)
	}

	// the renewal point itself
	fixture.clock.advance(time.Minute)
	fixture.wake()
	post := fixture.waitPost()
	if post.postTime != renewal {
		t.Fatalf("renewal post at %s, expected the renewal point", post.postTime.Sub(startTime))
	}
}

// A budget refusal sets the next attempt from the operator's retry_after and is
// stored at once; a network change cannot post through it.
func TestExtenderActivatorBudgetRefusalPersistsAndHolds(t *testing.T) {
	const refusal = "You have reached the maximum number of attempts. Please try again in 50 minutes."
	store := newTestPacingStore()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(refusal)
			operator.setRetryAfterSeconds(50 * 60)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
		},
	)
	startTime := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()
	store.waitSave(t)

	pacing, saves := store.family(4)
	if saves != 1 || pacing.State != ExtenderPacingRefused || !pacing.Hold ||
		pacing.LastAttemptAt != startTime || pacing.NextAllowedAttemptAt != startTime.Add(50*time.Minute) ||
		!pacing.RecordExpireTime.IsZero() {
		t.Fatalf("pacing after the refusal = %+v (%d saves)", pacing, saves)
	}

	// a network change makes every family due, and the hold stands against it
	fixture.networkChanged()
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts through the hold = %d, expected one", count)
	}

	// the hold ends where the operator said
	fixture.operator.setRefusal("")
	fixture.clock.advance(48 * time.Minute)
	fixture.wake()
	post := fixture.waitPost()
	if post.postTime != startTime.Add(50*time.Minute) {
		t.Fatalf("post at %s, expected +50m", post.postTime.Sub(startTime))
	}
}

// A renewal that is refused or fails does not clear the record: the family stays
// activated with the same expiry, and the next attempt is held and stored.
func TestExtenderActivatorFailedRenewalKeepsTheRecord(t *testing.T) {
	const refusal = "You have reached the maximum number of attempts. Please try again in 30 minutes."
	store := newTestPacingStore()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) { operator.setRecordLifetime(14 * 24 * time.Hour) },
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
		},
	)
	startTime := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()
	store.waitSave(t)
	expire := startTime.Add(14 * 24 * time.Hour)
	renewal := expire.Add(-time.Hour)

	fixture.operator.setRefusal(refusal)
	fixture.operator.setRetryAfterSeconds(30 * 60)
	version, _ := fixture.activator.ChangeMonitor().Get()
	fixture.clock.advance(renewal.Sub(startTime))
	fixture.wake()
	fixture.waitPost()
	status := fixture.waitOutcome(version).Family(4)
	store.waitSave(t)

	if !status.Activated || !status.LastRefused || status.LastError != refusal || status.ExpireTime != expire {
		t.Fatalf("status after the refused renewal = %+v, expected the record kept", status)
	}
	pacing, _ := store.family(4)
	if pacing.State != ExtenderPacingRenewalRefused || pacing.RecordExpireTime != expire ||
		!pacing.Hold || pacing.LastAttemptAt != renewal || pacing.NextAllowedAttemptAt != renewal.Add(30*time.Minute) {
		t.Fatalf("pacing after the refused renewal = %+v", pacing)
	}
	if fixture.directoryState(testActivateIpv4) == "" {
		t.Fatal("the record left the directory")
	}

	// a failed renewal, not a refusal, keeps it the same
	fixture.operator.setRefusal("")
	fixture.operator.setAnswer(testActivateAnswerUnavailable)
	version, _ = fixture.activator.ChangeMonitor().Get()
	fixture.clock.advance(30 * time.Minute)
	fixture.wake()
	fixture.waitPost()
	status = fixture.waitOutcome(version).Family(4)
	store.waitSave(t)
	pacing, _ = store.family(4)
	if !status.Activated || pacing.State != ExtenderPacingRenewalFailed || pacing.RecordExpireTime != expire || !pacing.Hold {
		t.Fatalf("after the failed renewal: status = %+v, pacing = %+v", status, pacing)
	}
}

// A restart whose stored family holds against the triggers (the operator's budget, a
// failed renewal) reads the stored pacing: no post before NextAllowedAttemptAt, the
// valid record shows as activated, and the post goes out at the instant.
func TestExtenderActivatorRestartHonoursAStoredHold(t *testing.T) {
	store := newTestPacingStore()
	var startTime, next, expire time.Time
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			startTime = fixture.clock.Now()
			next = startTime.Add(2 * time.Hour)
			expire = startTime.Add(3 * time.Hour)
			applyTestOperatorRecord(t, fixture.directory, fixture.operator, fixture.publicKey,
				startTime, expire, testActivateIpv4)
			store.record = &ExtenderActivationPacingRecord{
				PublicKey: fixture.publicKey,
				Families: map[int]ExtenderActivationPacing{4: {
					State:                ExtenderPacingRenewalRefused,
					RecordExpireTime:     expire,
					Ip:                   netip.MustParseAddr(testActivateIpv4),
					LastAttemptAt:        startTime.Add(-time.Hour),
					NextAllowedAttemptAt: next,
					Hold:                 true,
				}},
			}
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
		},
	)
	fixture.waitPass()
	status := fixture.activator.Status().Family(4)
	if !status.Activated || !status.ExpireTime.Equal(expire) || status.PacingState != ExtenderPacingRenewalRefused {
		t.Fatalf("restored status = %+v", status)
	}
	if pacing := fixture.activator.Pacing()[4]; !pacing.NextAllowedAttemptAt.Equal(next) {
		t.Fatalf("restored pacing = %+v", pacing)
	}

	fixture.step(time.Hour)
	fixture.step(59 * time.Minute)
	if count := fixture.operator.postCount(4); count != 0 {
		t.Fatalf("posts on boot before the stored time = %d, expected none", count)
	}
	fixture.clock.advance(time.Minute)
	fixture.wake()
	post := fixture.waitPost()
	if post.postTime != next {
		t.Fatalf("post at %s, expected the stored time", post.postTime.Sub(startTime))
	}
}

// A restart with a valid record does not wait for its renewal, a day out, to learn
// what the new process serves: it posts once, spread by the jitter, and then paces
// from the renewal of the record that post brings (F1).
func TestExtenderActivatorRestartPostsOnceJitteredFromAValidRecord(t *testing.T) {
	store := newTestPacingStore()
	var startTime time.Time
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			startTime = fixture.clock.Now()
			expire := startTime.Add(25 * time.Hour)
			applyTestOperatorRecord(t, fixture.directory, fixture.operator, fixture.publicKey,
				startTime, expire, testActivateIpv4)
			store.record = &ExtenderActivationPacingRecord{
				PublicKey: fixture.publicKey,
				Families: map[int]ExtenderActivationPacing{4: {
					State:                ExtenderPacingActivated,
					RecordExpireTime:     expire,
					Ip:                   netip.MustParseAddr(testActivateIpv4),
					LastAttemptAt:        startTime.Add(-time.Hour),
					NextAllowedAttemptAt: startTime.Add(19 * time.Hour),
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
	if status := fixture.activator.Status().Family(4); !status.Activated {
		t.Fatalf("restored status = %+v", status)
	}
	fixture.step(9 * time.Minute)
	if count := fixture.operator.postCount(4); count != 0 {
		t.Fatalf("posts before the jittered time = %d, expected none", count)
	}
	fixture.clock.advance(time.Minute)
	fixture.wake()
	post := fixture.waitPost()
	if post.postTime != startTime.Add(10*time.Minute) {
		t.Fatalf("post at %s, expected the jittered time", post.postTime.Sub(startTime))
	}
	fixture.waitActivation()
	store.waitSave(t)
	pacing, _ := store.family(4)
	if pacing.State != ExtenderPacingActivated || !pacing.NextAllowedAttemptAt.After(startTime.Add(20*time.Hour)) {
		t.Fatalf("pacing after the restart post = %+v, expected the new record's renewal", pacing)
	}
	// and not another post until that renewal
	fixture.step(time.Hour)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts = %d, expected the one", count)
	}
}

// The carriers the standing post offered are saved with the record, so a carrier that
// failed to bind and is off the record is still known after a restart.
func TestExtenderActivatorPersistsThePostedCarriers(t *testing.T) {
	store := newTestPacingStore()
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			fixture.carriers = []string{ExtenderCarrierTcp, ExtenderCarrierDns}
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.PacingStore = store
		},
	)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()
	store.waitSave(t)
	pacing, _ := store.family(4)
	if !slices.Equal(pacing.PostedCarriers, []string{ExtenderCarrierTcp, ExtenderCarrierDns}) {
		t.Fatalf("posted carriers = %v", pacing.PostedCarriers)
	}
}

// A stored pacing of another identity says nothing of this one: the family posts
// at once, as a new process does.
func TestExtenderActivatorRestartIgnoresAnotherKeysPacing(t *testing.T) {
	store := newTestPacingStore()
	fixture := newTestActivatorFixtureWithPrepare(
		t,
		func(fixture *testActivatorFixture) {
			store.record = &ExtenderActivationPacingRecord{
				PublicKey: []byte("another key"),
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
		},
	)
	fixture.waitPass()
	fixture.waitPost()
}

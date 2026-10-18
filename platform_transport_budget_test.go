package connect

import (
	"context"
	"testing"
	"time"
)

func TestPlatformTransportBudgetH1PrecedesH3(t *testing.T) {
	budget := NewPlatformTransportBudget(10, 2)
	h1 := budget.register(platformTransportBudgetH1, 6, true)
	h3 := budget.register(platformTransportBudgetH3, 6, false)

	h3Acquired := make(chan bool, 1)
	go func() {
		h3Acquired <- h3.Acquire(t.Context())
	}()
	select {
	case <-h3Acquired:
		t.Fatal("H3 consumed capacity reserved by a pending H1 claim")
	case <-time.After(25 * time.Millisecond):
	}
	if !h1.Acquire(t.Context()) {
		t.Fatal("H1 priority claim was not admitted")
	}
	select {
	case <-h3Acquired:
		t.Fatal("H3 was admitted while the higher-precedence H1 reservation was held")
	case <-time.After(25 * time.Millisecond):
	}
	h1.Release()
	select {
	case acquired := <-h3Acquired:
		if !acquired {
			t.Fatal("H3 did not acquire released capacity")
		}
	case <-time.After(time.Second):
		t.Fatal("H3 was not woken after H1 released capacity")
	}
	h3.Release()
	stats := budget.Stats()
	if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
		stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("unbalanced platform budget after release: %+v", stats)
	}
}

// Two explicit policy replacements may register concurrently, but only one
// may borrow its old H1 claim at a time. The second handoff becomes eligible
// when the first old H1 actually releases, so temporary byte/socket usage can
// exceed the ordinary cap by no more than one paired H1 reservation.
func TestPlatformTransportBudgetExplicitH3HandoffsAreQueuedAndBounded(t *testing.T) {
	budget := NewPlatformTransportBudget(10, 2)
	oldH1A := budget.register(platformTransportBudgetH1, 1, true)
	oldH1B := budget.register(platformTransportBudgetH1, 1, true)
	if !oldH1A.Acquire(t.Context()) || !oldH1B.Acquire(t.Context()) {
		t.Fatal("old H1 carriers did not fill the socket cap")
	}

	nextH3A := budget.register(platformTransportBudgetH3Explicit, 5, true)
	nextH3B := budget.register(platformTransportBudgetH3Explicit, 5, true)
	if !nextH3A.AllowHandoffFrom(oldH1A) || !nextH3B.AllowHandoffFrom(oldH1B) {
		t.Fatal("explicit H3 replacements did not register their H1 handoffs")
	}

	acquiredA := make(chan bool, 1)
	go func() { acquiredA <- nextH3A.Acquire(t.Context()) }()
	select {
	case acquired := <-acquiredA:
		if !acquired {
			t.Fatal("first explicit H3 handoff closed instead of acquiring")
		}
	case <-time.After(time.Second):
		t.Fatal("first explicit H3 handoff did not acquire")
	}

	acquiredB := make(chan bool, 1)
	go func() { acquiredB <- nextH3B.Acquire(t.Context()) }()
	if !nextH3B.IsWaiting() {
		t.Fatal("second explicit H3 bypassed the one-at-a-time handoff cap")
	}
	select {
	case <-acquiredB:
		t.Fatal("second explicit H3 acquired during the first temporary overlap")
	default:
	}
	stats := budget.Stats()
	if stats.ActiveHandoffCount != 1 || stats.PendingHandoffCount != 1 ||
		stats.ActiveHandoffByteCount != 1 ||
		stats.ActiveHandoffTransportCount != 1 ||
		stats.HandoffAcquisitionCount != 1 ||
		stats.UsedByteCount != 7 || stats.UsedTransportCount != 3 {
		t.Fatalf("first bounded H1/H3 overlap = %+v", stats)
	}

	oldH1A.Release()
	select {
	case acquired := <-acquiredB:
		if !acquired {
			t.Fatal("queued explicit H3 handoff closed instead of acquiring")
		}
	case <-time.After(time.Second):
		t.Fatal("queued explicit H3 handoff did not acquire after old H1 released")
	}
	stats = budget.Stats()
	if stats.ActiveHandoffCount != 1 || stats.PendingHandoffCount != 0 ||
		stats.ActiveHandoffByteCount != 1 ||
		stats.ActiveHandoffTransportCount != 1 ||
		stats.HandoffAcquisitionCount != 2 ||
		stats.UsedByteCount != 11 || stats.UsedTransportCount != 3 {
		t.Fatalf("second bounded H1/H3 overlap = %+v", stats)
	}

	oldH1B.Release()
	stats = budget.Stats()
	if stats.ActiveHandoffCount != 0 || stats.PendingHandoffCount != 0 ||
		stats.UsedByteCount != 10 || stats.UsedTransportCount != 2 {
		t.Fatalf("handoff overage did not resolve with old H1 release: %+v", stats)
	}
	nextH3A.Release()
	nextH3B.Release()
	stats = budget.Stats()
	if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
		stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("queued handoffs leaked aggregate budget: %+v", stats)
	}
}

func TestPlatformTransportBudgetProviderAutoH3CannotStarveClientPolicy(t *testing.T) {
	tests := []struct {
		name           string
		clientClass    platformTransportBudgetClass
		clientUsesSlot bool
	}{
		{
			name:           "foreground Auto H3 reclaims background provider Auto H3",
			clientClass:    platformTransportBudgetH3Auto,
			clientUsesSlot: false,
		},
		{
			name:           "explicit H3 reclaims background provider Auto H3",
			clientClass:    platformTransportBudgetH3Explicit,
			clientUsesSlot: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// This is the iOS creation order in compact units: the provider is
			// constructed first and holds H1 plus the only H3-sized lease. A
			// later outbound window has H1, but a second H3 does not fit until
			// the provider's optional Auto-H3 lease is reclaimed.
			budget := NewPlatformTransportBudget(8, 3)
			providerH1 := budget.register(platformTransportBudgetH1, 1, true)
			providerAutoH3 := budget.registerWithPriority(
				platformTransportBudgetH3Auto,
				4,
				false,
				PlatformTransportBudgetPriorityBackground,
			)
			if !providerH1.Acquire(t.Context()) || !providerAutoH3.Acquire(t.Context()) {
				t.Fatal("provider did not acquire its initial Auto H1/H3 leases")
			}
			clientH1 := budget.register(platformTransportBudgetH1, 1, true)
			if !clientH1.Acquire(t.Context()) {
				t.Fatal("client H1 did not acquire before its H3 policy was applied")
			}
			clientH3 := budget.registerWithPriority(
				test.clientClass,
				4,
				test.clientUsesSlot,
				PlatformTransportBudgetPriorityForeground,
			)

			clientAcquired := make(chan bool, 1)
			go func() { clientAcquired <- clientH3.Acquire(t.Context()) }()
			select {
			case <-providerAutoH3.PreemptNotify():
				// Exact root-cause barrier: the later client claim selected the
				// provider's already-acquired optional H3 lease for revocation.
			case <-time.After(time.Second):
				t.Fatal("client H3 remained starved behind provider Auto H3")
			}
			if !providerAutoH3.Yield() {
				t.Fatal("provider Auto H3 did not yield its preempted lease")
			}
			select {
			case acquired := <-clientAcquired:
				if !acquired {
					t.Fatal("client H3 claim closed instead of acquiring reclaimed capacity")
				}
			case <-time.After(time.Second):
				t.Fatal("client H3 did not acquire the provider's yielded capacity")
			}

			clientH3.Release()
			clientH1.Release()
			providerAutoH3.Release()
			providerH1.Release()
			stats := budget.Stats()
			if stats.PreemptedH3Count != 1 || stats.UsedByteCount != 0 ||
				stats.UsedTransportCount != 0 ||
				stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("unbalanced provider-first policy handoff: %+v", stats)
			}
		})
	}
}

// This is the exact policy-switch sequence from the iOS regression. The first
// Auto transition happens while old/new H1 transports fill the socket cap and
// another H1 migration is queued. Auto H3 shares its transport's H1 socket, so
// that structurally blocked H1 claim must not prevent H3 from starting. The
// explicit H3 stage must then reclaim optional Auto H3, and the final Auto
// stage must regain H3 after the explicit carrier is replaced.
func TestPlatformTransportBudgetPolicySwitchH1AutoH3Auto(t *testing.T) {
	budget := NewPlatformTransportBudget(8, 2)

	// H1-only: two live window transports fill the socket cap. A third H1
	// policy migration has registered, but cannot structurally acquire a slot.
	otherH1 := budget.register(platformTransportBudgetH1, 1, true)
	policyH1 := budget.register(platformTransportBudgetH1, 1, true)
	queuedH1 := budget.register(platformTransportBudgetH1, 1, true)
	if !otherH1.Acquire(t.Context()) || !policyH1.Acquire(t.Context()) {
		t.Fatal("H1-only policy did not fill the test socket cap")
	}
	if !queuedH1.IsWaiting() {
		t.Fatal("queued H1 unexpectedly fit beyond the socket cap")
	}

	// H1 -> Auto: H3 is slotless because this Auto transport already owns its
	// H1 slot. Before the fix, requiredCapacityLocked counted queuedH1 anyway,
	// reported three required slots, and this exact claim waited forever.
	autoH3 := budget.register(platformTransportBudgetH3Auto, 4, false)
	if autoH3.IsWaiting() {
		t.Fatal("H1 -> Auto left H3 blocked by an H1 claim beyond the socket cap")
	}
	if !autoH3.Acquire(t.Context()) {
		t.Fatal("H1 -> Auto did not start H3")
	}

	// Auto -> explicit H3: the required explicit choice revokes optional Auto
	// H3. It still waits for the old policy's H1 slot before connecting.
	explicitH3 := budget.register(platformTransportBudgetH3Explicit, 4, true)
	explicitAcquired := make(chan bool, 1)
	go func() { explicitAcquired <- explicitH3.Acquire(t.Context()) }()
	select {
	case <-autoH3.PreemptNotify():
	case <-time.After(time.Second):
		t.Fatal("explicit H3 did not preempt the optional Auto H3 lease")
	}
	if !autoH3.Yield() {
		t.Fatal("optional Auto H3 did not yield to explicit H3")
	}
	select {
	case <-explicitAcquired:
		t.Fatal("explicit H3 acquired before the old H1 policy released its slot")
	default:
	}
	policyH1.Release()
	select {
	case acquired := <-explicitAcquired:
		if !acquired {
			t.Fatal("explicit H3 closed instead of acquiring")
		}
	case <-time.After(time.Second):
		t.Fatal("Auto -> explicit H3 did not acquire after old H1 closed")
	}
	autoH3.Release()

	// The unrelated old H1 window leaves before the last policy replacement.
	// Auto's H1 can then connect make-before-break with explicit H3.
	otherH1.Release()
	finalAutoH1 := budget.register(platformTransportBudgetH1, 1, true)
	if !finalAutoH1.Acquire(t.Context()) {
		t.Fatal("explicit H3 -> Auto did not start H1")
	}
	finalAutoH3 := budget.register(platformTransportBudgetH3Auto, 4, false)
	finalAutoH3Acquired := make(chan bool, 1)
	go func() { finalAutoH3Acquired <- finalAutoH3.Acquire(t.Context()) }()
	select {
	case <-finalAutoH3Acquired:
		t.Fatal("final Auto H3 acquired before the explicit H3 lease was released")
	default:
	}
	explicitH3.Release()
	select {
	case acquired := <-finalAutoH3Acquired:
		if !acquired {
			t.Fatal("final Auto H3 closed instead of acquiring")
		}
	case <-time.After(time.Second):
		t.Fatal("explicit H3 -> Auto did not restart H3")
	}

	finalAutoH3.Release()
	finalAutoH1.Release()
	queuedH1.Release()
	stats := budget.Stats()
	if stats.PreemptedH3Count != 1 || stats.PendingH1Count != 0 ||
		stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
		stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("policy-switch sequence leaked aggregate budget: %+v", stats)
	}
}

func TestPlatformTransportBudgetTransportCountThrottlesCandidates(t *testing.T) {
	budget := NewPlatformTransportBudget(100, 1)
	first := budget.register(platformTransportBudgetH1, 1, true)
	second := budget.register(platformTransportBudgetH1, 1, true)
	if !first.Acquire(t.Context()) {
		t.Fatal("first candidate was not admitted")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	secondAcquired := make(chan bool, 1)
	go func() { secondAcquired <- second.Acquire(ctx) }()
	select {
	case <-secondAcquired:
		t.Fatal("candidate count cap admitted a second transport")
	case <-time.After(25 * time.Millisecond):
	}
	first.Release()
	select {
	case acquired := <-secondAcquired:
		if !acquired {
			t.Fatal("second candidate did not acquire the released slot")
		}
	case <-time.After(time.Second):
		t.Fatal("second candidate was not woken after slot release")
	}
	second.Release()
}

// TestPlatformTransportBudgetDoesNotPreemptForUnresolvableSlotDeficit pins the
// provider CPU regression from urnetwork/connect#211. Once the process-wide
// transport count is full, a pending H1 claim has both byte and slot deficits.
// A slotless Auto-H3 lease can resolve only the bytes; preempting it makes that
// lease yield and immediately reacquire forever without admitting the H1.
func TestPlatformTransportBudgetCanceledH1ClaimUnblocksH3(t *testing.T) {
	budget := NewPlatformTransportBudget(10, 2)
	existingH1 := budget.register(platformTransportBudgetH1, 6, true)
	if !existingH1.Acquire(t.Context()) {
		t.Fatal("existing H1 claim was not admitted")
	}
	pendingH1 := budget.register(platformTransportBudgetH1, 6, true)
	h3 := budget.register(platformTransportBudgetH3, 4, false)

	h3Acquired := make(chan bool, 1)
	go func() { h3Acquired <- h3.Acquire(t.Context()) }()
	select {
	case <-h3Acquired:
		t.Fatal("H3 ignored a pending H1 priority claim")
	case <-time.After(25 * time.Millisecond):
	}

	h1Ctx, cancelH1 := context.WithCancel(t.Context())
	h1Done := make(chan bool, 1)
	go func() { h1Done <- pendingH1.Acquire(h1Ctx) }()
	cancelH1()
	select {
	case acquired := <-h1Done:
		if acquired {
			t.Fatal("canceled H1 claim acquired the budget")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled H1 claim did not unregister")
	}
	select {
	case acquired := <-h3Acquired:
		if !acquired {
			t.Fatal("H3 did not acquire after the H1 claim was canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("H3 was not woken after the H1 claim was canceled")
	}

	h3.Release()
	existingH1.Release()
	stats := budget.Stats()
	if stats.PendingH1Count != 0 || stats.PendingH1ByteCount != 0 ||
		stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("budget leaked a canceled priority claim: %+v", stats)
	}
}

func TestPlatformTransportBudgetRejectsAlreadyCanceledClaim(t *testing.T) {
	budget := NewPlatformTransportBudget(10, 1)
	claim := budget.register(platformTransportBudgetH1, 1, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if claim.Acquire(ctx) {
		t.Fatal("already-canceled H1 claim acquired available capacity")
	}
	stats := budget.Stats()
	if stats.PendingH1Count != 0 || stats.PendingH1ByteCount != 0 ||
		stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 {
		t.Fatalf("already-canceled claim remained registered: %+v", stats)
	}
}

// --- ported from the pinned v2026 engine's platform_transport_budget_test.go ---
// The pinned engine's other additions to this file exercise the transport
// wiring (the budgetOnlyPlatformTransport helper and its full policy matrix,
// CanMakeBeforeBreakFrom, the reservation fields on PlatformTransport, the
// registerWithPriority handoff transitions, the runH3ModeForTest seam); this
// generation has not swapped in that wiring, so they are left to that slice.
// The three below are pure budget-API tests -- two budget-only, one settings
// defaults only -- and are ported as is.

func TestPlatformTransportBudgetAutoH3YieldClearsEitherHandoffEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		oldClass platformTransportBudgetClass
		oldBytes ByteCount
		newClass platformTransportBudgetClass
		newBytes ByteCount
		yieldOld bool
	}{
		{
			name:     "auto H3 replacement",
			oldClass: platformTransportBudgetH1,
			oldBytes: 1,
			newClass: platformTransportBudgetH3Auto,
			newBytes: 4,
		},
		{
			name:     "auto H3 previous carrier",
			oldClass: platformTransportBudgetH3Auto,
			oldBytes: 4,
			newClass: platformTransportBudgetH1,
			newBytes: 1,
			yieldOld: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := NewPlatformTransportBudget(4, 1)
			old := budget.register(test.oldClass, test.oldBytes, true)
			if !old.Acquire(t.Context()) {
				t.Fatal("old reservation did not acquire")
			}
			next := budget.register(test.newClass, test.newBytes, true)
			if !next.AllowHandoffFrom(old) || !next.Acquire(t.Context()) {
				t.Fatal("bounded H1 handoff did not acquire")
			}
			if budget.Stats().ActiveHandoffCount != 1 {
				t.Fatal("handoff was not active before yield")
			}
			toYield := next
			if test.yieldOld {
				toYield = old
			}
			if !toYield.Yield() {
				t.Fatal("optional Auto H3 did not yield")
			}
			if stats := budget.Stats(); stats.ActiveHandoffCount != 0 ||
				stats.PendingHandoffCount != 0 {
				t.Fatalf("yield retained a stale budget handoff: %+v", stats)
			}
			old.Release()
			next.Release()
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
				stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("yield handoff leaked aggregate budget: %+v", stats)
			}
		})
	}
}

func TestPlatformTransportBudgetDoesNotPreemptForUnresolvableSlotDeficit(t *testing.T) {
	const h1Count = 16
	budget := NewPlatformTransportBudget(24, h1Count)
	h1Claims := make([]*platformTransportBudgetReservation, 0, h1Count)
	for range h1Count {
		h1 := budget.register(platformTransportBudgetH1, 1, true)
		if !h1.Acquire(t.Context()) {
			t.Fatal("initial H1 claim was not admitted")
		}
		h1Claims = append(h1Claims, h1)
	}
	h3 := budget.register(platformTransportBudgetH3Auto, 8, false)
	if !h3.Acquire(t.Context()) {
		t.Fatal("slotless Auto-H3 claim was not admitted")
	}
	blockedH1 := budget.register(platformTransportBudgetH1, 1, true)

	budget.mutex.Lock()
	blockedH1.requestPreemptionLocked()
	preemptRequested := h3.preemptRequested
	budget.mutex.Unlock()
	if preemptRequested {
		t.Fatal("slotless Auto-H3 was preempted even though it cannot free the required slot")
	}
	select {
	case <-h3.PreemptNotify():
		t.Fatal("slotless Auto-H3 received an unresolvable preemption request")
	default:
	}
	if stats := budget.Stats(); stats.PreemptedH3Count != 0 {
		t.Fatalf("unresolvable preemption count = %d, want 0", stats.PreemptedH3Count)
	}

	blockedH1.Release()
	h3.Release()
	for _, h1 := range h1Claims {
		h1.Release()
	}
}

// The default settings' carrier budget follows the process memory target, so
// Auto keeps its H3 reservation on an iOS-sized target and runs H1 only on the
// legacy low target.
func TestPlatformTransportBudgetAutoModesFollowMemoryTarget(t *testing.T) {
	tests := []struct {
		name          string
		memoryTarget  ByteCount
		wantH3Acquire bool
	}{
		{name: "iOS target keeps full Auto", memoryTarget: mib(32), wantH3Acquire: true},
		{name: "legacy low target runs H1 only", memoryTarget: mib(8), wantH3Acquire: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			SetMemoryBudget(test.memoryTarget)
			t.Cleanup(func() { SetMemoryBudget(0) })
			settings := DefaultPlatformTransportSettings()
			budget := settings.PlatformTransportBudget
			h1 := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
			h3 := budget.register(platformTransportBudgetH3, settings.H3BudgetByteCount, false)
			if !h1.Acquire(t.Context()) {
				t.Fatal("H1 did not acquire its Auto reservation")
			}
			defer h1.Release()

			h3Ctx, cancelH3 := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancelH3()
			gotH3Acquire := h3.Acquire(h3Ctx)
			if gotH3Acquire {
				defer h3.Release()
			}
			if gotH3Acquire != test.wantH3Acquire {
				t.Fatalf(
					"H3 acquired=%t at memory target %d, want %t (budget=%+v)",
					gotH3Acquire,
					test.memoryTarget,
					test.wantH3Acquire,
					budget.Stats(),
				)
			}
		})
	}
}

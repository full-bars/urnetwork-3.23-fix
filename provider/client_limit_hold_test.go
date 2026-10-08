package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// 15. TestClientLimitHoldSurvivesRestart: registry A notes hold for X at t0;
// fresh registry B loads; X exceeded with identical RetryTime; Y not held;
// load at RetryTime+1s prunes X.
func TestClientLimitHoldSurvivesRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	t0 := time.Now().Truncate(time.Second)
	retryTime := t0.Add(15 * time.Minute)

	idX := connect.NewId()
	idY := connect.NewId()

	// Registry A notes hold for X at t0
	bX := clientLimitHoldFor(idX, t0)
	bX.Restore(retryTime)

	// Persist happens via watcher or direct call
	if err := persistClientLimitHold(idX.String(), bX.Status(), t0); err != nil {
		t.Fatalf("persistClientLimitHold failed: %v", err)
	}

	// Fresh registry B loads
	resetClientLimitHoldsForTest()
	bXLoaded := clientLimitHoldFor(idX, t0)
	stX := bXLoaded.Status()
	if !stX.Exceeded {
		t.Fatal("expected X to be exceeded after restart")
	}
	if !stX.RetryTime.Equal(retryTime) {
		t.Fatalf("expected X retry time %v, got %v", retryTime, stX.RetryTime)
	}

	// Y not held
	bYLoaded := clientLimitHoldFor(idY, t0)
	if stY := bYLoaded.Status(); stY.Exceeded {
		t.Fatalf("expected Y not held, got %+v", stY)
	}

	// Load at RetryTime+1s prunes X
	resetClientLimitHoldsForTest()
	bXExpired := clientLimitHoldFor(idX, retryTime.Add(time.Second))
	if stExpired := bXExpired.Status(); stExpired.Exceeded {
		t.Fatalf("expected X to be pruned and not held at retryTime+1s, got %+v", stExpired)
	}
}

// 16. TestClientLimitHoldMergeKeepsLater (parent and candidate writers).
func TestClientLimitHoldMergeKeepsLater(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	t0 := time.Unix(1700000000, 0)
	idX := connect.NewId().String()

	retryEarlier := t0.Add(10 * time.Minute)
	retryLater := t0.Add(20 * time.Minute)

	// Writer 1 (e.g. parent) writes earlier hold
	err := persistClientLimitHold(idX, connect.ClientLimitStatus{
		Exceeded:  true,
		RetryTime: retryEarlier,
	}, t0)
	if err != nil {
		t.Fatalf("writer 1 persist failed: %v", err)
	}

	// Writer 2 (e.g. candidate) writes later hold -> keeps later
	err = persistClientLimitHold(idX, connect.ClientLimitStatus{
		Exceeded:  true,
		RetryTime: retryLater,
	}, t0)
	if err != nil {
		t.Fatalf("writer 2 persist failed: %v", err)
	}

	path, err := clientLimitHoldPath()
	if err != nil {
		t.Fatalf("clientLimitHoldPath failed: %v", err)
	}
	var file clientLimitHoldFile
	oomReadJSON(path, &file)
	if file[idX] != retryLater.Unix() {
		t.Fatalf("expected retry time %d, got %d", retryLater.Unix(), file[idX])
	}

	// Writer 1 attempts to write earlier hold again -> merge-max keeps later
	err = persistClientLimitHold(idX, connect.ClientLimitStatus{
		Exceeded:  true,
		RetryTime: retryEarlier,
	}, t0)
	if err != nil {
		t.Fatalf("writer 1 repeat persist failed: %v", err)
	}

	oomReadJSON(path, &file)
	if file[idX] != retryLater.Unix() {
		t.Fatalf("expected merge-max to keep later %d, got %d", retryLater.Unix(), file[idX])
	}
}

// 17. TestClientLimitHoldCorruptFileReadsEmpty
func TestClientLimitHoldCorruptFileReadsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	path, err := clientLimitHoldPath()
	if err != nil {
		t.Fatalf("clientLimitHoldPath failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	// Write corrupt data
	if err := os.WriteFile(path, []byte("{corrupt-json"), 0o600); err != nil {
		t.Fatalf("write corrupt file failed: %v", err)
	}

	id := connect.NewId()
	b := clientLimitHoldFor(id, time.Now())
	if st := b.Status(); st.Exceeded {
		t.Fatalf("expected corrupt file to read as empty (not exceeded), got %+v", st)
	}
}

// 19. TestNewProviderClientAuthDeclaresIntent (mint and renewal share it);
// TestAuthClientArgsProvideIntentJSON (true present, false omitted).
func TestNewProviderClientAuthDeclaresIntent(t *testing.T) {
	id := connect.NewId()

	// default on
	t.Setenv("URNETWORK_PROVIDE_INTENT", "")
	authDefault := newProviderClientAuth("jwt", id)
	if !authDefault.ProvideIntent {
		t.Fatal("expected default ProvideIntent to be true")
	}
	argsDefault := newProviderAuthClientArgsForRenewal("desc", id)
	if !argsDefault.ProvideIntent {
		t.Fatal("expected default renewal args ProvideIntent to be true")
	}

	// disabled with 0
	t.Setenv("URNETWORK_PROVIDE_INTENT", "0")
	authDisabled := newProviderClientAuth("jwt", id)
	if authDisabled.ProvideIntent {
		t.Fatal("expected ProvideIntent to be false when URNETWORK_PROVIDE_INTENT=0")
	}
	argsDisabled := newProviderAuthClientArgsForRenewal("desc", id)
	if argsDisabled.ProvideIntent {
		t.Fatal("expected renewal args ProvideIntent to be false when URNETWORK_PROVIDE_INTENT=0")
	}

	// enabled with 1
	t.Setenv("URNETWORK_PROVIDE_INTENT", "1")
	authEnabled := newProviderClientAuth("jwt", id)
	if !authEnabled.ProvideIntent {
		t.Fatal("expected ProvideIntent to be true when URNETWORK_PROVIDE_INTENT=1")
	}
	argsEnabled := newProviderAuthClientArgsForRenewal("desc", id)
	if !argsEnabled.ProvideIntent {
		t.Fatal("expected renewal args ProvideIntent to be true when URNETWORK_PROVIDE_INTENT=1")
	}
}

func TestAuthClientArgsProvideIntentJSON(t *testing.T) {
	argsTrue := &connect.AuthNetworkClientArgs{
		Description:   "test",
		ProvideIntent: true,
	}
	bytesTrue, err := json.Marshal(argsTrue)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(bytesTrue), `"provide_intent":true`) {
		t.Fatalf("expected provide_intent:true in %s", string(bytesTrue))
	}

	argsFalse := &connect.AuthNetworkClientArgs{
		Description:   "test",
		ProvideIntent: false,
	}
	bytesFalse, err := json.Marshal(argsFalse)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(bytesFalse), "provide_intent") {
		t.Fatalf("expected provide_intent omitted when false in %s", string(bytesFalse))
	}
}

func clientLimitHoldCount() int {
	clientLimitHolds.Lock()
	defer clientLimitHolds.Unlock()
	return len(clientLimitHolds.m)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The registry must not keep an entry or a watcher for a client whose last
// user is gone, whatever the number of distinct client ids seen over the life
// of the process.
func TestClientLimitHoldReleasedWhenContextEnds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	const n = 50
	cancels := make([]context.CancelFunc, 0, n)
	for i := 0; i < n; i += 1 {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		clientLimitHoldWithContext(ctx, connect.NewId(), time.Now())
	}
	if got := clientLimitHoldCount(); got != n {
		t.Fatalf("entries = %d, want %d", got, n)
	}
	for _, cancel := range cancels {
		cancel()
	}
	waitFor(t, "all entries released", func() bool { return clientLimitHoldCount() == 0 })
}

// A replacement for the same client id shares the hold, and the entry stays
// until the LAST user ends, so the first user ending does not strip the
// replacement of its persistence.
func TestClientLimitHoldSharedUntilLastUserEnds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	id := connect.NewId()
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	b1 := clientLimitHoldWithContext(ctx1, id, time.Now())
	b2 := clientLimitHoldWithContext(ctx2, id, time.Now())
	if b1 != b2 {
		t.Fatal("the same client id must share one hold")
	}
	cancel1()
	time.Sleep(50 * time.Millisecond)
	if got := clientLimitHoldCount(); got != 1 {
		t.Fatalf("entry dropped while a user remains: entries = %d", got)
	}
	cancel2()
	waitFor(t, "entry released after last user", func() bool { return clientLimitHoldCount() == 0 })
}

// A standing hold is flushed when the last user ends, and a later instance of
// the same client restores it, so a proxy reload does not forget a hold.
func TestClientLimitHoldFlushedOnRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	id := connect.NewId()
	retry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	b := clientLimitHoldWithContext(ctx, id, time.Now())
	b.Restore(retry)
	cancel()
	waitFor(t, "entry released", func() bool { return clientLimitHoldCount() == 0 })

	b2 := clientLimitHoldFor(id, time.Now())
	st := b2.Status()
	if !st.Exceeded || !st.RetryTime.Equal(retry) {
		t.Fatalf("hold not restored after release: %+v, want retry %v", st, retry)
	}
}

// Clients that were never held must not touch the shared file.
func TestClientLimitHoldIdleClientsWriteNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetClientLimitHoldsForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 20; i += 1 {
		clientLimitHoldWithContext(ctx, connect.NewId(), time.Now())
	}
	time.Sleep(100 * time.Millisecond)
	path, err := clientLimitHoldPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("idle clients wrote %s (err=%v)", path, err)
	}
}

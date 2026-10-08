package main

import (
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

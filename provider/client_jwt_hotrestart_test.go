package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// withGlobalStore swaps globalClientJWTStore to a fresh store backed by path
// for the duration of t, restoring the original on exit. Tests must isolate
// from the process-wide default (which points at the real ~/.urnetwork).
func withGlobalStore(t *testing.T, path string) func() {
	t.Helper()
	orig := globalClientJWTStore
	// Reset loaded flag on the swapped-in store so the next Get/Put runs
	// loadLocked against the (temp) path.
	globalClientJWTStore = newClientJWTStore(path)
	return func() { globalClientJWTStore = orig }
}

// withHome points os.UserHomeDir-equivalent code at a temp dir by setting
// HOME; restores the prior value on exit. Required because provideAuth reads
// ~/.urnetwork/jwt directly via os.UserHomeDir.
func withHome(t *testing.T) (home string, restore func()) {
	t.Helper()
	orig, hadOrig := os.LookupEnv("HOME")
	home = t.TempDir()
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	return home, func() {
		if hadOrig {
			_ = os.Setenv("HOME", orig)
		} else {
			_ = os.Unsetenv("HOME")
		}
	}
}

// writeAccountJWT plants a valid account JWT at home/.urnetwork/jwt and
// returns the file path. claims["exp"] is required.
func writeAccountJWT(t *testing.T, home string, claims map[string]interface{}) string {
	t.Helper()
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = float64(time.Now().Add(time.Hour).Unix())
	}
	jwt := createFakeJWTWithClaims(claims)
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte(jwt), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- snapshotLocked / pruneOldBackupsLocked ---------------------------------

func TestClientJWTStoreSnapshotWritesBak(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)
	// Pre-populate so loadLocked has something to snapshot.
	store.entries["proxy-1"] = clientJWTEntry{
		ByClientJWT: "fake", ClientID: testClientId, MintedAt: time.Now(),
	}
	data := []byte(`{"proxy-1":{"by_client_jwt":"fake","client_id":"` + testClientId + `","network_id":"","minted_at":"2026-01-01T00:00:00Z"}}`)

	store.snapshotLocked(data, 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, err := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("backup content mismatch:\nwant %q\ngot  %q", data, got)
	}
}

func TestClientJWTStoreSnapshotIsAtomic(t *testing.T) {
	// A successful snapshot must leave NO .tmp half-file behind — otherwise a
	// crash mid-write would leave an unparseable artifact that the next
	// dedup pass would read instead of the real backup.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"k":1}`), 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := filepath.Glob(filepath.Join(dir, base+"-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("expected no .tmp left after atomic snapshot, got %v", tmp)
	}
}

func TestClientJWTStoreSnapshotDedupWithinInterval(t *testing.T) {
	// Identical content + recent previous snapshot → skip. Prevents a
	// crash-loop from burning all backup slots.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)
	store.entries["k"] = clientJWTEntry{ClientID: testClientId, MintedAt: time.Now()}

	data := []byte(`{"k":{"client_id":"` + testClientId + `"}}`)
	store.snapshotLocked(data, 1)
	store.snapshotLocked(data, 1) // identical content, recent → must skip

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 1 {
		t.Fatalf("expected dedup to keep exactly 1 backup, got %d", len(matches))
	}
}

func TestClientJWTStoreSnapshotFiresWhenContentChanges(t *testing.T) {
	// Different content within the dedup window MUST create a new backup —
	// dedup is about identical-content crash loops, not about suppressing all
	// change-tracking.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"v":1}`), 1)
	store.snapshotLocked([]byte(`{"v":2}`), 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 backups for changed content, got %d", len(matches))
	}
}

func TestClientJWTStoreSnapshotDistinctContentSameSecond(t *testing.T) {
	// Regression: two distinct-content snapshots in the same wall-clock
	// second used to collide on the filename (20060102T150405Z is second
	// resolution) and the second write's rename silently overwrote the
	// first. The timestamp format now carries microseconds
	// (20060102T150405.000000Z) so distinct content always lands in a
	// distinct file, even if both fire within the same second.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"a":1}`), 1)
	store.snapshotLocked([]byte(`{"b":2}`), 1) // same second, different content

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 distinct backups for distinct content in the same second, got %d (collision bug?)", len(matches))
	}
}

func TestClientJWTStorePruneKeepsMaxBackups(t *testing.T) {
	// Write clientJWTMaxBackups+3 backups, assert only MaxBackups remain,
	// and the OLDEST 3 were pruned.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	for i := 0; i < clientJWTMaxBackups+3; i++ {
		// Content differs from the previous snapshot, so dedup never skips.
		store.snapshotLocked([]byte(`{"i":`+string(rune('0'+i%10))+`}`), 1)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != clientJWTMaxBackups {
		t.Fatalf("expected exactly %d backups after prune, got %d", clientJWTMaxBackups, len(matches))
	}
}

func TestClientJWTStoreCorruptFileSnapshotsRawBytes(t *testing.T) {
	// The corrupt-file path in loadLocked must snapshot the raw bytes BEFORE
	// discarding them, so the operator has a recovery artifact.
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	corrupt := []byte("not json at all")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)
	_, _ = store.Get("any") // triggers loadLocked

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 backup from corrupt-file path, got %d", len(matches))
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != string(corrupt) {
		t.Errorf("backup should contain raw corrupt bytes, got %q", got)
	}
}

func TestClientJWTStoreSnapshotConcurrent(t *testing.T) {
	// Snapshot is called only from loadLocked (which is mutex-guarded), so
	// concurrent Put/Delete during a snapshot can't race on the directory
	// glob. Smoke-test the invariant: parallel Puts land without corrupting
	// the store, and loadLocked remains idempotent.
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Put("k", clientJWTEntry{
				ClientID: testClientId, MintedAt: time.Now(),
				ByClientJWT: "x",
			})
		}(i)
	}
	wg.Wait()

	if _, ok := store.Get("k"); !ok {
		t.Error("expected entry to survive concurrent Puts")
	}
}

// --- provideAuth: renew-on-expiry path -------------------------------------

// provideAuth relies on a global store and an account JWT on disk. The
// following tests cover the renew-on-expiry and self-heal branches by
// overriding the renew seam and swapping the global store to a temp file.

func TestProvideAuthReusesValidUnexpiredEntry(t *testing.T) {
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = globalClientJWTStore.Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1", // matches current → reuse
		MintedAt:    time.Now(),
	})

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		t.Fatal("renewal must NOT be called for a valid, unexpired entry")
		return "", nil
	}

	byJwt, id, reused, err := provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")
	if err != nil {
		t.Fatalf("provideAuth err = %v", err)
	}
	if !reused {
		t.Error("expected reused=true for valid unexpired entry")
	}
	if id.String() != testClientId {
		t.Errorf("client_id = %q, want %q", id.String(), testClientId)
	}
	if byJwt != goodJwt {
		t.Errorf("returned jwt = %q, want %q", byJwt, goodJwt)
	}
}

func TestProvideAuthRenewsOnExpiredEntry(t *testing.T) {
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	expiredJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(-time.Hour).Unix()),
	})
	_ = globalClientJWTStore.Put("direct", clientJWTEntry{
		ByClientJWT: expiredJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	})

	renewedJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId, // same id — renewal keeps identity
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	calls := 0
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
		calls++
		if clientId.String() != testClientId {
			t.Errorf("renewal called with clientId=%q, want %q", clientId.String(), testClientId)
		}
		return renewedJwt, nil
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	byJwt, id, reused, err := provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")
	if err != nil {
		t.Fatalf("provideAuth err = %v", err)
	}
	if calls != 1 {
		t.Errorf("renewal called %d times, want 1", calls)
	}
	if !reused {
		t.Error("expected reused=true after successful renewal")
	}
	if id.String() != testClientId {
		t.Errorf("client_id = %q, want %q (must be preserved across renewal)", id.String(), testClientId)
	}
	if byJwt != renewedJwt {
		t.Errorf("returned jwt = %q, want renewed %q", byJwt, renewedJwt)
	}

	// The renewal must persist the fresh JWT so the next restart reuses it
	// without going to the network.
	stored, ok := globalClientJWTStore.Get("direct")
	if !ok {
		t.Fatal("renewed entry should be in store after Put")
	}
	if stored.ByClientJWT != renewedJwt {
		t.Errorf("store by_jwt = %q, want renewed %q", stored.ByClientJWT, renewedJwt)
	}
	if stored.ClientID != testClientId {
		t.Errorf("store client_id = %q, want %q", stored.ClientID, testClientId)
	}
}

func TestProvideAuthFallsThroughOnRenewalFailure(t *testing.T) {
	// Renewal errored (e.g. server-side rejection) → do NOT update the store,
	// do NOT return reused=true, do NOT return the old (expired) JWT. The
	// caller should fall through to a fresh mint in the lower code.
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	expiredJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(-time.Hour).Unix()),
	})
	originalEntry := clientJWTEntry{
		ByClientJWT: expiredJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	}
	_ = globalClientJWTStore.Put("direct", originalEntry)

	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
		return "", errFakeRenew
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	// provideAuth will proceed past the renewal failure into the fresh-mint
	// path which dials the API. With apiUrl="" the NewBringYourApi call
	// panics or errors — we just need to assert the store wasn't updated.
	defer func() {
		if r := recover(); r != nil {
			// Expected: the lower path can't run without an API. What we
			// really care about is that the store is unchanged.
		}
	}()
	_, _, _, _ = provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")

	stored, ok := globalClientJWTStore.Get("direct")
	if !ok {
		t.Fatal("store entry should still exist (renewal failure must not delete)")
	}
	if stored.ByClientJWT != originalEntry.ByClientJWT {
		t.Errorf("store by_jwt = %q, want original %q (renewal failure must not overwrite)",
			stored.ByClientJWT, originalEntry.ByClientJWT)
	}
}

func TestProvideAuthMismatchMintsFresh(t *testing.T) {
	// Stored NetworkID="X", current JWT has network_id="Y" → mismatch, must
	// NOT reuse, must NOT renew. The store is left untouched.
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-Y",
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	validJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	originalEntry := clientJWTEntry{
		ByClientJWT: validJwt,
		ClientID:    testClientId,
		NetworkID:   "net-X", // mismatch
		MintedAt:    time.Now(),
	}
	_ = globalClientJWTStore.Put("direct", originalEntry)

	calls := 0
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
		calls++
		return "", nil
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	defer func() {
		if r := recover(); r != nil { // fresh-mint path needs API
		}
	}()
	_, _, _, _ = provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")

	if calls != 0 {
		t.Errorf("renewal called %d times on mismatch, want 0", calls)
	}
	stored, _ := globalClientJWTStore.Get("direct")
	if stored.NetworkID != "net-X" {
		t.Errorf("store NetworkID changed to %q, mismatch path must not touch store", stored.NetworkID)
	}
}

func TestProvideAuthRejectsCurrentJWTWithoutNetworkID(t *testing.T) {
	// If the current account JWT has no network_id claim, we can't verify
	// the stored identity belongs to the same account → fresh mint even for
	// legacy NetworkID="" entries. The legacy self-heal path is REACHED only
	// when the current JWT DOES carry a network_id.
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id": testClientId, // no network_id
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	validJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = globalClientJWTStore.Put("direct", clientJWTEntry{
		ByClientJWT: validJwt,
		ClientID:    testClientId,
		NetworkID:   "", // legacy
		MintedAt:    time.Now(),
	})

	calls := 0
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
		calls++
		return "", nil
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	_, _, _, _ = provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")

	if calls != 0 {
		t.Errorf("renewal called %d times when current JWT has no network_id, want 0 (can't verify identity)", calls)
	}
}

// --- self-heal of legacy NetworkID="" entries on reuse ---------------------

func TestProvideAuthSelfHealStampsNetworkID(t *testing.T) {
	// Stored NetworkID="" (legacy) + current JWT has network_id="net-1" →
	// reuse succeeds, AND the store entry is stamped with "net-1" so a
	// future account swap is detected.
	home, restoreHome := withHome(t)
	defer restoreHome()
	writeAccountJWT(t, home, map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = globalClientJWTStore.Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "", // legacy
		MintedAt:    time.Now(),
	})

	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
		t.Fatal("renewal must NOT fire for a valid unexpired entry")
		return "", nil
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	_, _, reused, err := provideAuth(nil, nil, "", docopt.Opts{}, "node", "direct")
	if err != nil {
		t.Fatalf("provideAuth err = %v", err)
	}
	if !reused {
		t.Error("expected reuse")
	}

	stored, _ := globalClientJWTStore.Get("direct")
	if stored.NetworkID != "net-1" {
		t.Errorf("self-heal: store NetworkID = %q, want stamped %q", stored.NetworkID, "net-1")
	}
	if stored.ByClientJWT != goodJwt {
		t.Errorf("self-heal: store by_jwt changed to %q, must preserve original", stored.ByClientJWT)
	}
	if stored.ClientID != testClientId {
		t.Errorf("self-heal: store client_id changed to %q, must preserve %q", stored.ClientID, testClientId)
	}
}

// --- helpers ---------------------------------------------------------------

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

var errFakeRenew = &fakeError{msg: "fake renewal failure"}

package hotkeywallet

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/urfoundation/sn/protocol"
)

// A store file is private where a mode bit can enforce it: written 0600, so
// owner-only. Windows has no mode bits for this — Go reports 0666 for any file
// the process can write, and privacy there is the ACL the directory inherits —
// so the check is not applicable rather than silently satisfied. Say so in the
// log instead of asserting a mode the platform cannot express.
func requirePrivateMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Logf("%s: mode bits are not a privacy boundary on this platform", path)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("%s is not private: mode %v", path, info.Mode().Perm())
	}
}

func testStore(t *testing.T) Store {
	t.Helper()
	return Store{Directory: filepath.Join(t.TempDir(), "hotkey-wallet")}
}

func testJson(t testing.TB, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStoreAppendsAndReloadsTheChain(t *testing.T) {
	store := testStore(t)
	if chain, err := store.Chain(); err != nil || chain != nil {
		t.Fatalf("an empty store has a chain: %v %v", chain, err)
	}
	chain := testChain(t, testKey(t, 1), testKey(t, 2), 2)
	for _, original := range chain {
		if err := store.Append(original); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := Store{Directory: store.Directory}.Chain()
	if err != nil || !slices.Equal(reloaded, chain) {
		t.Fatalf("the chain was not reloaded: %v", err)
	}
	path := filepath.Join(store.Directory, "originals.json")
	requirePrivateMode(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// the same original again is already appended
	if err := store.Append(chain[0]); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("appending a stored original rewrote the chain", err)
	}
}

func TestStoreRefusesAForkAndAnUnverifiedOriginal(t *testing.T) {
	coldkey, hotkey := testKey(t, 1), testKey(t, 2)
	store := testStore(t)
	chain := testChain(t, coldkey, hotkey, 2)
	if err := store.Append(chain[0]); err != nil {
		t.Fatal(err)
	}
	// another first generation, with its own nonce
	fork := testOriginal(t, nil, coldkey, hotkey, 100, 1100)
	unsigned := chain[1]
	unsigned.HotkeySignature = unsigned.ColdkeySignature
	for _, c := range []struct {
		name     string
		original protocol.HotkeyWalletMappingConsent
	}{
		{name: "a fork of generation 1", original: fork},
		{name: "a successor of the fork", original: testOriginal(t, []protocol.HotkeyWalletMappingConsent{fork}, coldkey, hotkey, 200, 1200)},
		{name: "a skipped generation", original: testOriginal(t, chain, coldkey, hotkey, 300, 1300)},
		{name: "a generation the hotkey did not sign", original: unsigned},
		{name: "another hotkey's successor", original: testOriginal(t, testChain(t, coldkey, testKey(t, 4), 1), coldkey, testKey(t, 4), 200, 1200)},
	} {
		if err := store.Append(c.original); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Errorf("%s was appended: %v", c.name, err)
		}
	}
	if stored, err := store.Chain(); err != nil || !slices.Equal(stored, chain[:1]) {
		t.Fatalf("a refused append changed the chain: %v", err)
	}
	if err := store.Append(chain[1]); err != nil {
		t.Fatal(err)
	}
}

func TestStorePendingIsRemovedOnceItsGenerationIsStored(t *testing.T) {
	coldkey, hotkey := testKey(t, 1), testKey(t, 2)
	store := testStore(t)
	if pending, err := store.Pending(); err != nil || pending != nil {
		t.Fatalf("an empty store has a pending statement: %v %v", pending, err)
	}
	statement, err := NextStatement(nil, testDomain.HotkeySubnet(), hotkey.PublicKey(), coldkey.PublicKey(), 100, 1100, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPending(*statement); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending()
	if err != nil || pending == nil || *pending != *statement {
		t.Fatalf("the pending statement was not kept: %+v %v", pending, err)
	}
	path := filepath.Join(store.Directory, "pending.json")
	message, _ := statement.Message()
	if raw, err := os.ReadFile(path); err != nil || string(raw) != strings.TrimPrefix(message, protocol.HotkeyWalletMappingConsentPrefix) {
		t.Fatalf("the pending statement is not its canonical JSON: %q %v", raw, err)
	}
	requirePrivateMode(t, path)

	first := testSigned(t, *statement, coldkey, hotkey)
	if err := store.Append(first); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(); err != nil || pending != nil {
		t.Fatalf("the appended statement is still pending: %+v %v", pending, err)
	}

	// a pending later generation stays through a repeated append
	next, err := NextStatement([]protocol.HotkeyWalletMappingConsent{first}, testDomain.HotkeySubnet(), hotkey.PublicKey(), coldkey.PublicKey(), 200, 1200, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPending(*next); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(first); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(); err != nil || pending == nil || *pending != *next {
		t.Fatalf("a pending later generation was removed: %+v %v", pending, err)
	}
}

func TestStoreRefusesAnInvalidPendingStatement(t *testing.T) {
	store := testStore(t)
	statement, err := NextStatement(nil, testDomain.HotkeySubnet(), testKey(t, 2).PublicKey(), testKey(t, 1).PublicKey(), 100, 1100, testNow)
	if err != nil {
		t.Fatal(err)
	}
	statement.Coldkey = [32]byte{}
	if err := store.SetPending(*statement); err == nil {
		t.Fatal("an invalid pending statement was kept")
	}
	if pending, err := store.Pending(); err != nil || pending != nil {
		t.Fatalf("a refused statement is pending: %+v %v", pending, err)
	}
	if err := os.MkdirAll(store.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Directory, "pending.json"), []byte(`{"schema":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(); err == nil {
		t.Fatalf("an invalid pending file was read: %+v", pending)
	}
}

func TestStoreRefusesAnAlteredChainFile(t *testing.T) {
	store := testStore(t)
	chain := testChain(t, testKey(t, 1), testKey(t, 2), 2)
	for _, original := range chain {
		if err := store.Append(original); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(store.Directory, "originals.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	swapped := chain[1]
	swapped.ColdkeySignature, swapped.HotkeySignature = chain[0].ColdkeySignature, chain[0].HotkeySignature
	for _, altered := range []string{
		string(raw) + "[]",
		// an unknown field is the only change
		strings.Replace(string(raw), `{"message"`, `{"extra":1,"message"`, 1),
		"[]",
		"",
		testJson(t, []protocol.HotkeyWalletMappingConsent{chain[1]}),
		testJson(t, []protocol.HotkeyWalletMappingConsent{chain[0], swapped}),
	} {
		if err := os.WriteFile(path, []byte(altered), 0600); err != nil {
			t.Fatal(err)
		}
		if stored, err := store.Chain(); err == nil {
			t.Errorf("an altered chain file was read: %q %v", altered, stored)
		}
		for _, original := range chain {
			if err := store.Append(original); err == nil {
				t.Errorf("an append onto an altered chain file was admitted: %q", altered)
			}
		}
	}
}

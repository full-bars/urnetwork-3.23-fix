package main

// sn_fleet_test.go — coverage for the fleet bind/revoke machinery behind
// `provider bind-head` / `provider unbind-head` (sn_fleet.go): manifest
// plumbing, seed loading, and the dual-signed binding round trip.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
)

type fleetTestManifestJSON struct {
	Schema      string            `json:"schema"`
	ChainID     uint64            `json:"chain_id"`
	Netuid      uint16            `json:"netuid"`
	Coordinator string            `json:"coordinator"`
	FleetID     string            `json:"fleet_id"`
	Hotkey      string            `json:"hotkey"`
	Generation  uint64            `json:"generation"`
	Members     []fleetTestMember `json:"members"`
}

type fleetTestMember struct {
	ClientID  string `json:"client_id"`
	ClientKey string `json:"client_key"`
}

// newFleetTestManifest builds a manifest in the canonical field order the
// parser requires (schema, chain_id, netuid, coordinator, fleet_id, hotkey,
// generation, members), compact-marshaled like FleetManifest.Canonical.
func newFleetTestManifest(t *testing.T, clientID [16]byte, clientKey [32]byte, hotkey [32]byte) *protocol.FleetManifest {
	t.Helper()
	doc := fleetTestManifestJSON{
		Schema:      protocol.FleetManifestSchema,
		ChainID:     1116,
		Netuid:      521,
		Coordinator: "0x" + strings.Repeat("11", 20),
		FleetID:     "0x" + strings.Repeat("22", 32),
		Hotkey:      "0x" + hex.EncodeToString(hotkey[:]),
		Generation:  1,
		Members: []fleetTestMember{
			{
				ClientID:  "0x" + hex.EncodeToString(clientID[:]),
				ClientKey: "0x" + hex.EncodeToString(clientKey[:]),
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := protocol.ParseFleetManifest(raw)
	if err != nil {
		t.Fatalf("ParseFleetManifest: %s", err)
	}
	return manifest
}

func newFleetTestKeys(t *testing.T) ([16]byte, [32]byte, ed25519.PrivateKey, [32]byte, []byte) {
	t.Helper()
	clientSeed := make([]byte, ed25519.SeedSize)
	for i := range clientSeed {
		clientSeed[i] = byte(i + 1)
	}
	clientKey := ed25519.NewKeyFromSeed(clientSeed)
	var clientKeyArr [32]byte
	copy(clientKeyArr[:], clientKey.Public().(ed25519.PublicKey))
	var clientID [16]byte
	copy(clientID[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})

	hotkeySeed := make([]byte, 32)
	for i := range hotkeySeed {
		hotkeySeed[i] = byte(0x40 + i)
	}
	hotkeyPair, err := (sr25519.Scheme{}).FromSeed(hotkeySeed)
	if err != nil {
		t.Fatal(err)
	}
	var hotkeyArr [32]byte
	copy(hotkeyArr[:], hotkeyPair.Public())
	return clientID, clientKeyArr, clientKey, hotkeyArr, hotkeySeed
}

func TestSnFleetBindingAndSignRoundTrip(t *testing.T) {
	clientID, clientKeyArr, clientKey, hotkeyArr, hotkeySeed := newFleetTestKeys(t)
	manifest := newFleetTestManifest(t, clientID, clientKeyArr, hotkeyArr)
	member, err := snFleetMember(manifest, clientID)
	if err != nil {
		t.Fatal(err)
	}

	binding, clientSig, hotkeySig, err := snFleetBindingAndSign(manifest, member, 100, 200, clientKey, hotkeySeed)
	if err != nil {
		t.Fatalf("snFleetBindingAndSign: %s", err)
	}
	if binding.ClientID != clientID || binding.ClientKey != clientKeyArr || binding.Hotkey != hotkeyArr {
		t.Fatal("binding does not carry the member identity")
	}
	if binding.ValidFromEpoch != 100 || binding.ValidToEpoch != 200 {
		t.Fatal("binding does not carry the requested epochs")
	}
	if !binding.VerifyClient(clientSig) {
		t.Fatal("client signature does not verify against the binding digest")
	}
	if !binding.VerifyHotkey(hotkeySig) {
		t.Fatal("hotkey signature does not verify against the binding digest")
	}
	calldata, err := onchain.BuildFleetBindingCalldata(binding, clientSig, hotkeySig)
	if err != nil {
		t.Fatalf("BuildFleetBindingCalldata: %s", err)
	}
	if len(calldata) == 0 {
		t.Fatal("empty bindFleetMember calldata")
	}
}

func TestSnFleetBindingAndSignRejectsWrongHotkeySeed(t *testing.T) {
	clientID, clientKeyArr, clientKey, hotkeyArr, _ := newFleetTestKeys(t)
	manifest := newFleetTestManifest(t, clientID, clientKeyArr, hotkeyArr)
	member, err := snFleetMember(manifest, clientID)
	if err != nil {
		t.Fatal(err)
	}
	wrongSeed := make([]byte, 32)
	for i := range wrongSeed {
		wrongSeed[i] = byte(0x80 + i)
	}
	if _, _, _, err := snFleetBindingAndSign(manifest, member, 100, 200, clientKey, wrongSeed); err == nil {
		t.Fatal("a hotkey seed that does not match the manifest must be rejected")
	}
}

func TestSnFleetBindingAndSignRejectsWrongClientKey(t *testing.T) {
	clientID, clientKeyArr, _, hotkeyArr, hotkeySeed := newFleetTestKeys(t)
	manifest := newFleetTestManifest(t, clientID, clientKeyArr, hotkeyArr)
	member, err := snFleetMember(manifest, clientID)
	if err != nil {
		t.Fatal(err)
	}
	otherSeed := make([]byte, ed25519.SeedSize)
	for i := range otherSeed {
		otherSeed[i] = byte(0x90 + i)
	}
	otherKey := ed25519.NewKeyFromSeed(otherSeed)
	if _, _, _, err := snFleetBindingAndSign(manifest, member, 100, 200, otherKey, hotkeySeed); err == nil {
		t.Fatal("a client key that does not match the member must be rejected")
	}
}

func TestSnFleetMemberNotFound(t *testing.T) {
	clientID, clientKeyArr, _, hotkeyArr, _ := newFleetTestKeys(t)
	manifest := newFleetTestManifest(t, clientID, clientKeyArr, hotkeyArr)
	var other [16]byte
	other[0] = 0xff
	if _, err := snFleetMember(manifest, other); err == nil {
		t.Fatal("an unknown client_id must be rejected")
	}
}

func TestParseClientID16Arg(t *testing.T) {
	good := "0x" + strings.Repeat("ab", 16)
	parsed, err := parseClientID16Arg("--client_id", good)
	if err != nil {
		t.Fatalf("parseClientID16Arg: %s", err)
	}
	if hex.EncodeToString(parsed[:]) != strings.Repeat("ab", 16) {
		t.Fatal("parsed client id does not round trip")
	}
	for _, bad := range []string{"0x1234", "zz" + strings.Repeat("ab", 15), ""} {
		if _, err := parseClientID16Arg("--client_id", bad); err == nil {
			t.Fatalf("parseClientID16Arg(%q) must fail", bad)
		}
	}
}

func TestSnLoadSeeds(t *testing.T) {
	dir := t.TempDir()
	rawSeed := make([]byte, 32)
	for i := range rawSeed {
		rawSeed[i] = byte(i)
	}
	rawPath := filepath.Join(dir, "raw.seed")
	if err := os.WriteFile(rawPath, rawSeed, 0600); err != nil {
		t.Fatal(err)
	}
	hexPath := filepath.Join(dir, "hex.seed")
	if err := os.WriteFile(hexPath, []byte("0x"+hex.EncodeToString(rawSeed)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rawPath, hexPath} {
		seed, err := snLoadHotkeySeed(path)
		if err != nil {
			t.Fatalf("snLoadHotkeySeed(%s): %s", path, err)
		}
		if string(seed) != string(rawSeed) {
			t.Fatalf("snLoadHotkeySeed(%s) does not round trip", path)
		}
		key, err := snLoadClientSeedOverride(path)
		if err != nil {
			t.Fatalf("snLoadClientSeedOverride(%s): %s", path, err)
		}
		if string(key.Seed()) != string(rawSeed) {
			t.Fatalf("snLoadClientSeedOverride(%s) does not round trip", path)
		}
	}
	short := filepath.Join(dir, "short.seed")
	if err := os.WriteFile(short, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snLoadHotkeySeed(short); err == nil {
		t.Fatal("a short seed file must be rejected")
	}
}

func TestSnFleetRevokeDomainRoundTrip(t *testing.T) {
	clientID, _, clientKey, _, _ := newFleetTestKeys(t)
	revoke := protocol.FleetRevoke{
		ChainID:        1116,
		Netuid:         521,
		Coordinator:    [20]byte{0x11},
		ClientID:       clientID,
		Generation:     1,
		EffectiveEpoch: 300,
	}
	digest, err := revoke.Digest()
	if err != nil {
		t.Fatalf("FleetRevoke.Digest: %s", err)
	}
	signature := ed25519.Sign(clientKey, digest[:])
	if !revoke.VerifyClient(clientKey.Public().(ed25519.PublicKey), signature) {
		t.Fatal("client revoke signature does not verify")
	}
}

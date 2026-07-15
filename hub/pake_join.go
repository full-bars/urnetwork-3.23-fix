package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bytemare/ecc"
	"github.com/bytemare/opaque"
)

const (
	pakeServerIdentity    = "urnetwork-hub"
	pakeFleetJoinIdentity = "urnetwork-fleet-join"
)

// pakeConfiguration is the single OPAQUE configuration every join-related
// function in this file shares. Per the library's own docs, a configuration
// must remain identical between registration and every subsequent login for
// a given client — centralizing it here is what guarantees that.
func pakeConfiguration() *opaque.Configuration {
	return opaque.DefaultConfiguration()
}

// pakeServerKeys is the on-disk, JSON-serializable form of an
// opaque.ServerKeyMaterial. Unlike the CA keypair in hub/ca.go, this is not
// deterministically re-derived from the root password on every boot — OPAQUE's
// long-term server identity is ordinary long-term key material, generated
// once via the library's own randomness (the same as any server's static TLS
// or SSH host key) and persisted, not regenerated.
type pakeServerKeys struct {
	PrivateKeyHex string `json:"private_key_hex"`
	PublicKeyHex  string `json:"public_key_hex"`
	OPRFSeedHex   string `json:"oprf_seed_hex"`
}

func pakeServerKeysPath(dataDir string) string {
	return filepath.Join(dataDir, "hub.pake_server.json")
}

// loadOrCreatePakeServerKeys returns this hub's long-term OPAQUE server key
// material, generating and persisting it on first use.
func loadOrCreatePakeServerKeys(dataDir string) (*opaque.ServerKeyMaterial, error) {
	path := pakeServerKeysPath(dataDir)

	if data, err := os.ReadFile(path); err == nil {
		var stored pakeServerKeys
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		conf := pakeConfiguration()
		privKey := ecc.Group(conf.AKE).NewScalar()
		if err := privKey.DecodeHex(stored.PrivateKeyHex); err != nil {
			return nil, fmt.Errorf("decode private key in %s: %w", path, err)
		}
		pubKey, err := hex.DecodeString(stored.PublicKeyHex)
		if err != nil {
			return nil, fmt.Errorf("decode public key in %s: %w", path, err)
		}
		oprfSeed, err := hex.DecodeString(stored.OPRFSeedHex)
		if err != nil {
			return nil, fmt.Errorf("decode OPRF seed in %s: %w", path, err)
		}

		return &opaque.ServerKeyMaterial{
			Identity:       []byte(pakeServerIdentity),
			PrivateKey:     privKey,
			PublicKeyBytes: pubKey,
			OPRFGlobalSeed: oprfSeed,
		}, nil
	}

	conf := pakeConfiguration()
	oprfSeed := conf.GenerateOPRFSeed()
	privKey, pubKey := conf.KeyGen()

	stored := pakeServerKeys{
		PrivateKeyHex: privKey.Hex(),
		PublicKeyHex:  hex.EncodeToString(pubKey.Encode()),
		OPRFSeedHex:   hex.EncodeToString(oprfSeed),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("marshal server keys: %w", err)
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}

	return &opaque.ServerKeyMaterial{
		Identity:       []byte(pakeServerIdentity),
		PrivateKey:     privKey,
		PublicKeyBytes: pubKey.Encode(),
		OPRFGlobalSeed: oprfSeed,
	}, nil
}

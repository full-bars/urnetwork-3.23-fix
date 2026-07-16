package main

import (
	"crypto/sha256"
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

// pakeJoinRecord is the on-disk, JSON-serializable form of the single
// opaque.ClientRecord this hub registers for its "fleet-join" identity.
type pakeJoinRecord struct {
	CredentialIdentifierHex string `json:"credential_identifier_hex"`
	ClientIdentity          string `json:"client_identity"`
	RegistrationRecordHex   string `json:"registration_record_hex"`
	// PasswordHashHex is a SHA-256 hex digest of the CA password this record
	// was registered against — never the password itself. It lets
	// loadOrRegisterPakeJoin detect that hub.password has been rotated and
	// re-register against the new password, instead of silently continuing
	// to accept the old one forever.
	PasswordHashHex string `json:"password_hash_hex"`
}

func pakeJoinRecordPath(dataDir string) string {
	return filepath.Join(dataDir, "hub.pake_record.json")
}

// pakePasswordHash returns the SHA-256 hex digest of the CA password, used
// only to detect password rotation — never to reconstruct the password.
func pakePasswordHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

func loadOrRegisterPakeJoin(dataDir string, skm *opaque.ServerKeyMaterial, password string) (*opaque.ClientRecord, error) {
	path := pakeJoinRecordPath(dataDir)
	conf := pakeConfiguration()
	passwordHash := pakePasswordHash(password)

	if data, err := os.ReadFile(path); err == nil {
		var stored pakeJoinRecord
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		if stored.PasswordHashHex == passwordHash {
			server, err := conf.Server()
			if err != nil {
				return nil, err
			}
			credID, err := hex.DecodeString(stored.CredentialIdentifierHex)
			if err != nil {
				return nil, fmt.Errorf("decode credential identifier in %s: %w", path, err)
			}
			recordBytes, err := hex.DecodeString(stored.RegistrationRecordHex)
			if err != nil {
				return nil, fmt.Errorf("decode registration record in %s: %w", path, err)
			}
			record, err := server.Deserialize.RegistrationRecord(recordBytes)
			if err != nil {
				return nil, fmt.Errorf("deserialize registration record in %s: %w", path, err)
			}

			return &opaque.ClientRecord{
				CredentialIdentifier: credID,
				ClientIdentity:       []byte(stored.ClientIdentity),
				RegistrationRecord:   record,
			}, nil
		}
		// CA password has changed since this record was registered — fall
		// through and re-register against the new password so PAKE join
		// stays in lockstep with hub.password rotation.
	}

	server, err := conf.Server()
	if err != nil {
		return nil, err
	}
	if err := server.SetKeyMaterial(skm); err != nil {
		return nil, err
	}
	client, err := conf.Client()
	if err != nil {
		return nil, err
	}

	credID := opaque.RandomBytes(32)

	regReq, err := client.RegistrationInit([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("RegistrationInit: %w", err)
	}
	regResp, err := server.RegistrationResponse(regReq, credID, nil)
	if err != nil {
		return nil, fmt.Errorf("RegistrationResponse: %w", err)
	}
	record, _, err := client.RegistrationFinalize(regResp, []byte(pakeFleetJoinIdentity), []byte(pakeServerIdentity))
	if err != nil {
		return nil, fmt.Errorf("RegistrationFinalize: %w", err)
	}

	stored := pakeJoinRecord{
		CredentialIdentifierHex: hex.EncodeToString(credID),
		ClientIdentity:          pakeFleetJoinIdentity,
		RegistrationRecordHex:   hex.EncodeToString(record.Serialize()),
		PasswordHashHex:         passwordHash,
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("marshal join record: %w", err)
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}

	return &opaque.ClientRecord{
		CredentialIdentifier: credID,
		ClientIdentity:       []byte(pakeFleetJoinIdentity),
		RegistrationRecord:   record,
	}, nil
}

func pakeServerLoginStep1(skm *opaque.ServerKeyMaterial, record *opaque.ClientRecord, ke1Bytes []byte) ([]byte, *opaque.ServerOutput, error) {
	conf := pakeConfiguration()
	server, err := conf.Server()
	if err != nil {
		return nil, nil, err
	}
	if err := server.SetKeyMaterial(skm); err != nil {
		return nil, nil, err
	}

	ke1, err := server.Deserialize.KE1(ke1Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("deserialize KE1: %w", err)
	}

	ke2, serverOutput, err := server.GenerateKE2(ke1, record)
	if err != nil {
		return nil, nil, fmt.Errorf("GenerateKE2: %w", err)
	}

	return ke2.Serialize(), serverOutput, nil
}

func pakeServerLoginFinish(serverOutput *opaque.ServerOutput, ke3Bytes []byte) ([]byte, error) {
	conf := pakeConfiguration()
	server, err := conf.Server()
	if err != nil {
		return nil, err
	}

	ke3, err := server.Deserialize.KE3(ke3Bytes)
	if err != nil {
		return nil, fmt.Errorf("deserialize KE3: %w", err)
	}

	if err := server.LoginFinish(ke3, serverOutput.ClientMAC); err != nil {
		return nil, fmt.Errorf("login rejected: %w", err)
	}

	return serverOutput.SessionSecret, nil
}

func pakeClientLoginStep1(password string) ([]byte, *opaque.Client, error) {
	conf := pakeConfiguration()
	client, err := conf.Client()
	if err != nil {
		return nil, nil, err
	}

	ke1, err := client.GenerateKE1([]byte(password))
	if err != nil {
		return nil, nil, fmt.Errorf("GenerateKE1: %w", err)
	}

	return ke1.Serialize(), client, nil
}

func pakeClientLoginFinish(client *opaque.Client, ke2Bytes []byte) ([]byte, []byte, error) {
	ke2, err := client.Deserialize.KE2(ke2Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("deserialize KE2: %w", err)
	}

	ke3, sessionKey, _, err := client.GenerateKE3(ke2, []byte(pakeFleetJoinIdentity), []byte(pakeServerIdentity))
	if err != nil {
		return nil, nil, fmt.Errorf("GenerateKE3: %w", err)
	}

	return ke3.Serialize(), sessionKey, nil
}

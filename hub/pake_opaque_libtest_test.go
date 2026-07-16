package main

import (
	"bytes"
	"testing"

	"github.com/bytemare/opaque"
	"github.com/bytemare/opaque/message"
)

// TestOpaqueLibraryRegistrationAndLoginRoundTrip pins the exact third-party
// API surface this plan depends on. If bytemare/opaque changes any of these
// signatures or behaviors in a future upgrade, this test — not our own
// derived code — is what should fail first.
func TestOpaqueLibraryRegistrationAndLoginRoundTrip(t *testing.T) {
	conf := opaque.DefaultConfiguration()

	serverID := []byte("urnetwork-hub")
	clientID := []byte("urnetwork-fleet-join")
	password := []byte("correct horse battery staple")

	oprfSeed := conf.GenerateOPRFSeed()
	serverPrivateKey, serverPublicKey := conf.KeyGen()

	server, err := conf.Server()
	if err != nil {
		t.Fatalf("conf.Server(): %v", err)
	}
	skm := &opaque.ServerKeyMaterial{
		Identity:       serverID,
		PrivateKey:     serverPrivateKey,
		PublicKeyBytes: serverPublicKey.Encode(),
		OPRFGlobalSeed: oprfSeed,
	}
	if err := server.SetKeyMaterial(skm); err != nil {
		t.Fatalf("SetKeyMaterial: %v", err)
	}

	client, err := conf.Client()
	if err != nil {
		t.Fatalf("conf.Client(): %v", err)
	}

	// --- Registration ---
	credID := opaque.RandomBytes(32)

	regReq, err := client.RegistrationInit(password)
	if err != nil {
		t.Fatalf("RegistrationInit: %v", err)
	}
	regResp, err := server.RegistrationResponse(regReq, credID, nil)
	if err != nil {
		t.Fatalf("RegistrationResponse: %v", err)
	}
	record, _, err := client.RegistrationFinalize(regResp, clientID, serverID)
	if err != nil {
		t.Fatalf("RegistrationFinalize: %v", err)
	}
	clientRecord := &opaque.ClientRecord{
		CredentialIdentifier: credID,
		ClientIdentity:       clientID,
		RegistrationRecord:   record,
	}

	// --- Login with the correct password ---
	// A fresh Client instance is required here: the one used for
	// registration above already ran a protocol round (RegistrationInit),
	// and the library rejects reusing client state across separate runs.
	client, err = conf.Client()
	if err != nil {
		t.Fatalf("conf.Client() for login: %v", err)
	}
	ke1, err := client.GenerateKE1(password)
	if err != nil {
		t.Fatalf("GenerateKE1: %v", err)
	}
	ke2, serverOutput, err := server.GenerateKE2(ke1, clientRecord)
	if err != nil {
		t.Fatalf("GenerateKE2: %v", err)
	}
	ke3, clientSessionKey, _, err := client.GenerateKE3(ke2, clientID, serverID)
	if err != nil {
		t.Fatalf("GenerateKE3: %v", err)
	}
	if err := server.LoginFinish(ke3, serverOutput.ClientMAC); err != nil {
		t.Fatalf("LoginFinish (correct password): %v", err)
	}
	if !bytes.Equal(clientSessionKey, serverOutput.SessionSecret) {
		t.Fatal("client and server session keys do not match")
	}

	// --- Login with the wrong password must fail ---
	// Again, a fresh Client instance — same reason as above.
	client, err = conf.Client()
	if err != nil {
		t.Fatalf("conf.Client() for wrong-password login: %v", err)
	}
	wrongKE1, err := client.GenerateKE1([]byte("wrong password"))
	if err != nil {
		t.Fatalf("GenerateKE1 (wrong password): %v", err)
	}
	wrongKE2, wrongServerOutput, err := server.GenerateKE2(wrongKE1, clientRecord)
	if err != nil {
		t.Fatalf("GenerateKE2 (wrong password) should not itself error: %v", err)
	}
	wrongKE3, _, _, err := client.GenerateKE3(wrongKE2, clientID, serverID)
	if err == nil {
		if err := server.LoginFinish(wrongKE3, wrongServerOutput.ClientMAC); err == nil {
			t.Fatal("expected LoginFinish to reject a login with the wrong password")
		}
	}

	// Message types must serialize/deserialize through the wire format we'll
	// actually use once this is wired to a real network hop in a later plan.
	var ke2Round *message.KE2
	ke2Round, err = server.Deserialize.KE2(ke2.Serialize())
	if err != nil {
		t.Fatalf("KE2 round-trip: %v", err)
	}
	if !bytes.Equal(ke2Round.Serialize(), ke2.Serialize()) {
		t.Fatal("KE2 did not round-trip through Serialize/Deserialize unchanged")
	}
}

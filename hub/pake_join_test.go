package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreatePakeServerKeys_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatalf("loadOrCreatePakeServerKeys: %v", err)
	}
	if len(skm.PublicKeyBytes) == 0 || len(skm.OPRFGlobalSeed) == 0 || skm.PrivateKey == nil {
		t.Fatal("expected generated key material to be populated")
	}

	path := filepath.Join(dir, "hub.pake_server.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected hub.pake_server.json to be written: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreatePakeServerKeys_ReloadsIdenticalMaterial(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	second, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if !bytes.Equal(first.PublicKeyBytes, second.PublicKeyBytes) {
		t.Error("public key changed across reload — server identity is not stable across restarts")
	}
	if !bytes.Equal(first.OPRFGlobalSeed, second.OPRFGlobalSeed) {
		t.Error("OPRF seed changed across reload")
	}
	if !first.PrivateKey.Equal(second.PrivateKey) {
		t.Error("private key changed across reload")
	}
}

func TestLoadOrCreatePakeServerKeys_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.pake_server.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadOrCreatePakeServerKeys(dir); err == nil {
		t.Fatal("expected an error loading a corrupt hub.pake_server.json, got nil")
	}
}

func TestLoadOrRegisterPakeJoin_RegistersAndPersists(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}

	record, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple")
	if err != nil {
		t.Fatalf("loadOrRegisterPakeJoin: %v", err)
	}
	if record.RegistrationRecord == nil {
		t.Fatal("expected a non-nil RegistrationRecord")
	}

	path := filepath.Join(dir, "hub.pake_record.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected hub.pake_record.json to be written: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrRegisterPakeJoin_ReloadSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple"); err != nil {
		t.Fatalf("initial registration: %v", err)
	}

	skm2, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record2, err := loadOrRegisterPakeJoin(dir, skm2, "correct horse battery staple")
	if err != nil {
		t.Fatalf("reload after restart: %v", err)
	}

	conf := pakeConfiguration()
	server, err := conf.Server()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetKeyMaterial(skm2); err != nil {
		t.Fatal(err)
	}
	client, err := conf.Client()
	if err != nil {
		t.Fatal(err)
	}

	ke1, err := client.GenerateKE1([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	ke2, serverOutput, err := server.GenerateKE2(ke1, record2)
	if err != nil {
		t.Fatalf("GenerateKE2 after simulated restart: %v", err)
	}
	ke3, _, _, err := client.GenerateKE3(ke2, []byte(pakeFleetJoinIdentity), []byte(pakeServerIdentity))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.LoginFinish(ke3, serverOutput.ClientMAC); err != nil {
		t.Fatalf("login failed after simulated hub restart: %v", err)
	}
}

func TestPakeLoginHandshake_CorrectPasswordSucceeds(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	ke1Bytes, client, err := pakeClientLoginStep1("correct horse battery staple")
	if err != nil {
		t.Fatalf("pakeClientLoginStep1: %v", err)
	}

	ke2Bytes, serverOutput, err := pakeServerLoginStep1(skm, record, ke1Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginStep1: %v", err)
	}

	ke3Bytes, clientSessionKey, err := pakeClientLoginFinish(client, ke2Bytes)
	if err != nil {
		t.Fatalf("pakeClientLoginFinish: %v", err)
	}

	serverSessionKey, err := pakeServerLoginFinish(serverOutput, ke3Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginFinish: %v", err)
	}

	if len(serverSessionKey) == 0 || !bytes.Equal(clientSessionKey, serverSessionKey) {
		t.Fatal("client and server session keys do not match")
	}
}

func TestPakeLoginHandshake_WrongPasswordFails(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	ke1Bytes, client, err := pakeClientLoginStep1("totally wrong password")
	if err != nil {
		t.Fatalf("pakeClientLoginStep1: %v", err)
	}
	ke2Bytes, serverOutput, err := pakeServerLoginStep1(skm, record, ke1Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginStep1: %v", err)
	}
	ke3Bytes, _, ke3Err := pakeClientLoginFinish(client, ke2Bytes)
	if ke3Err != nil {
		return
	}
	if _, err := pakeServerLoginFinish(serverOutput, ke3Bytes); err == nil {
		t.Fatal("expected pakeServerLoginFinish to reject a wrong-password login")
	}
}

func TestPakeLoginHandshake_ConcurrentJoinsDoNotInterfere(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	sessionKeys := make([][]byte, n)
	errs := make([]error, n)
	done := make(chan int, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- i }()

			ke1Bytes, client, err := pakeClientLoginStep1("correct horse battery staple")
			if err != nil {
				errs[i] = err
				return
			}
			ke2Bytes, serverOutput, err := pakeServerLoginStep1(skm, record, ke1Bytes)
			if err != nil {
				errs[i] = err
				return
			}
			ke3Bytes, clientKey, err := pakeClientLoginFinish(client, ke2Bytes)
			if err != nil {
				errs[i] = err
				return
			}
			serverKey, err := pakeServerLoginFinish(serverOutput, ke3Bytes)
			if err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(clientKey, serverKey) {
				errs[i] = fmt.Errorf("session key mismatch for goroutine %d", i)
				return
			}
			sessionKeys[i] = serverKey
		}(i)
	}

	for i := 0; i < n; i++ {
		<-done
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if sessionKeys[i] != nil && sessionKeys[j] != nil && bytes.Equal(sessionKeys[i], sessionKeys[j]) {
				t.Errorf("goroutines %d and %d derived the same session key — sessions are not independent", i, j)
			}
		}
	}
}

func TestLoadOrRegisterPakeJoin_PasswordRotationReRegisters(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loadOrRegisterPakeJoin(dir, skm, "old password"); err != nil {
		t.Fatalf("initial registration: %v", err)
	}

	// Simulate the operator rotating hub.password: re-derive against a new
	// password. Without password-change detection, the stale record would be
	// silently reloaded and the old password would keep working forever.
	record, err := loadOrRegisterPakeJoin(dir, skm, "new password")
	if err != nil {
		t.Fatalf("re-registration after password rotation: %v", err)
	}

	// The new password must now succeed.
	ke1Bytes, client, err := pakeClientLoginStep1("new password")
	if err != nil {
		t.Fatal(err)
	}
	ke2Bytes, serverOutput, err := pakeServerLoginStep1(skm, record, ke1Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginStep1 with new password: %v", err)
	}
	ke3Bytes, _, err := pakeClientLoginFinish(client, ke2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pakeServerLoginFinish(serverOutput, ke3Bytes); err != nil {
		t.Fatalf("login with new password should succeed after rotation: %v", err)
	}

	// The old password must no longer succeed.
	ke1Bytes, client, err = pakeClientLoginStep1("old password")
	if err != nil {
		t.Fatal(err)
	}
	ke2Bytes, serverOutput, err = pakeServerLoginStep1(skm, record, ke1Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginStep1 should not itself error: %v", err)
	}
	ke3Bytes, _, ke3Err := pakeClientLoginFinish(client, ke2Bytes)
	if ke3Err == nil {
		if _, err := pakeServerLoginFinish(serverOutput, ke3Bytes); err == nil {
			t.Fatal("login with old (rotated-away) password should be rejected after re-registration")
		}
	}
}

func TestPakeLoginHandshake_TamperedKE3Rejected(t *testing.T) {
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadOrRegisterPakeJoin(dir, skm, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	ke1Bytes, client, err := pakeClientLoginStep1("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ke2Bytes, serverOutput, err := pakeServerLoginStep1(skm, record, ke1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ke3Bytes, _, err := pakeClientLoginFinish(client, ke2Bytes)
	if err != nil {
		t.Fatal(err)
	}

	tampered := make([]byte, len(ke3Bytes))
	copy(tampered, ke3Bytes)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := pakeServerLoginFinish(serverOutput, tampered); err == nil {
		t.Fatal("expected a tampered KE3 to be rejected by pakeServerLoginFinish")
	}
}

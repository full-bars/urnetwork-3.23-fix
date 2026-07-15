package main

import (
	"bytes"
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

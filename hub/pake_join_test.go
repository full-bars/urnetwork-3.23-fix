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

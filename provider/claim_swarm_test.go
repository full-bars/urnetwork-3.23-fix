package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeClaimSwarmMemberConfig(t *testing.T, dir, id, keyFile, rpc string) string {
	t.Helper()
	jwtFile := filepath.Join(dir, id+".jwt")
	if err := os.WriteFile(jwtFile, []byte(financialTestJwt(t, financialTestClientId, id)), 0o600); err != nil {
		t.Fatal(err)
	}
	config := ClaimDaemonConfig{
		SchemaVersion: 1, Release: "1.0", APIURL: "http://127.0.0.1:18081", RPC: []string{rpc},
		KeyFile: keyFile, JWTFile: jwtFile, StateDir: filepath.Join(dir, id+"-state"), PollSeconds: 10, LookbackEpochs: 3,
	}
	b, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".yml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaimSwarmConfigIsStrictAndSharesOneNonceDomain(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "relay.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	first := writeClaimSwarmMemberConfig(t, dir, "miner-1", keyFile, "http://rpc")
	second := writeClaimSwarmMemberConfig(t, dir, "miner-2", keyFile, "http://rpc")
	config := ClaimSwarmConfig{Schema: ClaimSwarmSchema, ListenAddress: "127.0.0.1:22081", Members: []ClaimSwarmMember{{ID: "miner-1", ConfigPath: first}, {ID: "miner-2", ConfigPath: second}}}
	if _, _, err := loadClaimSwarmMembers(&config); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "swarm.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClaimSwarmConfig(path); err != nil {
		t.Fatal(err)
	}
	unknown := append(append([]byte(nil), b[:len(b)-1]...), []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(path, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClaimSwarmConfig(path); err == nil {
		t.Fatal("unknown claim swarm field was accepted")
	}
	differentKey := filepath.Join(dir, "other.key")
	if err := os.WriteFile(differentKey, []byte(strings.Repeat("02", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Members[1].ConfigPath = writeClaimSwarmMemberConfig(t, dir, "miner-3", differentKey, "http://rpc")
	if _, _, err := loadClaimSwarmMembers(&config); err == nil {
		t.Fatal("two relayer nonce domains were accepted in one claim swarm")
	}
}

func TestClaimSwarmStatusFailsClosedOnMissingMember(t *testing.T) {
	config := &ClaimSwarmConfig{Schema: ClaimSwarmSchema, ListenAddress: "127.0.0.1:22081", Members: []ClaimSwarmMember{{ID: "miner-1", ConfigPath: "/tmp/miner-1.yml"}}}
	swarm, err := NewClaimSwarm(config)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	response := httptest.NewRecorder()
	swarm.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty claim swarm status = %d, want 503", response.Code)
	}
	swarm.running["miner-1"] = true
	response = httptest.NewRecorder()
	swarm.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("complete claim swarm status = %d, want 200", response.Code)
	}
}

// Config files may be distinct while their queues alias the same directory,
// including a not-yet-created child under a symlinked parent.
func TestClaimSwarmRejectsDuplicatePhysicalQueueOwners(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "synthetic-relay.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	first := writeClaimSwarmMemberConfig(t, dir, "first", keyFile, "https://rpc.example")
	second := writeClaimSwarmMemberConfig(t, dir, "second", keyFile, "https://rpc.example")
	firstCfg, err := LoadClaimDaemonConfig(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCfg, err := LoadClaimDaemonConfig(second)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	secondCfg.StateDir = filepath.Join(alias, filepath.Base(firstCfg.StateDir))
	encoded, err := yaml.Marshal(secondCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	config := &ClaimSwarmConfig{Members: []ClaimSwarmMember{{ID: "first", ConfigPath: first}, {ID: "second", ConfigPath: second}}}
	if _, _, err := loadClaimSwarmMembers(config); err == nil || !strings.Contains(err.Error(), "share one queue state directory") {
		t.Fatalf("aliased queue admitted: %v", err)
	}
}

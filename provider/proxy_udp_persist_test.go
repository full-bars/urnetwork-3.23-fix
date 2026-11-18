package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestUDPPersistRestartAndExpiry(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	ps := &connect.ProxySettings{Network: "tcp", Address: "example.test:1234", Auth: &proxy.Auth{User: "private-user", Password: "private-password"}}
	count := 0
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		count++
		return proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1", Err: errors.New("private-error")}
	})
	store.enablePersistence(path, now)
	store.Check(context.Background(), ps, now)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-user", "private-password", "private-error", "example.test"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("state contains private identity or error")
		}
	}
	loaded := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		t.Fatal("unexpected probe")
		return proxyUDPResult{}
	})
	loaded.enablePersistence(path, now.Add(time.Minute))
	result := loaded.Check(context.Background(), ps, now.Add(time.Minute))
	if result.Status != proxyUDPOK || result.UDPExit != "192.0.2.1" || count != 1 {
		t.Fatalf("restored result: %+v, calls %d", result, count)
	}
	if got := loaded.coverage([]string{ps.Key()}, now.Add(proxyUDPOKTTL)); got.Unchecked != 1 {
		t.Fatalf("expired coverage: %+v", got)
	}
	expired := newProxyUDPCheckStore(nil)
	expired.enablePersistence(path, now.Add(proxyUDPOKTTL))
	if got := expired.coverage([]string{ps.Key()}, now.Add(proxyUDPOKTTL)); got.Unchecked != 1 {
		t.Fatalf("expired restart: %+v", got)
	}
}

func TestUDPPersistInvalidInput(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ps := &connect.ProxySettings{Address: "example.test:1234"}
	path := filepath.Join(t.TempDir(), "state.json")
	future := udpPersistFile{Version: 1, Entries: []udpPersistEntry{{Key: udpPersistKey(ps.Key()), Status: proxyUDPOK, At: now.Add(time.Second)}}}
	futureJSON, _ := json.Marshal(future)
	for _, data := range [][]byte{[]byte("{"), []byte(`{"version":2,"entries":[]}`), futureJSON, []byte(strings.Repeat("x", udpStateMaxBytes+1))} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		store := newProxyUDPCheckStore(nil)
		store.enablePersistence(path, now)
		if got := store.coverage([]string{ps.Key()}, now); got.Unchecked != 1 {
			t.Fatalf("invalid input accepted: %+v", got)
		}
	}
}

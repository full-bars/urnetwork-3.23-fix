package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretCanaryUser = "canary-user-7731"
const secretCanaryPassword = "canary-pass-9042"
const secretCanaryToken = "canary-token-5518"

func TestProxySecretCorruptURLStateError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy_url.json")
	value := secretCanaryUser + ":" + secretCanaryPassword + "?token=" + secretCanaryToken
	data, err := json.Marshal(map[string]any{"cache": map[string]any{"example.com:1080": map[string]string{"last_probe": value}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readProxyURLStateFrom(path)
	if err == nil || !isProxyURLStateCorrupt(err) {
		t.Fatal("expected corrupt state error")
	}
	for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
		if strings.Contains(err.Error(), secret) {
			t.Error("corrupt state error contains a canary")
		}
	}
}

func TestProxySecretNestedURLError(t *testing.T) {
	raw := "http://" + secretCanaryUser + ":" + secretCanaryPassword + "@example.com/?token=" + secretCanaryToken
	err := &url.Error{Op: "get", URL: raw, Err: &url.Error{Op: "parse", URL: raw, Err: errors.New("invalid")}}
	got := stripURLFromError(err).Error()
	for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
		if strings.Contains(got, secret) {
			t.Error("nested URL error contains a canary")
		}
	}
}

func TestProxySecretRejectedPaste(t *testing.T) {
	for _, line := range []string{
		"socks5://" + secretCanaryUser + ":" + secretCanaryPassword + "@example.com:1080?token=" + secretCanaryToken,
		"example.com:1080?token=" + secretCanaryToken,
		secretCanaryUser + " " + secretCanaryPassword + " " + secretCanaryToken,
	} {
		got := redactProxyLine(line)
		for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
			if strings.Contains(got, secret) {
				t.Error("rejected paste output contains a canary")
			}
		}
	}
}

func TestProxySecretRejectedURLLog(t *testing.T) {
	out := captureTlog(t, func() {
		parseProxyURLLine("http://" + secretCanaryUser + ":" + secretCanaryPassword + "@example.com:1080?token=" + secretCanaryToken)
	})
	for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
		if strings.Contains(out, secret) {
			t.Error("rejected proxy log contains a canary")
		}
	}
}

func TestProxySecretCredentialFragments(t *testing.T) {
	for _, got := range []string{obfuscateUser(secretCanaryUser), obfuscatePassword(secretCanaryPassword)} {
		if strings.Contains(got, "ca") || strings.Contains(got, "31") || strings.Contains(got, "42") {
			t.Error("credential fragment appears in output")
		}
	}
}

func TestProxySecretAuditStatus(t *testing.T) {
	resetProxyAuditStatus(t)
	key := identityKey("example.com:1080?token="+secretCanaryToken, secretCanaryUser)
	h := newGovHarness(key)
	h.act = true
	h.twoBadTicks(key)
	b, err := json.Marshal(handleControlRequest(newControlState(), controlRequest{Cmd: "status"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretCanaryToken, "ca***31"} {
		if strings.Contains(string(b), secret) {
			t.Error("audit status contains a secret or credential fragment")
		}
	}
}

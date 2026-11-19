package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

const secretCanaryUser = "canary-user-7731"
const secretCanaryPassword = "canary-pass-9042"
const secretCanaryToken = "canary-token-5518"

func TestProxySecretStateFilesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	cases := []struct {
		name  string
		write func(string) error
	}{
		{"proxy.state", func(path string) error {
			return writeProxyStateTo(path, &ProxyState{Proxies: map[string]ProxyEntry{
				identityKey("example.com:1080", secretCanaryUser): {ID: 1},
			}})
		}},
		{"proxy_url.json", func(path string) error {
			return writeProxyURLStateTo(path, &ProxyURLState{Cache: map[string]ProxyURLEntry{
				"example.com:1080": {User: secretCanaryUser, Password: secretCanaryPassword},
			}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(path); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0600 {
				t.Errorf("file mode = %04o, want 0600", got)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), secretCanaryUser) {
				t.Error("expected identity in private state")
			}
			if tc.name == "proxy_url.json" && !strings.Contains(string(data), secretCanaryPassword) {
				t.Error("expected credentials in private URL cache")
			}
		})
	}
}

func TestProxySecretLegacyAdoptionLog(t *testing.T) {
	address := "http://" + secretCanaryUser + ":" + secretCanaryPassword + "@example.com:1080?token=" + secretCanaryToken
	state := &ProxyState{Proxies: map[string]ProxyEntry{address: {ID: 1}}}
	desired := []*connect.ProxySettings{authSettings(address, "first", "p"), authSettings(address, "second", "p")}
	out := captureTlog(t, func() { adoptLegacyProxyState(state, desired) })
	for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
		if strings.Contains(out, secret) {
			t.Error("legacy adoption log contains a canary")
		}
	}
}

func TestProxySecretAuditReleaseMultipleAccounts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	address := "http://" + secretCanaryUser + ":" + secretCanaryPassword + "@example.com:1080?token=" + secretCanaryToken
	one, two := identityKey(address, "one"), identityKey(address, "two")
	h := newAuditHarness(one, two)
	saved := globalProxyFailureHistory
	globalProxyFailureHistory = h.hist
	currentProxyAuditor.Store(h.g)
	t.Cleanup(func() { currentProxyAuditor.Store(nil); globalProxyFailureHistory = saved })
	cfg := defaultProxyAuditConfig()
	for _, key := range []string{one, two} {
		h.g.st.commitPark(cfg, key, h.now, 0.5)
	}
	var response controlResponse
	out := captureTlog(t, func() {
		response = handleControlRequest(newControlState(), controlRequest{Cmd: "audit", Action: "release", Address: address})
	})
	data, err := json.Marshal(response)
	if err != nil || !response.OK {
		t.Fatal("release failed")
	}
	for _, secret := range []string{secretCanaryUser, secretCanaryPassword, secretCanaryToken} {
		if strings.Contains(out, secret) || strings.Contains(string(data), secret) {
			t.Error("audit release output contains a canary")
		}
	}
}

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

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const secretCanaryUser = "canary-user-7731"
const secretCanaryPassword = "canary-pass-9042"
const secretCanaryToken = "canary-token-5518"

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

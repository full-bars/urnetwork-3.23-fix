package connect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

func createFakeConnectJWT(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
}

func TestParseByJwtUnverified_NetworkId(t *testing.T) {
	networkName := "testnet"
	networkId := "019f0000-0000-0000-0000-000000000001"

	jwt := createFakeConnectJWT(map[string]interface{}{
		"user_id":      "019f0000-0000-0000-0000-000000000002",
		"network_name": networkName,
		"network_id":   networkId,
	})

	byJwt, err := ParseByJwtUnverified(jwt)
	if err != nil {
		t.Fatalf("ParseByJwtUnverified() error = %v", err)
	}

	if byJwt.NetworkName != networkName {
		t.Errorf("byJwt.NetworkName = %q, want %q", byJwt.NetworkName, networkName)
	}

	expectedId, err := ParseId(networkId)
	if err != nil {
		t.Fatalf("ParseId(%q) error = %v", networkId, err)
	}
	if byJwt.NetworkId != expectedId {
		t.Errorf("byJwt.NetworkId = %v, want %v", byJwt.NetworkId, expectedId)
	}
}

func TestParseByJwtUnverified_NetworkIdMissing(t *testing.T) {
	jwt := createFakeConnectJWT(map[string]interface{}{
		"user_id":      "019f0000-0000-0000-0000-000000000002",
		"network_name": "testnet",
	})

	byJwt, err := ParseByJwtUnverified(jwt)
	if err != nil {
		t.Fatalf("ParseByJwtUnverified() error = %v", err)
	}

	if byJwt.NetworkId != (Id{}) {
		t.Error("byJwt.NetworkId should be zero-valued when network_id is missing from claims")
	}

	if byJwt.NetworkName != "testnet" {
		t.Errorf("byJwt.NetworkName = %q, want %q", byJwt.NetworkName, "testnet")
	}
}

func TestParseByJwtUnverified_InvalidJWT(t *testing.T) {
	_, err := ParseByJwtUnverified("not.a.jwt")
	if err == nil {
		t.Fatal("ParseByJwtUnverified() expected error for malformed JWT")
	}
}

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProviderAuthClientArgsForRenewal(t *testing.T) {
	cid := connect.NewId()
	args := newProviderAuthClientArgsForRenewal("mybox [beta-test]", cid)
	if args.ClientId == nil || *args.ClientId != cid {
		t.Fatalf("renewal args must carry the existing ClientId (got %v)", args.ClientId)
	}
	if args.SourceClientId != nil {
		t.Fatalf("renewal args must not link a source client")
	}
	if args.Description != "mybox [beta-test]" {
		t.Fatalf("description mismatch: %q", args.Description)
	}
}

func TestRenewClientJWTPreservesClientId(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/auth-client" {
			t.Errorf("path = %q, want /network/auth-client", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer account-jwt" {
			t.Errorf("Authorization = %q, want Bearer account-jwt", got)
		}
		var args connect.AuthNetworkClientArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode args: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if args.ClientId == nil {
			t.Errorf("renewal must send ClientId")
		}
		claims := map[string]interface{}{
			"client_id": args.ClientId.String(),
			"exp":       float64(time.Now().Add(24 * time.Hour).Unix()),
		}
		_ = json.NewEncoder(w).Encode(&connect.AuthNetworkClientResult{
			ByClientJwt: createFakeJWTWithClaims(claims),
		})
	}))
	defer server.Close()

	ctx := context.Background()
	byJwt, err := renewClientJWT(ctx, server.URL, "account-jwt", connect.NewId(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if byJwt == "" {
		t.Fatal("empty renewed JWT")
	}
	if !jwtContainsClientId(byJwt) {
		t.Fatalf("renewed JWT should contain client_id claim")
	}
}

func TestRenewClientJWTPreservesSameClientIdValue(t *testing.T) {
	clientId := connect.NewId()
	var gotClientId string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args connect.AuthNetworkClientArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
		if args.ClientId != nil {
			gotClientId = args.ClientId.String()
		}
		claims := map[string]interface{}{
			"client_id": args.ClientId.String(),
			"exp":       float64(time.Now().Add(24 * time.Hour).Unix()),
		}
		_ = json.NewEncoder(w).Encode(&connect.AuthNetworkClientResult{
			ByClientJwt: createFakeJWTWithClaims(claims),
		})
	}))
	defer server.Close()

	ctx := context.Background()
	byJwt, err := renewClientJWT(ctx, server.URL, "account-jwt", clientId, "test")
	if err != nil {
		t.Fatal(err)
	}
	if gotClientId != clientId.String() {
		t.Fatalf("renewal sent client_id %q, want %q", gotClientId, clientId.String())
	}
	// The returned JWT must carry the same client_id value we passed.
	claims := decodeFakeJWTClaims(t, byJwt)
	if claims["client_id"] != clientId.String() {
		t.Fatalf("renewed JWT client_id = %v, want %q", claims["client_id"], clientId.String())
	}
}

func decodeFakeJWTClaims(t *testing.T, jwt string) map[string]interface{} {
	t.Helper()
	parts := splitJwtPartsUnsafe(jwt)
	if len(parts) < 2 {
		t.Fatalf("jwt %q has %d parts, want >=2", jwt, len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	claims := map[string]interface{}{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	return claims
}

func splitJwtPartsUnsafe(jwt string) []string {
	// Minimal splitter for the fake tokens produced by createFakeJWTWithClaims.
	var parts []string
	start := 0
	for i := 0; i < len(jwt); i++ {
		if jwt[i] == '.' {
			parts = append(parts, jwt[start:i])
			start = i + 1
		}
	}
	parts = append(parts, jwt[start:])
	return parts
}

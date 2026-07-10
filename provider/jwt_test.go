package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func createFakeJWTWithClaims(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
}

func createFakeJWT(exp int64) string {
	return createFakeJWTWithClaims(map[string]interface{}{"exp": float64(exp)})
}

func TestValidateJWTExpiry(t *testing.T) {
	tests := []struct {
		name    string
		jwtFunc func() string
		wantErr error
	}{
		{
			name: "Valid token in the future",
			jwtFunc: func() string {
				// Expires in 1 hour
				return createFakeJWT(time.Now().Unix() + 3600)
			},
			wantErr: nil,
		},
		{
			name: "Expired token",
			jwtFunc: func() string {
				// Expired 1 hour ago
				return createFakeJWT(time.Now().Unix() - 3600)
			},
			wantErr: ErrTokenInvalid,
		},
		{
			name: "Expired token (barely expired > 30s)",
			jwtFunc: func() string {
				// Expired 31 seconds ago (caught by -30 leeway)
				return createFakeJWT(time.Now().Unix() - 31)
			},
			wantErr: ErrTokenInvalid,
		},
		{
			name: "Valid token (within 30s leeway)",
			jwtFunc: func() string {
				// Expired 15 seconds ago (allowed by -30 leeway)
				return createFakeJWT(time.Now().Unix() - 15)
			},
			wantErr: nil,
		},
		{
			name: "Invalid token format (should pass through to API)",
			jwtFunc: func() string {
				return "invalid.token.format"
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := tt.jwtFunc()
			err := validateJWTExpiry(token)
			if err != tt.wantErr {
				t.Errorf("validateJWTExpiry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJWTContainsClientId(t *testing.T) {
	tests := []struct {
		name string
		jwt  string
		want bool
	}{
		{
			name: "network JWT without client_id",
			jwt: createFakeJWTWithClaims(map[string]interface{}{
				"network_id":   "abc123",
				"user_id":      "def456",
				"network_name": "testnet",
				"exp":          float64(time.Now().Unix() + 86400),
			}),
			want: false,
		},
		{
			name: "client JWT with client_id",
			jwt: createFakeJWTWithClaims(map[string]interface{}{
				"network_id": "abc123",
				"user_id":    "def456",
				"client_id":  "xyz789",
				"device_id":  "dev001",
				"exp":        float64(time.Now().Unix() + 86400),
			}),
			want: true,
		},
		{
			name: "invalid JWT",
			jwt:  "not.a.jwt",
			want: false,
		},
		{
			name: "empty string",
			jwt:  "",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jwtContainsClientId(tt.jwt)
			if got != tt.want {
				t.Errorf("jwtContainsClientId() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJWTNetworkId(t *testing.T) {
	tests := []struct {
		name   string
		jwt    string
		want   string
		wantOk bool
	}{
		{
			name: "network JWT with network_id",
			jwt: createFakeJWTWithClaims(map[string]interface{}{
				"network_id": "net-abc123",
				"user_id":    "def456",
				"exp":        float64(time.Now().Unix() + 86400),
			}),
			want:   "net-abc123",
			wantOk: true,
		},
		{
			name: "JWT missing network_id",
			jwt: createFakeJWTWithClaims(map[string]interface{}{
				"user_id": "def456",
				"exp":     float64(time.Now().Unix() + 86400),
			}),
			want:   "",
			wantOk: false,
		},
		{
			name:   "invalid JWT",
			jwt:    "not.a.jwt",
			want:   "",
			wantOk: false,
		},
		{
			name:   "empty string",
			jwt:    "",
			want:   "",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := jwtNetworkId(tt.jwt)
			if got != tt.want || ok != tt.wantOk {
				t.Errorf("jwtNetworkId() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOk)
			}
		})
	}
}

func TestHotRestartEnabled(t *testing.T) {
	// must default to on unless explicitly disabled via URNETWORK_HOT_RESTART=0
	t.Setenv("URNETWORK_HOT_RESTART", "")
	if !hotRestartEnabled() {
		t.Error("hotRestartEnabled() = false with URNETWORK_HOT_RESTART='', want true")
	}

	t.Setenv("URNETWORK_HOT_RESTART", "0")
	if hotRestartEnabled() {
		t.Error("hotRestartEnabled() = true with URNETWORK_HOT_RESTART=0, want false")
	}

	// various other inputs (e.g. "1") should leave it enabled
	for _, v := range []string{"1", "true", "yes", "off"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("URNETWORK_HOT_RESTART", v)
			if !hotRestartEnabled() {
				t.Errorf("hotRestartEnabled() = false with URNETWORK_HOT_RESTART=%q, want true (only \"0\" disables it)", v)
			}
		})
	}
}

package main

import (
	"testing"
	"time"
	"fmt"
	"encoding/base64"
	"encoding/json"
)

func createFakeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(map[string]interface{}{"exp": float64(exp)})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
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

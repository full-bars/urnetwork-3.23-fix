package main

// Network token renewal at POST /auth/network-refresh.
//
// The three-step code-create/code-login flow still works, but every run mints
// an auth code and a new sign-in session. The dedicated route renews the
// presented network token in one call and keeps its create time, roles,
// principal and guest mode. The route is newer than the oldest servers this
// provider may talk to, so it is tried first and the three-step flow stays as
// the fallback.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/connect"
)

// networkRefreshTokenResult is `NetworkRefreshTokenResult` in the server API.
type networkRefreshTokenResult struct {
	ByJwt string                    `json:"by_jwt,omitempty"`
	Error *networkRefreshTokenError `json:"error,omitempty"`
}

type networkRefreshTokenError struct {
	Message string `json:"message"`
}

// networkRefreshFallbackStatus reports whether a non-200 response means the
// legacy three-step flow should run instead.
//
// A missing route (404, 405, 501) is a server that predates the route. Any
// other 4xx except 401, 408, 425 and 429 is a refusal of this request: a client
// JWT, an API key, or a sign-in the server will not renew. The legacy flow is
// what ran before the route existed, so it gets the request. 401, 408, 425 and
// 429 return an error instead: 408, 425 and 429 are transient, and 401 means
// the legacy code-create call would present the same dead token, fail the same
// way, and mask the real error. 5xx is a server failure and returns an error
// too, so the caller keeps the current JWT and retries on its next hourly
// pass.
func networkRefreshFallbackStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	case http.StatusNotImplemented:
		return true
	}
	return 400 <= statusCode && statusCode < 500
}

// networkRefreshJWT renews a network JWT at POST /auth/network-refresh.
//
// fallback is true when the caller should use the three-step flow: the
// presented credential is not a network token, the server does not have the
// route, or the server refused the request. The returned error then explains
// why. A transport error, a 5xx, a transient 4xx, or a malformed success
// response returns fallback false: another flow would either repeat the
// failure or hide a server problem.
func networkRefreshJWT(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl, byJwt string) (string, bool, error) {
	if byJwt == "" {
		return "", true, fmt.Errorf("no network JWT to renew")
	}
	// The route is network only: the server refuses a client JWT before the
	// handler runs. Never present one, and never let a client JWT reach the
	// handler through a route mismatch.
	if jwtContainsClientId(byJwt) {
		return "", true, fmt.Errorf("presented token is a client JWT, not a network JWT")
	}

	body, err := connect.HttpPostWithStrategyRaw(
		ctx,
		clientStrategy,
		apiUrl+"/auth/network-refresh",
		nil,
		byJwt,
	)
	if err != nil {
		var status *connect.HttpStatusError
		if errors.As(err, &status) {
			if networkRefreshFallbackStatus(status.StatusCode) {
				return "", true, fmt.Errorf("network-refresh refused (HTTP %d): %w", status.StatusCode, err)
			}
			return "", false, fmt.Errorf("network-refresh api error: %w", err)
		}
		return "", false, fmt.Errorf("network-refresh request failed: %w", err)
	}

	var result networkRefreshTokenResult
	if err := json.Unmarshal(body, &result); err != nil {
		return "", false, fmt.Errorf("network-refresh response decode failed: %w", err)
	}
	if result.Error != nil {
		return "", true, fmt.Errorf("network-refresh refused: %s", result.Error.Message)
	}
	if result.ByJwt == "" {
		return "", true, fmt.Errorf("network-refresh returned no token")
	}
	if jwtContainsClientId(result.ByJwt) {
		return "", false, fmt.Errorf("regression guard: network-refresh returned a client JWT instead of a network JWT")
	}
	return result.ByJwt, false, nil
}

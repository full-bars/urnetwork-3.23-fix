// The subset of the sn clientauth package that the claim daemon's credential
// path uses, copied from sn 4ca8316e (clientauth/session.go,
// clientauth/registration_identity.go) with the sdk import removed: the sn
// package imports sdk for its measurement files, which the v33 lane does not
// carry. Only ReadToken, ClientIdFromJwt and ValidateRefreshedClientJwt and
// their private helpers travel.
package clientauth

import (
	"fmt"
	"os"
	"strings"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// ReadToken reads a bearer token file. Local wiring only; the server remains
// the authority that verifies it.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// ClientIdFromJwt extracts the client identity for local wiring only. The
// token is authoritatively verified by the server when it refreshes/connects.
func ClientIdFromJwt(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return connect.Id{}, err
	}
	clientIdString, ok := claims["client_id"].(string)
	if !ok || clientIdString == "" {
		return connect.Id{}, fmt.Errorf("client JWT has no client_id")
	}
	deviceIdString, ok := claims["device_id"].(string)
	if !ok || deviceIdString == "" {
		return connect.Id{}, fmt.Errorf("client JWT has no device_id")
	}
	return connect.ParseId(clientIdString)
}

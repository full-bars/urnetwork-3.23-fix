package clientauth

// These claims select local ownership only. The API remains the authority
// that verifies bearer signatures, revocation and network membership.

import (
	"errors"
	"slices"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

// Stable principal fields exclude token timestamps, bearer bytes and builds.
type registrationPrincipal struct {
	NetworkId string   `json:"network_id"`
	UserId    string   `json:"user_id"`
	Roles     []string `json:"roles"`
	Principal string   `json:"principal"`
}

// Known client/device identity must survive every credential rotation.
type registrationTokenIdentity struct {
	registrationPrincipal
	clientId string
	deviceId string
}

func registrationIdentity(token string) (registrationTokenIdentity, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(token, claims); err != nil {
		return registrationTokenIdentity{}, errors.New("client registration credential is malformed")
	}
	var identity registrationTokenIdentity
	for _, field := range []struct {
		name  string
		value *string
	}{
		{name: "network_id", value: &identity.NetworkId}, {name: "user_id", value: &identity.UserId},
		{name: "principal", value: &identity.Principal}, {name: "client_id", value: &identity.clientId}, {name: "device_id", value: &identity.deviceId},
	} {
		if raw, exists := claims[field.name]; exists {
			value, ok := raw.(string)
			if !ok {
				return registrationTokenIdentity{}, errors.New("client registration identity claim has invalid type")
			}
			*field.value = value
		}
	}
	if raw, exists := claims["roles"]; exists && raw != nil {
		roles, ok := raw.([]any)
		if !ok {
			return registrationTokenIdentity{}, errors.New("client registration roles have invalid type")
		}
		for _, raw := range roles {
			role, ok := raw.(string)
			if !ok || role == "" {
				return registrationTokenIdentity{}, errors.New("client registration role is invalid")
			}
			identity.Roles = append(identity.Roles, role)
		}
		slices.Sort(identity.Roles)
		identity.Roles = slices.Compact(identity.Roles)
	}
	return identity, nil
}

// This equality does not grant server authority; it prevents a completed
// response from silently switching the durable process to another identity.
func (self registrationPrincipal) equal(other registrationPrincipal) bool {
	return self.NetworkId == other.NetworkId && self.UserId == other.UserId && self.Principal == other.Principal && slices.Equal(self.Roles, other.Roles)
}

func validRegistrationIdentityId(value string) bool {
	id, err := connect.ParseId(value)
	return err == nil && id != (connect.Id{}) && id.String() == value
}

// Used both during restart and before a live refresh listener persists a token.
func ValidateRefreshedClientJwt(original, refreshed string) error {
	before, beforeErr := registrationIdentity(original)
	after, afterErr := registrationIdentity(refreshed)
	if beforeErr != nil || afterErr != nil || !validRegistrationIdentityId(before.clientId) || !validRegistrationIdentityId(before.deviceId) || before.clientId != after.clientId || before.deviceId != after.deviceId || !before.registrationPrincipal.equal(after.registrationPrincipal) {
		return errors.New("refreshed client credential changed its original identity")
	}
	return nil
}

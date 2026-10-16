// Real credential-owner behavior from sn 4ca8316e's sn_credentials_test.go.
// The sn-generation claim-command tests of that file need the v33's own claim
// command and do not travel with this family; the credential owner test below
// exercises the daemon lane's refresh and identity-preservation directly.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

// The daemon API observes a renewed file without replacing its provider,
// and an unchanged disk token cannot roll back a completed refresh.
func TestClaimProviderCredentialReloadPreservesOriginalIdentityAndRefresh(t *testing.T) {
	original := setTestProviderJwt(t)
	renewed := financialTestJwt(t, financialTestClientId, "sdk-renewed")
	diskRenewed := financialTestJwt(t, financialTestClientId, "disk-renewed")
	var requests atomic.Int32
	var authLock sync.Mutex
	var authorizations, refreshAuthorizations []string
	refreshToken := renewed
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			return
		case "/auth/refresh":
			authLock.Lock()
			refreshAuthorizations = append(refreshAuthorizations, r.Header.Get("Authorization"))
			reply := refreshToken
			authLock.Unlock()
			_ = json.NewEncoder(w).Encode(refreshJwtResult{ByJwt: reply})
		case "/sn/pool/claim":
			requests.Add(1)
			authLock.Lock()
			authorizations = append(authorizations, r.Header.Get("Authorization"))
			authLock.Unlock()
			_ = json.NewEncoder(w).Encode(finiteClaimTestResult())
		default:
			t.Error("unexpected credential route", r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, finiteClaimTestSettings())
	defer strategy.Close()
	api := newDaemonApi(ctx, strategy, endpoint.URL)
	defer api.Close()
	owner := &claimCredentialApi{api: api}
	if _, err := owner.SnPoolClaimSyncWithContext(ctx, &connect.SnPoolClaimArgs{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.SnPoolClaimSyncWithContext(ctx, &connect.SnPoolClaimArgs{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	authLock.Lock()
	second := authorizations[1]
	secondRefresh := refreshAuthorizations[1]
	authLock.Unlock()
	if second != "Bearer "+renewed || secondRefresh != "Bearer "+renewed || api.ByJwt() != renewed {
		t.Fatal("unchanged original file rolled back renewal")
	}
	path := testProviderStatePath(t, ".provider.jwt")
	if raw, err := os.ReadFile(path); err != nil || string(raw) != original {
		t.Fatal("daemon rewrote provider-owned credential", err)
	}
	if err := os.WriteFile(path, []byte(diskRenewed), 0600); err != nil {
		t.Fatal(err)
	}
	// Observe the accepted disk generation; the synchronous refresh may already
	// have advanced the in-memory token again under the same original identity.
	if _, err := owner.SnPoolClaimSyncWithContext(ctx, &connect.SnPoolClaimArgs{Epoch: 1}); err != nil || owner.diskToken != diskRenewed {
		t.Fatal("same-provider disk renewal refused", err)
	}
	authLock.Lock()
	thirdRefresh := refreshAuthorizations[2]
	authLock.Unlock()
	if thirdRefresh != "Bearer "+diskRenewed {
		t.Fatal("changed retained credential did not reach the refresh owner")
	}
	before := requests.Load()
	for _, replacement := range []string{financialTestJwt(t, "00000000-0000-0000-0000-000000000202", "sibling"), financialTestJwt(t, "", "network")} {
		if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.SnPoolClaimSyncWithContext(ctx, &connect.SnPoolClaimArgs{Epoch: 1}); err == nil {
			t.Fatal("daemon changed original provider")
		}
	}
	if requests.Load() != before || !bytes.Equal([]byte(owner.original), []byte(original)) {
		t.Fatal("changed identity reached proof read")
	}
	if err := os.WriteFile(path, []byte(diskRenewed), 0600); err != nil {
		t.Fatal(err)
	}
	// A validly encoded refresh response cannot change any original identity
	// coordinate before publication or the subsequent proof read.
	for claim, value := range map[string]any{"client_id": "00000000-0000-0000-0000-000000000202", "device_id": "00000000-0000-0000-0000-000000000502", "network_id": "00000000-0000-0000-0000-000000000302", "user_id": "00000000-0000-0000-0000-000000000402", "roles": []string{"owner"}, "principal": "another-owner"} {
		claims := gojwt.MapClaims{}
		if _, _, err := gojwt.NewParser().ParseUnverified(renewed, claims); err != nil {
			t.Fatal(err)
		}
		claims[claim] = value
		changed, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatal(err)
		}
		authLock.Lock()
		refreshToken = changed
		authLock.Unlock()
		if _, err := owner.SnPoolClaimSyncWithContext(ctx, &connect.SnPoolClaimArgs{Epoch: 1}); err == nil || api.ByJwt() != renewed || requests.Load() != before {
			t.Fatal("changed refresh identity was published", claim, err)
		}
	}
}

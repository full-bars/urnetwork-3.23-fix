// Fixtures for the claim daemon family, carried from sn 4ca8316e's
// sn_credentials_test.go, sn_wallet_test.go and sn_claim_command_test.go. Only
// the helpers this family's tests need travel; the sn-generation claim-command
// fixtures stay with their own command suite.
package main

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urfoundation/sn/merkle"
	"github.com/urnetwork/connect"
)

const financialTestClientId = "00000000-0000-0000-0000-000000000201"

// This is local identity syntax, never a production credential or key.
func financialTestJwt(t testing.TB, clientId, marker string) string {
	t.Helper()
	claims := gojwt.MapClaims{"network_id": "00000000-0000-0000-0000-000000000301", "user_id": "00000000-0000-0000-0000-000000000401", "roles": []string{"provider"}, "principal": "synthetic-financial-owner", "exp": time.Now().Add(24 * time.Hour).Unix(), "marker": marker}
	if clientId != "" {
		claims["client_id"], claims["device_id"] = clientId, "00000000-0000-0000-0000-000000000501"
	}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// testProviderStatePath returns a named file under the (test) home's
// provider state directory, the same layout providerStatePath uses.
func testProviderStatePath(t *testing.T, name string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".urnetwork", name)
}

// setTestNetworkJwt points the provider state dir at a temp home holding the
// network jwt `provider auth` would have written.
func setTestNetworkJwt(t *testing.T, jwt string) {
	t.Helper()
	withTempHome(t)
	stateDir := filepath.Dir(testProviderStatePath(t, "jwt"))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testProviderStatePath(t, "jwt"), []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Both files exist so a regression to the bootstrap token is observable.
func setTestProviderJwt(t *testing.T) string {
	t.Helper()
	setTestNetworkJwt(t, financialTestJwt(t, "", "bootstrap"))
	token := financialTestJwt(t, financialTestClientId, "provider")
	if err := os.WriteFile(testProviderStatePath(t, ".provider.jwt"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	return token
}

// Synthetic complete one-leaf proof for a claim read. Only the credential
// owner's reads consume it; no proof decision runs here.
func finiteClaimTestResult() *connect.SnPoolClaimResult {
	coldkey := [32]byte{17, 29, 41}
	root := merkle.PayoutLeaf(coldkey, big.NewInt(10000))
	return &connect.SnPoolClaimResult{Epoch: 1, NoId: []byte{7}, Coldkey: coldkey[:], ShareBps: 10000, PayoutRoot: root[:], Proof: [][]byte{}, ContractAddress: "0x0000000000000000000000000000000000000007", ChainId: 945}
}

// Use the real native HTTP strategy without route racing or inner pauses so
// each completed read is one independently observed request. The sn fixture
// also pinned a zero GET retry count; the v33 strategy has no such field, so
// the native transport settings alone are used.
func finiteClaimTestSettings() *connect.ClientStrategySettings {
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.RequestTimeout = 2 * time.Second
	settings.ConnectTimeout = time.Second
	return settings
}

// newClaimQueueTestStore retains explicit ownership until the test work ends
// (from sn 4ca8316e's claim_queue_owner_test.go; the queue-owner suite that
// defines its siblings stays with its own family).
func newClaimQueueTestStore(t *testing.T, directory string) *claimQueueStore {
	t.Helper()
	store, err := newClaimQueueStore(directory, claimQueueTestContext(t, t.Context(), directory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

// parseArgsForTest parses argv against mainUsage without exiting on error
// (from sn 4ca8316e's main_test.go).
func parseArgsForTest(t *testing.T, argv []string) docopt.Opts {
	t.Helper()
	parser := &docopt.Parser{
		HelpHandler: docopt.NoHelpHandler,
	}
	opts, err := parser.ParseArgs(mainUsage(), argv, "test")
	if err != nil {
		t.Fatalf("parse %v: %s", argv, err)
	}
	return opts
}

// claimPollTestHooks fails unexpected side effects immediately (from sn
// 4ca8316e's claim_queue_poll_test.go).
func claimPollTestHooks(t *testing.T, now time.Time) claimQueuePollHooks {
	t.Helper()
	return claimQueuePollHooks{
		now: func() time.Time { return now },
		save: func(*ClaimQueue) error {
			t.Fatal("unexpected durable write")
			return nil
		},
		reconcile: func(context.Context, *ClaimQueueEntry) (string, error) {
			t.Fatal("unexpected reconciliation")
			return "", nil
		},
		submit: func(context.Context, *ClaimQueueEntry) error {
			t.Fatal("unexpected transaction submission")
			return nil
		},
	}
}

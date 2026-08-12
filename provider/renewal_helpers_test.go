package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// --- jwtClientId ---------------------------------------------------------

func TestJwtClientId_Valid(t *testing.T) {
	jwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": "client-abc-123",
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	if got := jwtClientId(jwt); got != "client-abc-123" {
		t.Fatalf("jwtClientId = %q, want %q", got, "client-abc-123")
	}
}

func TestJwtClientId_MissingClaim(t *testing.T) {
	// A structurally valid JWT that simply has no client_id claim.
	jwt := createFakeJWTWithClaims(map[string]interface{}{
		"network_id": "net-1",
	})
	if got := jwtClientId(jwt); got != "" {
		t.Fatalf("jwtClientId with no client_id claim = %q, want empty string", got)
	}
}

func TestJwtClientId_Garbage(t *testing.T) {
	if got := jwtClientId("not-a-jwt"); got != "" {
		t.Fatalf("jwtClientId(garbage) = %q, want empty string", got)
	}
	if got := jwtClientId(""); got != "" {
		t.Fatalf("jwtClientId(\"\") = %q, want empty string", got)
	}
}

func TestJwtClientId_NonStringClaim(t *testing.T) {
	// A client_id claim that isn't a string must not be type-asserted into a
	// panic; jwtClientId should just report it as absent.
	jwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": 12345,
	})
	if got := jwtClientId(jwt); got != "" {
		t.Fatalf("jwtClientId with non-string client_id claim = %q, want empty string", got)
	}
}

// --- accountNetworkId -----------------------------------------------------

func TestAccountNetworkId_Valid(t *testing.T) {
	jwt := createFakeJWTWithClaims(map[string]interface{}{
		"network_id": "net-xyz",
	})
	if got := accountNetworkId(jwt); got != "net-xyz" {
		t.Fatalf("accountNetworkId = %q, want %q", got, "net-xyz")
	}
}

func TestAccountNetworkId_MalformedTokenReturnsEmpty(t *testing.T) {
	// The store treats an empty NetworkID as "mint-fresh", so a malformed
	// account JWT must degrade to "" rather than panicking or erroring.
	if got := accountNetworkId("not-a-jwt"); got != "" {
		t.Fatalf("accountNetworkId(garbage) = %q, want empty string", got)
	}
	if got := accountNetworkId(""); got != "" {
		t.Fatalf("accountNetworkId(\"\") = %q, want empty string", got)
	}
}

func TestAccountNetworkId_MissingClaimReturnsEmpty(t *testing.T) {
	jwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": "some-client",
	})
	if got := accountNetworkId(jwt); got != "" {
		t.Fatalf("accountNetworkId with no network_id claim = %q, want empty string", got)
	}
}

// --- readAccountJWT --------------------------------------------------------

func TestReadAccountJWT_Success(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	urnetworkDir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(urnetworkDir, 0700); err != nil {
		t.Fatal(err)
	}
	const token = "account.jwt.token"
	if err := os.WriteFile(filepath.Join(urnetworkDir, "jwt"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := readAccountJWT()
	if err != nil {
		t.Fatal(err)
	}
	if got != token {
		t.Fatalf("readAccountJWT = %q, want %q", got, token)
	}
}

func TestReadAccountJWT_TrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	urnetworkDir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(urnetworkDir, 0700); err != nil {
		t.Fatal(err)
	}
	const token = "account.jwt.token"
	if err := os.WriteFile(filepath.Join(urnetworkDir, "jwt"), []byte("\n  "+token+"  \n"), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := readAccountJWT()
	if err != nil {
		t.Fatal(err)
	}
	if got != token {
		t.Fatalf("readAccountJWT did not trim whitespace: got %q, want %q", got, token)
	}
}

func TestReadAccountJWT_MissingFile(t *testing.T) {
	home := t.TempDir() // no .urnetwork/jwt inside
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := readAccountJWT(); err == nil {
		t.Fatal("expected an error when ~/.urnetwork/jwt does not exist")
	}
}

// --- providerDescription ---------------------------------------------------

func TestProviderDescription_UsesNodeName(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "")
	t.Setenv("URNETWORK_PUBLIC_IP", "")

	got := providerDescription("mybox")
	want := "mybox [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

func TestProviderDescription_FallsBackToHostHostnameWhenNodeNameEmpty(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "docker-host")
	t.Setenv("URNETWORK_PUBLIC_IP", "")

	got := providerDescription("")
	want := "docker-host [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

func TestProviderDescription_FallsBackToOSHostnameWhenNoOverridesSet(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "")
	t.Setenv("URNETWORK_PUBLIC_IP", "")

	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("cannot determine os.Hostname() in this environment")
	}

	// Passing the real hostname explicitly as nodeName takes the exact same
	// downstream path (isContainerID / IP formatting) that providerDescription
	// takes internally when it falls back to os.Hostname(), so the two calls
	// must agree without this test having to reimplement that logic itself.
	got := providerDescription("")
	want := providerDescription(hostname)
	if got != want {
		t.Fatalf("providerDescription(\"\") = %q, want %q (matching providerDescription(%q))", got, want, hostname)
	}
}

func TestProviderDescription_ContainerIdNameWithoutPublicIpBecomesGenericProvider(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "abcdef012345") // 12-char hex -> matches containerIDRe
	t.Setenv("URNETWORK_PUBLIC_IP", "")

	got := providerDescription("")
	want := "provider [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

func TestProviderDescription_ContainerIdNameWithPublicIpUsesRedactedIpOnly(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "abcdef012345") // container id gibberish
	t.Setenv("URNETWORK_PUBLIC_IP", "203.0.113.42")

	got := providerDescription("")
	want := "203.x.x.42 [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

func TestProviderDescription_NodeNameWithPublicIpAppendsRedactedIp(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "")
	t.Setenv("URNETWORK_PUBLIC_IP", "203.0.113.42")

	got := providerDescription("mybox")
	want := "mybox @ 203.x.x.42 [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

func TestProviderDescription_NonIPv4PublicIpIsIgnored(t *testing.T) {
	t.Setenv("HOST_HOSTNAME", "")
	// Neither a valid IPv4 literal nor empty: the IPv4 parse must fail and
	// fall through to the no-IP branch instead of panicking on nil parts.
	for _, badIP := range []string{"not-an-ip", "::1", "2001:db8::1"} {
		t.Run(badIP, func(t *testing.T) {
			t.Setenv("URNETWORK_PUBLIC_IP", badIP)
			got := providerDescription("mybox")
			want := "mybox [" + RequireVersion() + "]"
			if got != want {
				t.Fatalf("providerDescription with public IP %q = %q, want %q", badIP, got, want)
			}
		})
	}
}

func TestProviderDescription_NodeNameItselfIsContainerId(t *testing.T) {
	// When nodeName is non-empty, the HOST_HOSTNAME/os.Hostname() fallback
	// branch is skipped entirely and displayName is set directly from
	// nodeName — this exercises container-id detection on that direct path
	// rather than on the fallback path (covered by the HOST_HOSTNAME tests
	// above).
	t.Setenv("HOST_HOSTNAME", "should-be-ignored")
	t.Setenv("URNETWORK_PUBLIC_IP", "")

	got := providerDescription("0123456789ab") // 12-char hex, non-empty nodeName
	want := "provider [" + RequireVersion() + "]"
	if got != want {
		t.Fatalf("providerDescription = %q, want %q", got, want)
	}
}

// --- watchReusedIdentityForRevocation / revocationDone ---------------------

func withTestClientJWTStore(t *testing.T) *clientJWTStore {
	t.Helper()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	old := globalClientJWTStore
	globalClientJWTStore = store
	t.Cleanup(func() { globalClientJWTStore = old })
	return store
}

func TestWatchReusedIdentityForRevocation_ContextCancelReturnsPromptly(t *testing.T) {
	store := withTestClientJWTStore(t)
	if err := store.Put("proxy-ctx-cancel", clientJWTEntry{
		ByClientJWT: "some-token",
		ClientID:    "client-1",
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the watcher even starts

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchReusedIdentityForRevocation(ctx, "proxy-ctx-cancel", 987654, nil)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReusedIdentityForRevocation did not return promptly on context cancellation")
	}

	if _, ok := store.Get("proxy-ctx-cancel"); !ok {
		t.Fatal("identity must not be evicted when the context is canceled before any tick fires")
	}
}

func TestWatchReusedIdentityForRevocation_RevocationDoneStandsDownImmediately(t *testing.T) {
	store := withTestClientJWTStore(t)
	if err := store.Put("proxy-revocation-done", clientJWTEntry{
		ByClientJWT: "some-token",
		ClientID:    "client-2",
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Simulates the renewal watcher having already closed revocationDone
	// (i.e. a successful renewal happened) before this watcher's select even
	// runs.
	revocationDone := make(chan struct{})
	close(revocationDone)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchReusedIdentityForRevocation(ctx, "proxy-revocation-done", 987655, revocationDone)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReusedIdentityForRevocation did not stand down promptly when revocationDone was already closed")
	}

	if _, ok := store.Get("proxy-revocation-done"); !ok {
		t.Fatal("identity must not be evicted once the renewal watcher reported a successful renewal")
	}
}

func TestWatchReusedIdentityForRevocation_NilRevocationDoneDoesNotBlockContextCancel(t *testing.T) {
	// A nil revocationDone channel must behave like "never signaled" (receive
	// from nil blocks forever in the select) without panicking or otherwise
	// preventing the ctx.Done() case from firing.
	store := withTestClientJWTStore(t)
	if err := store.Put("proxy-nil-revocation", clientJWTEntry{
		ByClientJWT: "some-token",
		ClientID:    "client-3",
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchReusedIdentityForRevocation(ctx, "proxy-nil-revocation", 987656, nil)
	}()

	// Give the goroutine a moment to enter its select loop, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReusedIdentityForRevocation with a nil revocationDone did not return on context cancellation")
	}

	if _, ok := store.Get("proxy-nil-revocation"); !ok {
		t.Fatal("identity must not be evicted on plain context cancellation")
	}
}

// --- renewClientJWT: context cancellation ----------------------------------

// TestRenewClientJWTContextCanceled pins the ctx.Done() early-return path:
// renewClientJWT must not block waiting on a slow/hanging backend once its
// context is canceled.
func TestRenewClientJWTContextCanceled(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never responds until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before renewClientJWT is even called

	errCh := make(chan error, 1)
	jwtCh := make(chan string, 1)
	go func() {
		byJwt, err := renewClientJWT(ctx, server.URL, "account-jwt", connect.NewId(), "test", nil)
		jwtCh <- byJwt
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error when renewClientJWT is called with an already-canceled context")
		}
		if got := <-jwtCh; got != "" {
			t.Fatalf("renewClientJWT returned a non-empty JWT on context cancellation: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renewClientJWT did not return promptly on context cancellation")
	}
}
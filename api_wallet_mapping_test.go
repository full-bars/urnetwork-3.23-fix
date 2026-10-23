// The actual SDK HTTP owner carries the exact wallet-approved message without
// changing the authenticated client or invoking a signer on the user's behalf.
package connect

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// walletMappingTestApi serves `handler` and returns an api that posts to it,
// authenticated as `byJwt`. The fork's ordinary strategy over a local test
// server is the harness its api tests use (api_ranking_test.go); the upstream
// test drives a synthetic RoundTripper through the strategy instead, and that
// helper belongs to the transport tests this line does not carry.
func walletMappingTestApi(t *testing.T, byJwt string, handler http.HandlerFunc) *BringYourApi {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ctx := t.Context()
	api := NewBringYourApi(ctx, NewClientStrategyWithDefaults(ctx), server.URL)
	api.SetByJwt(byJwt)
	t.Cleanup(api.Close)
	return api
}

// Challenge acquisition and the later explicit signature submission remain two
// separate authenticated calls, preserving all display and signature bytes.
func TestWalletMappingApiKeepsExactAuthenticatedApproval(t *testing.T) {
	clientId := Id{1}
	message := "Approve URnetwork provider wallet mapping\nsynthetic exact original bytes"
	posts := 0
	api := walletMappingTestApi(t, "synthetic-wallet-api-token", func(w http.ResponseWriter, request *http.Request) {
		posts++
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer synthetic-wallet-api-token" {
			http.Error(w, "mapping request lost authenticated post", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if posts == 1 {
			var args SnWalletMappingChallengeArgs
			if request.URL.Path != "/sn/wallet/consent" || json.Unmarshal(raw, &args) != nil || args.ClientId == nil || *args.ClientId != clientId || args.FromEpoch != 0 || args.ThroughEpoch != 100 {
				http.Error(w, "mapping challenge changed original selection", http.StatusBadRequest)
				return
			}
			body, _ := json.Marshal(SnWalletMappingChallengeResult{Message: message})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		var args SnSetWalletArgs
		if posts != 2 || request.URL.Path != "/sn/wallet" || json.Unmarshal(raw, &args) != nil || args.ClientId == nil || *args.ClientId != clientId || args.Message != message || args.Signature != "0x"+strings.Repeat("12", 64) {
			http.Error(w, "mapping submission changed signed original", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mapping_hash":"synthetic-original-hash","mapping_generation":1}`)
	})
	challenge, err := api.SnWalletMappingChallengeSync(&SnWalletMappingChallengeArgs{ClientId: &clientId, ColdkeySs58: "synthetic-wallet", FromEpoch: 0, ThroughEpoch: 100})
	if err != nil || challenge == nil || challenge.Message != message || posts != 1 {
		t.Fatal("challenge was changed or submitted without signature", challenge, posts, err)
	}
	result, err := api.SnSetWalletSync(&SnSetWalletArgs{ClientId: &clientId, ColdkeySs58: "synthetic-wallet", Message: challenge.Message, Signature: "0x" + strings.Repeat("12", 64)})
	if err != nil || result == nil || result.MappingHash != "synthetic-original-hash" || result.MappingGeneration != 1 || posts != 2 {
		t.Fatal("actual mapping acknowledgement was lost", result, posts, err)
	}
}

// Legacy callers retain their prior wire grammar and receive no invented
// mapping generation merely because the new optional fields exist.
func TestWalletMappingApiLegacyRequestRetainsMissingConsent(t *testing.T) {
	posts := 0
	api := walletMappingTestApi(t, "synthetic-wallet-api-token", func(w http.ResponseWriter, request *http.Request) {
		posts++
		raw, err := io.ReadAll(request.Body)
		if err != nil || strings.Contains(string(raw), "message") || strings.Contains(string(raw), "signature") {
			http.Error(w, "legacy request acquired invented mapping fields", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	value, err := api.SnSetWalletSync(&SnSetWalletArgs{ColdkeySs58: "synthetic-legacy"})
	if err != nil || value == nil || value.MappingHash != "" || value.MappingGeneration != 0 || posts != 1 {
		t.Fatal("legacy request became authenticated mapping evidence", value, err)
	}
}

// The network consent is requested from its own route with no client, and
// submitted through the same wallet route without a client.
func TestNetworkWalletMappingApiRequestsWithoutClient(t *testing.T) {
	message := "Approve URnetwork network wallet mapping\nsynthetic exact original bytes"
	posts := 0
	api := walletMappingTestApi(t, "synthetic-network-wallet-token", func(w http.ResponseWriter, request *http.Request) {
		posts++
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer synthetic-network-wallet-token" {
			http.Error(w, "network mapping request lost authenticated post", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if posts == 1 {
			var args map[string]any
			if request.URL.Path != "/sn/wallet/network-consent" || json.Unmarshal(raw, &args) != nil || args["client_id"] != nil || args["coldkey_ss58"] != "synthetic-wallet" || args["from_epoch"] != float64(7) || args["through_epoch"] != float64(107) {
				http.Error(w, "network mapping challenge changed its selection", http.StatusBadRequest)
				return
			}
			body, _ := json.Marshal(SnWalletMappingChallengeResult{Message: message})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		var args SnSetWalletArgs
		if posts != 2 || request.URL.Path != "/sn/wallet" || json.Unmarshal(raw, &args) != nil || args.ClientId != nil || args.Message != message {
			http.Error(w, "network mapping submission changed signed original", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mapping_hash":"synthetic-network-hash","mapping_generation":1}`)
	})
	challenge, err := api.SnNetworkWalletMappingChallengeSync(&SnNetworkWalletMappingChallengeArgs{ColdkeySs58: "synthetic-wallet", FromEpoch: 7, ThroughEpoch: 107})
	if err != nil || challenge == nil || challenge.Message != message || posts != 1 {
		t.Fatal("network challenge was changed", challenge, posts, err)
	}
	result, err := api.SnSetWalletSync(&SnSetWalletArgs{ColdkeySs58: "synthetic-wallet", Message: challenge.Message, Signature: "0x" + strings.Repeat("12", 64)})
	if err != nil || result == nil || result.MappingHash != "synthetic-network-hash" || result.MappingGeneration != 1 || posts != 2 {
		t.Fatal("network mapping acknowledgement was lost", result, posts, err)
	}
}

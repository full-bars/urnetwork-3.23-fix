package urnettools

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

func TestFetchSnStatus_MockEndpoints(t *testing.T) {
	expectedColdkey := "5GbD4Vk6cASzfgkywuGkGHszPSr1s6gx9y9fFBDjLV2q1GWS"
	pubkey, err := ss58.DecodeWithPrefix(expectedColdkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatalf("failed to decode test coldkey: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/network/ranking":
			resp := connect.NetworkRankingResult{
				NetworkRanking: &connect.NetworkRanking{
					NetMibCount:       1234567.89,
					LeaderboardRank:   7,
					LeaderboardPublic: true,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/sn/epoch":
			resp := connect.SnEpochResult{
				Epoch:               50,
				StartBlock:          50000,
				CommitDeadlineBlock: 50300,
				TrailsDeadlineBlock: 50600,
				FinalizeBlock:       50720,
				ContractAddress:     "0x1111222233334444555566667777888899990000",
				ChainId:             964,
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/sn/pool/claim":
			resp := connect.SnPoolClaimResult{
				Epoch:      49,
				Coldkey:    pubkey[:],
				ShareBps:   450,
				PayoutRoot: make([]byte, 32),
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "jwt"), []byte("test-jwt"), 0600); err != nil {
		t.Fatalf("failed to write mock jwt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "api_url"), []byte(ts.URL), 0600); err != nil {
		t.Fatalf("failed to write mock api_url: %v", err)
	}

	p := Provider{
		Unit:      "urnetwork-test.service",
		User:      "testuser",
		StateDir:  tmpDir,
		NetworkID: "net-uuid-meso",
	}

	info, err := FetchSnStatus(p)
	if err != nil {
		t.Fatalf("FetchSnStatus failed: %v", err)
	}

	if info.LeaderboardRank != 7 {
		t.Errorf("expected rank 7, got %d", info.LeaderboardRank)
	}
	if !info.Top200Eligible {
		t.Errorf("expected Top200Eligible=true for rank 7")
	}
	if !strings.Contains(info.TierDescription, "Tier 1 Elite") {
		t.Errorf("expected Tier 1 Elite description, got %q", info.TierDescription)
	}
	if info.NetMibCount != 1234567.89 {
		t.Errorf("expected NetMibCount 1234567.89, got %f", info.NetMibCount)
	}
	if info.ColdkeySs58 != expectedColdkey {
		t.Errorf("expected ColdkeySs58 %s, got %s", expectedColdkey, info.ColdkeySs58)
	}
	if info.CurrentEpoch != 50 {
		t.Errorf("expected CurrentEpoch 50, got %d", info.CurrentEpoch)
	}
	if info.PayoutShareBps != 450 {
		t.Errorf("expected PayoutShareBps 450, got %d", info.PayoutShareBps)
	}
}

func TestRenderSnStatusDashboard(t *testing.T) {
	info := &SnStatusInfo{
		ProviderUnit:      "urnetwork-main.service",
		StateDir:          "/var/lib/urnetwork",
		NetworkName:       "mesocyclone",
		NetworkID:         "uuid-1234",
		LeaderboardRank:   7,
		LeaderboardPublic: true,
		NetMibCount:       5242880.0, // 5 GiB
		ColdkeySs58:       "5GbD4Vk6cASzfgkywuGkGHszPSr1s6gx9y9fFBDjLV2q1GWS",
		Top200Eligible:    true,
		TierDescription:   "Tier 1 Elite (Rank #7 Globally)",
		CurrentEpoch:      100,
		StartBlock:        100000,
		FinalizeBlock:     100720,
		ContractAddress:   "0x000000000000000000000000000000000000dEaD",
		ChainID:           964,
		PayoutShareBps:    850,
		ClaimEpoch:        99,
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	renderSnStatusDashboard(info)

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "URNetwork Subnet 25 — Node & Miner Status") {
		t.Errorf("dashboard missing header")
	}
	if !strings.Contains(out, "mesocyclone") {
		t.Errorf("dashboard missing network name")
	}
	if !strings.Contains(out, "#7") {
		t.Errorf("dashboard missing rank")
	}
	if !strings.Contains(out, "5120.00 GiB") {
		t.Errorf("dashboard missing formatted GiB bandwidth")
	}
	if !strings.Contains(out, "5GbD4Vk6cASzfgkywuGkGHszPSr1s6gx9y9fFBDjLV2q1GWS") {
		t.Errorf("dashboard missing coldkey")
	}
	if !strings.Contains(out, "8.50%") {
		t.Errorf("dashboard missing formatted payout share percentage")
	}
}

func TestFetchSnStatus_EpochEdgeCasesAndErrors(t *testing.T) {
	var requestedClaimEpoch string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/network/ranking":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
		case "/sn/epoch":
			resp := connect.SnEpochResult{
				Epoch: 1, // current epoch is 1 -> finalized epoch should be 0
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/sn/pool/claim":
			requestedClaimEpoch = r.URL.Query().Get("epoch")
			resp := connect.SnPoolClaimResult{
				Epoch:    0,
				ShareBps: 100,
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "jwt"), []byte("test-jwt"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "api_url"), []byte(ts.URL), 0600)

	p := Provider{
		Unit:     "test.service",
		StateDir: tmpDir,
	}

	info, err := FetchSnStatus(p)
	if err != nil {
		t.Fatalf("FetchSnStatus failed: %v", err)
	}

	if requestedClaimEpoch != "0" {
		t.Errorf("expected claim requested for epoch 0, got %s", requestedClaimEpoch)
	}
	if info.ClaimEpoch != 0 {
		t.Errorf("expected ClaimEpoch 0, got %d", info.ClaimEpoch)
	}
	if info.PayoutShareBps != 100 {
		t.Errorf("expected PayoutShareBps 100, got %d", info.PayoutShareBps)
	}
	if info.Error == "" {
		t.Errorf("expected error to be populated for 500 status on ranking")
	}
}

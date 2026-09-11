package urnettools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

// mockSnStatusAPI is a test double that returns pre-configured API responses.
type mockSnStatusAPI struct {
	ranking *connect.NetworkRankingResult
	epoch   *connect.SnEpochResult
	claim   *connect.SnPoolClaimResult
	// Track calls for assertions
	byJwt string
}

func (m *mockSnStatusAPI) SetByJwt(jwt string) { m.byJwt = jwt }
func (m *mockSnStatusAPI) NetworkGetRankingSync() (*connect.NetworkRankingResult, error) {
	if m.ranking == nil {
		return nil, fmt.Errorf("network ranking: 500 Internal Server Error")
	}
	return m.ranking, nil
}
func (m *mockSnStatusAPI) SnEpochSync() (*connect.SnEpochResult, error) {
	if m.epoch == nil {
		return nil, fmt.Errorf("subnet epoch: 500 Internal Server Error")
	}
	return m.epoch, nil
}
func (m *mockSnStatusAPI) SnPoolClaimSync(args *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
	if m.claim == nil {
		return nil, fmt.Errorf("pool claim: 500 Internal Server Error")
	}
	return m.claim, nil
}

func TestFetchSnStatus_MockEndpoints(t *testing.T) {
	expectedColdkey := "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5"
	pubkey, err := ss58.DecodeWithPrefix(expectedColdkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatalf("failed to decode test coldkey: %v", err)
	}

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "jwt"), []byte("test-jwt"), 0600); err != nil {
		t.Fatalf("failed to write mock jwt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "api_url"), []byte("http://unused"), 0600); err != nil {
		t.Fatalf("failed to write mock api_url: %v", err)
	}

	p := Provider{
		Unit:      "urnetwork-test.service",
		User:      "testuser",
		StateDir:  tmpDir,
		NetworkID: "net-uuid-meso",
	}

	// Inject mock API
	mock := &mockSnStatusAPI{
		ranking: &connect.NetworkRankingResult{
			NetworkRanking: &connect.NetworkRanking{
				NetMibCount:       1234567.89,
				LeaderboardRank:   7,
				LeaderboardPublic: true,
			},
		},
		epoch: &connect.SnEpochResult{
			Epoch:               50,
			StartBlock:          50000,
			CommitDeadlineBlock: 50300,
			TrailsDeadlineBlock: 50600,
			FinalizeBlock:       50720,
			ContractAddress:     "0x1111222233334444555566667777888899990000",
			ChainId:             964,
		},
		claim: &connect.SnPoolClaimResult{
			Epoch:      49,
			Coldkey:    pubkey[:],
			ShareBps:   450,
			PayoutRoot: make([]byte, 32),
		},
	}
	origAPI := newSnStatusAPI
	newSnStatusAPI = func(_ context.Context, _ string, _ string) snStatusAPI { return mock }
	t.Cleanup(func() { newSnStatusAPI = origAPI })

	info, err := FetchSnStatus(p)
	if err != nil {
		t.Fatalf("FetchSnStatus failed: %v", err)
	}
	if info.Error != "" {
		t.Fatalf("FetchSnStatus returned partial error: %s", info.Error)
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
		ColdkeySs58:       "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5",
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
	if !strings.Contains(out, "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5") {
		t.Errorf("dashboard missing coldkey")
	}
	if !strings.Contains(out, "8.50%") {
		t.Errorf("dashboard missing formatted payout share percentage")
	}
}

func TestFetchSnStatus_EpochEdgeCasesAndErrors(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "jwt"), []byte("test-jwt"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "api_url"), []byte("http://unused"), 0600)

	p := Provider{
		Unit:     "test.service",
		StateDir: tmpDir,
	}

	// Mock: ranking returns error, epoch returns epoch=1, claim returns share=100
	mock := &mockSnStatusAPI{
		ranking: nil, // simulate error
		epoch: &connect.SnEpochResult{
			Epoch: 1, // current epoch is 1 -> finalized epoch should be 0
		},
		claim: &connect.SnPoolClaimResult{
			Epoch:    0,
			ShareBps: 100,
		},
	}
	origAPI := newSnStatusAPI
	newSnStatusAPI = func(_ context.Context, _ string, _ string) snStatusAPI { return mock }
	t.Cleanup(func() { newSnStatusAPI = origAPI })

	info, err := FetchSnStatus(p)
	if err != nil {
		t.Fatalf("FetchSnStatus failed: %v", err)
	}
	// Partial failure: ranking error, epoch+claim succeed
	if info.Error == "" {
		t.Errorf("expected error to be populated for ranking failure")
	}
	if info.ClaimEpoch != 0 {
		t.Errorf("expected ClaimEpoch 0, got %d", info.ClaimEpoch)
	}
	if info.PayoutShareBps != 100 {
		t.Errorf("expected PayoutShareBps 100, got %d", info.PayoutShareBps)
	}
}

func TestFetchSnStatus_AllEndpointsFail(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "jwt"), []byte("test-jwt"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "api_url"), []byte("http://unused"), 0600)

	p := Provider{Unit: "test.service", StateDir: tmpDir}

	mock := &mockSnStatusAPI{
		ranking: nil, // error
		epoch:   nil, // error
		claim:   nil, // error
	}
	origAPI := newSnStatusAPI
	newSnStatusAPI = func(_ context.Context, _ string, _ string) snStatusAPI { return mock }
	t.Cleanup(func() { newSnStatusAPI = origAPI })

	info, err := FetchSnStatus(p)
	if err == nil {
		t.Fatalf("expected non-nil error when all endpoints fail")
	}
	if info.Error == "" {
		t.Errorf("expected info.Error to be populated")
	}
}

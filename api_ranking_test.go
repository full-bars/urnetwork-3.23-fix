package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNetworkGetRankingSync(t *testing.T) {
	expectedRank := 7
	expectedMib := 1234567.89

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/ranking" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-jwt-token" {
			t.Errorf("unexpected authorization header: %s", authHeader)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		resp := NetworkRankingResult{
			NetworkRanking: &NetworkRanking{
				NetMibCount:       expectedMib,
				LeaderboardRank:   expectedRank,
				LeaderboardPublic: true,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	ctx := context.Background()
	strategy := NewClientStrategyWithDefaults(ctx)
	api := NewBringYourApi(ctx, strategy, ts.URL)
	api.SetByJwt("test-jwt-token")

	result, err := api.NetworkGetRankingSync()
	if err != nil {
		t.Fatalf("NetworkGetRankingSync failed: %v", err)
	}
	if result.NetworkRanking == nil {
		t.Fatalf("expected network ranking in response, got nil")
	}
	if result.NetworkRanking.LeaderboardRank != expectedRank {
		t.Errorf("expected rank %d, got %d", expectedRank, result.NetworkRanking.LeaderboardRank)
	}
	if result.NetworkRanking.NetMibCount != expectedMib {
		t.Errorf("expected mib %f, got %f", expectedMib, result.NetworkRanking.NetMibCount)
	}
	if !result.NetworkRanking.LeaderboardPublic {
		t.Errorf("expected leaderboard public to be true")
	}
}

func TestStatsLeaderboardSync(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/leaderboard" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}

		resp := LeaderboardResult{
			Earners: []*LeaderboardEarner{
				{
					NetworkId:   "net-uuid-1",
					NetworkName: "mesocyclone",
					NetMibCount: 999999.5,
					IsPublic:    true,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	ctx := context.Background()
	strategy := NewClientStrategyWithDefaults(ctx)
	api := NewBringYourApi(ctx, strategy, ts.URL)
	api.SetByJwt("test-jwt")

	result, err := api.StatsLeaderboardSync()
	if err != nil {
		t.Fatalf("StatsLeaderboardSync failed: %v", err)
	}
	if len(result.Earners) != 1 {
		t.Fatalf("expected 1 earner, got %d", len(result.Earners))
	}
	if result.Earners[0].NetworkName != "mesocyclone" {
		t.Errorf("expected earner name 'mesocyclone', got %q", result.Earners[0].NetworkName)
	}
}

package connect

import (
	"fmt"
)

type NetworkRanking struct {
	NetMibCount       float64 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

type NetworkRankingError struct {
	Message string `json:"message"`
}

type NetworkRankingResult struct {
	NetworkRanking *NetworkRanking      `json:"network_ranking,omitempty"`
	Error          *NetworkRankingError `json:"error,omitempty"`
}

type NetworkRankingCallback ApiCallback[*NetworkRankingResult]

// NetworkGetRanking fetches the current network's ranking metrics via
// the authenticated GET /network/ranking route.
func (self *BringYourApi) NetworkGetRanking(callback NetworkRankingCallback) {
	go HandleError(func() {
		HttpGetWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/network/ranking", self.apiUrl),
			self.ByJwt(),
			&NetworkRankingResult{},
			callback,
		)
	})
}

func (self *BringYourApi) NetworkGetRankingSync() (*NetworkRankingResult, error) {
	return HttpGetWithStrategy(
		self.ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/network/ranking", self.apiUrl),
		self.ByJwt(),
		&NetworkRankingResult{},
		NewNoopApiCallback[*NetworkRankingResult](),
	)
}

type GetLeaderboardArgs struct{}

type LeaderboardEarner struct {
	NetworkId         string  `json:"network_id"`
	NetworkName       string  `json:"network_name"`
	NetMibCount       float64 `json:"net_mib_count"`
	IsPublic          bool    `json:"is_public"`
	ContainsProfanity bool    `json:"contains_profanity"`
}

type LeaderboardError struct {
	Message string `json:"message"`
}

type LeaderboardResult struct {
	Earners []*LeaderboardEarner `json:"earners"`
	Error   *LeaderboardError    `json:"error,omitempty"`
}

type LeaderboardCallback ApiCallback[*LeaderboardResult]

// StatsLeaderboard fetches the global top 100 leaderboard via
// the authenticated POST /stats/leaderboard route.
func (self *BringYourApi) StatsLeaderboard(callback LeaderboardCallback) {
	go HandleError(func() {
		HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/stats/leaderboard", self.apiUrl),
			&GetLeaderboardArgs{},
			self.ByJwt(),
			&LeaderboardResult{},
			callback,
		)
	})
}

func (self *BringYourApi) StatsLeaderboardSync() (*LeaderboardResult, error) {
	return HttpPostWithStrategy(
		self.ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/stats/leaderboard", self.apiUrl),
		&GetLeaderboardArgs{},
		self.ByJwt(),
		&LeaderboardResult{},
		NewNoopApiCallback[*LeaderboardResult](),
	)
}

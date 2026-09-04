package urnettools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

// SnStatusInfo captures Subnet 25 and network performance telemetry.
type SnStatusInfo struct {
	ProviderUnit      string  `json:"provider_unit,omitempty"`
	ProviderUser      string  `json:"provider_user,omitempty"`
	StateDir          string  `json:"state_dir"`
	NetworkName       string  `json:"network_name"`
	NetworkID         string  `json:"network_id"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
	NetMibCount       float64 `json:"net_mib_count"`
	ColdkeySs58       string  `json:"coldkey_ss58,omitempty"`
	ColdkeyHex        string  `json:"coldkey_hex,omitempty"`
	Top200Eligible    bool    `json:"top200_eligible"`
	TierDescription   string  `json:"tier_description"`
	CurrentEpoch      uint64  `json:"current_epoch,omitempty"`
	StartBlock        uint64  `json:"start_block,omitempty"`
	FinalizeBlock     uint64  `json:"finalize_block,omitempty"`
	ContractAddress   string  `json:"contract_address,omitempty"`
	ChainID           uint64  `json:"chain_id,omitempty"`
	PayoutShareBps    int     `json:"payout_share_bps,omitempty"`
	ClaimEpoch        uint64  `json:"claim_epoch,omitempty"`
	Error             string  `json:"error,omitempty"`
}

// cmdSnStatus queries and displays Subnet 25 telemetry for a targeted provider.
func cmdSnStatus(args []string) error {
	jsonMode := false
	var filtered []string
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonMode = true
		} else {
			filtered = append(filtered, a)
		}
	}

	t, _, err := parseTargetFlagsLenient(filtered)
	if err != nil {
		return err
	}

	var p Provider
	if t.StateDir != "" {
		if _, err := os.Stat(filepath.Join(t.StateDir, "jwt")); err == nil {
			p = Provider{
				StateDir: t.StateDir,
				Unit:     "state-dir",
			}
		}
	}

	if p.StateDir == "" {
		providers := Discover()
		if len(providers) == 0 {
			docker := DiscoverDocker()
			if len(docker) > 0 {
				return fmt.Errorf("no systemd providers found on this box; running in docker (use 'urnet-docker sn-status')")
			}
			// Fallback check for local home dir
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("no providers discovered and cannot determine home dir: %w", err)
			}
			candidate := filepath.Join(home, ".urnetwork")
			if _, err := os.Stat(filepath.Join(candidate, "jwt")); err == nil {
				p = Provider{
					StateDir: candidate,
					Unit:     "manual",
				}
			} else {
				return fmt.Errorf("no active providers found on this box")
			}
		} else {
			selected, narrowed, err := selectTargetOrSoleAccessible(providers, t, false)
			if err != nil {
				return err
			}
			if narrowed && !jsonMode {
				printNarrowedNote(len(providers), selected, "sn-status")
			}
			p = selected
		}
	}

	info, err := FetchSnStatus(p)
	if err != nil {
		return err
	}

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	renderSnStatusDashboard(info)
	return nil
}

// FetchSnStatus gathers telemetry from the local state and URNetwork API.
func FetchSnStatus(p Provider) (*SnStatusInfo, error) {
	stateDir := p.StateDir
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".urnetwork")
	}

	jwtPath := filepath.Join(stateDir, "jwt")
	jwtBytes, err := os.ReadFile(jwtPath)
	if errors.Is(err, os.ErrNotExist) {
		home, _ := os.UserHomeDir()
		jwtPath = filepath.Join(home, ".urnetwork", "jwt")
		jwtBytes, err = os.ReadFile(jwtPath)
	}
	if err != nil {
		return nil, fmt.Errorf("no authentication JWT found at %s: %w. Run 'urnet-tools auth' first", jwtPath, err)
	}
	byJwt := strings.TrimSpace(string(jwtBytes))

	apiUrl := "https://api.bringyour.com"
	if urlBytes, err := os.ReadFile(filepath.Join(stateDir, "api_url")); err == nil {
		if trimmed := strings.TrimSpace(string(urlBytes)); trimmed != "" {
			apiUrl = trimmed
		}
	}

	info := &SnStatusInfo{
		ProviderUnit: p.Unit,
		ProviderUser: p.User,
		StateDir:     stateDir,
		NetworkName:  p.netLabel(),
		NetworkID:    p.NetworkID,
	}

	// Check local coldkey cache if available
	for _, fn := range []string{"coldkey", ".coldkey"} {
		if ckBytes, err := os.ReadFile(filepath.Join(stateDir, fn)); err == nil {
			trimmed := strings.TrimSpace(string(ckBytes))
			if trimmed != "" {
				info.ColdkeySs58 = trimmed
				if pubkey, err := ss58.DecodeWithPrefix(trimmed, ss58.BittensorPrefix); err == nil {
					info.ColdkeyHex = fmt.Sprintf("0x%x", pubkey)
				}
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	// 1. Fetch Network Ranking
	if rankingRes, err := api.NetworkGetRankingSync(); err == nil && rankingRes != nil && rankingRes.NetworkRanking != nil {
		info.LeaderboardRank = rankingRes.NetworkRanking.LeaderboardRank
		info.NetMibCount = rankingRes.NetworkRanking.NetMibCount
		info.LeaderboardPublic = rankingRes.NetworkRanking.LeaderboardPublic

		rank := info.LeaderboardRank
		if rank > 0 && rank <= 10 {
			info.Top200Eligible = true
			info.TierDescription = fmt.Sprintf("Tier 1 Elite (Rank #%d Globally)", rank)
		} else if rank > 10 && rank <= 50 {
			info.Top200Eligible = true
			info.TierDescription = fmt.Sprintf("Tier 2 High-Volume (Rank #%d Globally)", rank)
		} else if rank > 50 && rank <= 200 {
			info.Top200Eligible = true
			info.TierDescription = fmt.Sprintf("Tier 3 Active (Rank #%d Globally)", rank)
		} else if rank > 200 {
			info.Top200Eligible = false
			info.TierDescription = fmt.Sprintf("Rank #%d (%d below Top 200 cutoff)", rank, rank-200)
		} else {
			info.TierDescription = "Unranked / Processing"
		}
	}

	// 2. Fetch Subnet Epoch
	if epochRes, err := api.SnEpochSync(); err == nil && epochRes != nil {
		info.CurrentEpoch = epochRes.Epoch
		info.StartBlock = epochRes.StartBlock
		info.FinalizeBlock = epochRes.FinalizeBlock
		info.ContractAddress = epochRes.ContractAddress
		info.ChainID = epochRes.ChainId

		// 3. Fetch Payout Claim for latest finalized epoch (e-1)
		targetEpoch := epochRes.Epoch
		if targetEpoch > 1 {
			targetEpoch = epochRes.Epoch - 1
		}
		if claimRes, err := api.SnPoolClaimSync(&connect.SnPoolClaimArgs{Epoch: targetEpoch}); err == nil && claimRes != nil {
			info.ClaimEpoch = claimRes.Epoch
			info.PayoutShareBps = claimRes.ShareBps
			if len(claimRes.Coldkey) == 32 {
				var ck [32]byte
				copy(ck[:], claimRes.Coldkey)
				info.ColdkeyHex = fmt.Sprintf("0x%x", ck)
				if encoded, err := ss58.Encode(ck, ss58.BittensorPrefix); err == nil && info.ColdkeySs58 == "" {
					info.ColdkeySs58 = encoded
				}
			}
		}
	}

	return info, nil
}

func renderSnStatusDashboard(s *SnStatusInfo) {
	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")
	fmt.Println("  \x1b[1;36mURNetwork Subnet 25 — Node & Miner Status\x1b[0m")
	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")

	unit := s.ProviderUnit
	if unit == "" {
		unit = "urnetwork"
	}
	fmt.Printf("  \x1b[1mProvider:\x1b[0m         %s (State: %s)\n", unit, s.StateDir)

	netDisplay := s.NetworkName
	if netDisplay == "" || netDisplay == "-" {
		netDisplay = "default"
	}
	if s.NetworkID != "" {
		netDisplay = fmt.Sprintf("%s (%s)", netDisplay, s.NetworkID)
	}
	fmt.Printf("  \x1b[1mNetwork:\x1b[0m          %s\n", netDisplay)

	// Global Rank & Eligibility
	if s.LeaderboardRank > 0 {
		eligibilityColor := "\x1b[32m" // green
		if !s.Top200Eligible {
			eligibilityColor = "\x1b[33m" // yellow
		}
		pubBadge := ""
		if s.LeaderboardPublic {
			pubBadge = " [Public]"
		}
		fmt.Printf("  \x1b[1mGlobal Rank:\x1b[0m      %s#%d%s\x1b[0m — %s%s\n",
			eligibilityColor, s.LeaderboardRank, pubBadge, s.TierDescription, "\x1b[0m")
	} else {
		fmt.Printf("  \x1b[1mGlobal Rank:\x1b[0m      \x1b[90mUnranked or pending initial telemetry\x1b[0m\n")
	}

	// Bandwidth
	if s.NetMibCount > 0 {
		fmt.Printf("  \x1b[1mNet Bandwidth:\x1b[0m    \x1b[1;32m%.2f MiB\x1b[0m (%.2f GiB) provided\n",
			s.NetMibCount, s.NetMibCount/1024.0)
	} else {
		fmt.Printf("  \x1b[1mNet Bandwidth:\x1b[0m    0.00 MiB\n")
	}

	// Coldkey
	if s.ColdkeySs58 != "" {
		fmt.Printf("  \x1b[1mColdkey (SS58):\x1b[0m   \x1b[33m%s\x1b[0m\n", s.ColdkeySs58)
		if s.ColdkeyHex != "" {
			fmt.Printf("  \x1b[1mColdkey (Hex):\x1b[0m    \x1b[90m%s\x1b[0m\n", s.ColdkeyHex)
		}
	} else if s.ColdkeyHex != "" {
		fmt.Printf("  \x1b[1mColdkey (Hex):\x1b[0m    \x1b[33m%s\x1b[0m\n", s.ColdkeyHex)
	} else {
		fmt.Printf("  \x1b[1mColdkey:\x1b[0m          \x1b[90m(Not configured — register with 'provider wallet set <coldkey>')\x1b[0m\n")
	}

	// Subnet Epoch & Contract
	if s.CurrentEpoch > 0 {
		fmt.Printf("  \x1b[1mSubnet Epoch:\x1b[0m     #%d (Start: #%d, Finalize: #%d)\n",
			s.CurrentEpoch, s.StartBlock, s.FinalizeBlock)
	}
	if s.ContractAddress != "" {
		fmt.Printf("  \x1b[1mContract:\x1b[0m         %s (Chain ID: %d)\n", s.ContractAddress, s.ChainID)
	}
	if s.PayoutShareBps > 0 {
		sharePct := float64(s.PayoutShareBps) / 100.0
		fmt.Printf("  \x1b[1mPayout Share:\x1b[0m     \x1b[32m%.2f%%\x1b[0m (%d bps in Epoch #%d)\n",
			sharePct, s.PayoutShareBps, s.ClaimEpoch)
	}

	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")
}

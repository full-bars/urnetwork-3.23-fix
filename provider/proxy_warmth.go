package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// ProxyWarmthTier represents the authentication warmth state of a proxy.
type ProxyWarmthTier int

const (
	// WarmthCold indicates no stored client JWT or invalid/mismatched identity.
	// Requires fresh minting against the rate-limited auth API.
	WarmthCold ProxyWarmthTier = iota

	// WarmthRenewable indicates a stored client JWT that has expired but retains
	// a valid client_id eligible for 1-step renewal without minting fresh.
	WarmthRenewable

	// WarmthValid indicates a fully valid, unexpired client JWT ready for immediate use.
	WarmthValid
)

func (t ProxyWarmthTier) String() string {
	switch t {
	case WarmthValid:
		return "valid"
	case WarmthRenewable:
		return "renewable"
	default:
		return "cold"
	}
}

const (
	// WarmValidStagger is the accelerated launch gap for proxies with valid, unexpired client JWTs.
	// Because these proxies bypass the auth API (/mint) and dial WebRTC directly, they ramp up
	// at 25ms intervals rather than 150ms (e.g. 100 hot proxies launch within ~2.5s).
	WarmValidStagger = 25 * time.Millisecond

	// WarmRenewableStagger is the launch gap for proxies whose stored client JWT expired but retain
	// their valid client_id for 1-step renewal.
	WarmRenewableStagger = 50 * time.Millisecond

	// ColdFileStagger is the standard launch gap for fresh file-sourced proxies.
	ColdFileStagger = 150 * time.Millisecond

	// ColdURLStagger is the conservative launch gap for fresh URL-sourced proxies.
	ColdURLStagger = 500 * time.Millisecond
)

// currentProviderNetworkID extracts the network_id claim from the active ~/.urnetwork/jwt file,
// falling back to any stored network_id in the client JWT store if missing.
func currentProviderNetworkID() string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if jwtBytes, err := os.ReadFile(filepath.Join(home, ".urnetwork", "jwt")); err == nil {
			if nid, ok := jwtNetworkId(strings.TrimSpace(string(jwtBytes))); ok && nid != "" {
				return nid
			}
		}
	}
	if globalClientJWTStore != nil {
		return globalClientJWTStore.AnyNetworkID()
	}
	return ""
}

// proxyLaunchStagger returns the per-proxy stagger duration based on warmth and source.
func proxyLaunchStagger(tier ProxyWarmthTier, isURLSourced bool) time.Duration {
	switch tier {
	case WarmthValid:
		return WarmValidStagger
	case WarmthRenewable:
		return WarmRenewableStagger
	default:
		if isURLSourced {
			return ColdURLStagger
		}
		return ColdFileStagger
	}
}

// evaluateProxyWarmth checks whether a proxy address has a warm client JWT stored on disk.
func evaluateProxyWarmth(address string, currentNetworkID string) ProxyWarmthTier {
	if !hotRestartEnabled() {
		return WarmthCold
	}
	if globalClientJWTStore == nil {
		return WarmthCold
	}
	if currentNetworkID == "" {
		currentNetworkID = currentProviderNetworkID()
	}
	entry, ok := globalClientJWTStore.Get(address)
	if !ok || entry.ByClientJWT == "" || entry.ClientID == "" {
		return WarmthCold
	}
	// If currentNetworkID is missing, try to fill it in from the stored entry.
	if currentNetworkID == "" && entry.NetworkID != "" {
		currentNetworkID = entry.NetworkID
	}
	// If neither side has a network_id, provideAuth cannot verify identity reuse and must mint fresh.
	if currentNetworkID == "" && entry.NetworkID == "" {
		return WarmthCold
	}
	// Reject known network mismatches.
	if entry.NetworkID != "" && currentNetworkID != "" && entry.NetworkID != currentNetworkID {
		return WarmthCold
	}
	if err := validateJWTExpiry(entry.ByClientJWT); err == nil && jwtContainsClientId(entry.ByClientJWT) {
		return WarmthValid
	}
	// Salvage priority: if client_id is a valid identity, it can be renewed to salvage
	// reputation even if the expired token payload lacked the client_id claim.
	if _, err := connect.ParseId(entry.ClientID); err == nil {
		return WarmthRenewable
	}
	return WarmthCold
}

// ProxySchedule holds the launch configuration and cumulative delay for a proxy.
type ProxySchedule struct {
	Settings *connect.ProxySettings
	Tier     ProxyWarmthTier
	Delay    time.Duration
	Stagger  time.Duration
}

// prioritizeAndScheduleProxies sorts proxies by provenance and warmth, then computes cumulative launch delays.
// Note: the proxies slice is reordered in place.
func prioritizeAndScheduleProxies(
	proxies []*connect.ProxySettings,
	proxySourceOf map[string]string,
	currentNetworkID string,
) ([]ProxySchedule, int, int, int) {
	if currentNetworkID == "" {
		currentNetworkID = currentProviderNetworkID()
	}
	warmthMap := make(map[string]ProxyWarmthTier, len(proxies))
	var warmCount, renewableCount, coldCount int

	for _, s := range proxies {
		tier := evaluateProxyWarmth(s.Address, currentNetworkID)
		warmthMap[s.Address] = tier
		switch tier {
		case WarmthValid:
			warmCount++
		case WarmthRenewable:
			renewableCount++
		default:
			coldCount++
		}
	}

	sort.SliceStable(proxies, func(i, j int) bool {
		addrI := proxies[i].Address
		addrJ := proxies[j].Address
		srcI := proxySourceOf[addrI]
		srcJ := proxySourceOf[addrJ]

		// 1. Primary rule: File-sourced (or internal) proxies before URL-sourced proxies
		if (srcI != "url") != (srcJ != "url") {
			return srcI != "url"
		}

		// 2. Secondary rule: Higher warmth tier dials first
		tierI := warmthMap[addrI]
		tierJ := warmthMap[addrJ]
		if tierI != tierJ {
			return tierI > tierJ
		}

		// 3. Stable fallback preserves original order
		return false
	})

	schedules := make([]ProxySchedule, len(proxies))
	var cumulativeDelay time.Duration

	for i, s := range proxies {
		tier := warmthMap[s.Address]
		isURL := proxySourceOf[s.Address] == "url"
		stagger := proxyLaunchStagger(tier, isURL)

		schedules[i] = ProxySchedule{
			Settings: s,
			Tier:     tier,
			Delay:    cumulativeDelay,
			Stagger:  stagger,
		}
		cumulativeDelay += stagger
	}

	return schedules, warmCount, renewableCount, coldCount
}

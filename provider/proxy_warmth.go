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

// prioritizeAndScheduleProxies sorts proxies by warmth, then provenance, then earnings,
// then computes cumulative launch delays.
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
	// Earnings are read once per proxy rather than inside the comparator:
	// the store is mutex-guarded and a sort does O(n log n) comparisons, so
	// reading per comparison would take the lock tens of thousands of times
	// on a node carrying thousands of proxies.
	earningsMap := make(map[string]float64, len(proxies))
	now := time.Now()
	var warmCount, renewableCount, coldCount int

	for _, s := range proxies {
		// warmthMap/earningsMap are keyed by identity (ProxySettings.Key()),
		// not bare address, so two accounts sharing a gateway address get
		// independent tier/earnings records instead of colliding in these
		// maps. evaluateProxyWarmth itself still probes the client-JWT
		// cache by bare address (a known, low-stakes approximation: two
		// identities at the same address currently share one warm-restart
		// JWT slot, so at most one of them benefits from hot-restart at a
		// time — a startup latency cost, not a correctness or money issue).
		// proxyEarningsScore is looked up by the real key: the earnings
		// store itself is already identity-keyed (see proxy_earnings_store.go).
		key := s.Key()
		tier := evaluateProxyWarmth(s.Address, currentNetworkID)
		warmthMap[key] = tier
		earningsMap[key] = proxyEarningsScore(key, now)
		switch tier {
		case WarmthValid:
			warmCount++
		case WarmthRenewable:
			renewableCount++
		default:
			coldCount++
		}
	}

	// trusted reports whether an address launches with the file list rather
	// than behind it. File and internal proxies are trusted by provenance.
	// A URL-sourced address is trusted once it has actually moved real
	// billable traffic: at that point it is no longer an unproven address
	// off a public list, it is a known earner, and making it wait behind
	// every file proxy costs real throughput during the warmup window.
	trusted := func(key string) bool {
		return proxySourceOf[key] != "url" || earningsMap[key] >= earningsPromotionBytes
	}

	sort.SliceStable(proxies, func(i, j int) bool {
		addrI := proxies[i].Key()
		addrJ := proxies[j].Key()

		// 1. Primary rule: higher warmth tier dials first. Warmth is the
		//    primary rule because a cold identity must mint against the
		//    rate-limited auth API. Promoting a rich cold proxy ahead of a
		//    warm proxy would spend a scarce mint slot and stall an identity
		//    that could have dialled straight through.
		tierI := warmthMap[addrI]
		tierJ := warmthMap[addrJ]
		if tierI != tierJ {
			return tierI > tierJ
		}

		// 2. Secondary rule: trusted provenance dials first. File-sourced
		//    and internal proxies always qualify; a URL-sourced proven
		//    earner is promoted into the same group.
		trustI := trusted(addrI)
		trustJ := trusted(addrJ)
		if trustI != trustJ {
			return trustI
		}

		// 3. Tertiary rule: within one tier and group, the bigger earner
		//    dials first. This is where the ranking does most of its work,
		//    since the warm tier is where launches are cheap.
		earnI := earningsMap[addrI]
		earnJ := earningsMap[addrJ]
		if earnI != earnJ {
			return earnI > earnJ
		}

		// 4. Stable fallback preserves original order
		return false
	})

	// Exploration quota: interleave 1 unproven proxy for every 5 top-earners
	// among cold proxies to prevent permanent starvation. Without this, a
	// node that restarts frequently starves unproven proxies behind the
	// cumulative cold-proxy ramp delay and they never accumulate traffic.
	//
	// "unproven" means untrusted provenance (URL-sourced, below promotion
	// threshold). The interleave picks from the tail of the sorted list
	// (cold unproven proxies) and inserts one after every 5 trusted cold
	// proxies, preserving the relative order within each group.
	trustedCold := make([]*connect.ProxySettings, 0, len(proxies))
	unprovenCold := make([]*connect.ProxySettings, 0, len(proxies))
	for _, s := range proxies {
		if warmthMap[s.Key()] == WarmthCold {
			if trusted(s.Key()) {
				trustedCold = append(trustedCold, s)
			} else {
				unprovenCold = append(unprovenCold, s)
			}
		}
	}
	if len(unprovenCold) > 0 && len(trustedCold) > 0 {
		reordered := make([]*connect.ProxySettings, 0, len(proxies))
		// Append warm + renewable proxies first (they always go first).
		for _, s := range proxies {
			if warmthMap[s.Key()] != WarmthCold {
				reordered = append(reordered, s)
			}
		}
		// Interleave trusted cold with unproven cold.
		unprovenIdx := 0
		for i, s := range trustedCold {
			reordered = append(reordered, s)
			if (i+1)%5 == 0 && unprovenIdx < len(unprovenCold) {
				reordered = append(reordered, unprovenCold[unprovenIdx])
				unprovenIdx++
			}
		}
		for ; unprovenIdx < len(unprovenCold); unprovenIdx++ {
			reordered = append(reordered, unprovenCold[unprovenIdx])
		}
		copy(proxies, reordered)
	}

	schedules := make([]ProxySchedule, len(proxies))
	var cumulativeDelay time.Duration

	for i, s := range proxies {
		key := s.Key()
		tier := warmthMap[key]
		isURL := proxySourceOf[key] == "url"
		promoted := isURL && earningsMap[key] >= earningsPromotionBytes
		// Promoted URL proxies belong with the cold file group; they have
		// earned trusted provenance and should not be penalised with the
		// conservative ColdURLStagger.
		stagger := proxyLaunchStagger(tier, isURL && !promoted)

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

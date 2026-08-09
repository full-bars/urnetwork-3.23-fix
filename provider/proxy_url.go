package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// ProxyURLState is the on-disk record of configured live proxy URL sources
// and the addresses fetched from them so far. Unlike proxy.state, this file
// is additive-only by design: fetched addresses are only ever removed by
// removeDeadProxies/evictProxyURLAddress (manual or automatic cleanup),
// never by a fetch cycle.
type ProxyURLState struct {
	Sources []string                 `json:"sources"`
	Cache   map[string]ProxyURLEntry `json:"cache"`

	// Blacklist records addresses that were permanently evicted (see
	// evictProxyURLAddress) and the time they were blacklisted. Permanent,
	// no expiry: mergeProxyURLEntries skips any address found here, so a
	// fetch (which is otherwise add-only) can never silently bring a
	// blacklisted address back, even across process restarts.
	Blacklist map[string]time.Time `json:"blacklist,omitempty"`

	// ExcludePatterns holds case-insensitive host substrings set by
	// `proxy remove --match`. Any fetched proxy whose host matches one of
	// these is skipped at merge time, so a URL source refresh can never
	// re-add proxies the operator removed by pattern. Managed by
	// `proxy remove --match` / `proxy unexclude`.
	ExcludePatterns []string `json:"exclude_patterns,omitempty"`

	// DegradedCleanupThreshold sets how long a URL-sourced proxy can be
	// degraded before the automatic cleanup cycle evicts it. A zero value
	// (default) disables degraded auto-cleanup — only dead/inactive proxies
	// are removed, matching the pre-v24.18 behavior.
	// Set at runtime via:  urnetwork proxy set degraded-cleanup 24h
	// Read every cleanup cycle, so changes take effect without restart.
	DegradedCleanupThreshold string `json:"degraded_cleanup_threshold,omitempty"`

	// TargetPoolSize is the AIMD-discovered operating point for URL-sourced
	// proxies — grown while pressure is low, cut multiplicatively under
	// sustained pressure. 0 = controller hasn't run. proxy_url_max remains a
	// hard ceiling on top of this.
	TargetPoolSize int `json:"target_pool_size,omitempty"`
}

// ProxyURLEntry records the auth (if any) for one address fetched from a URL
// source. Most public proxy lists provide unauthenticated entries.
type ProxyURLEntry struct {
	User       string    `json:"user,omitempty"`
	Password   string    `json:"password,omitempty"`
	ProbeOK    bool      `json:"probe_ok"`              // passed API reachability probe
	ProbeFails int       `json:"probe_fails,omitempty"` // consecutive API probe failures
	LastProbe  time.Time `json:"last_probe,omitempty"`  // last API probe time

	// Score is the stage-1 table probe result (ok/total) from the last
	// graded pass, 0 when the entry has never been table-probed. Matches
	// the backend data model so fleet grading can consume it directly.
	Score float64 `json:"score,omitempty"`
	// Graded is true once a stage-1 table probe has run and recorded its
	// result. It is distinct from Score: a proxy that scored 0.0 was graded
	// (and failed), while Score==0 with Graded=false means "never probed".
	// The auth-time admission gate only enforces the bar for graded entries,
	// so a honeypot that answers the API CONNECT but nothing else is
	// blocked even though its score field is numerically zero.
	Graded bool `json:"graded,omitempty"`
	// Failed lists the target hostnames that did not answer the last
	// stage-1 pass, for diagnostics and fleet reporting.
	Failed []string `json:"failed,omitempty"`
}

func proxyURLStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_url.json"), nil
}

func readProxyURLState() (*ProxyURLState, error) {
	path, err := proxyURLStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyURLStateFrom(path)
}

func readProxyURLStateFrom(path string) (*ProxyURLState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyURLState{Cache: map[string]ProxyURLEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy_url.json: %w", err)
	}
	var s ProxyURLState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse proxy_url.json: %w", err)
	}
	if s.Cache == nil {
		s.Cache = map[string]ProxyURLEntry{}
	}
	return &s, nil
}

func writeProxyURLState(s *ProxyURLState) error {
	path, err := proxyURLStatePath()
	if err != nil {
		return err
	}
	return writeProxyURLStateTo(path, s)
}

func writeProxyURLStateTo(path string, s *ProxyURLState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// parseProxyURLLine parses one line from a remote proxy list. Unlike
// parseProxyAddress (used by --proxy_file, which requires credentials),
// entries without credentials are valid here — open/anonymous proxies are
// the common case for public proxy lists. Accepted forms:
//
//	host:port
//	host:port:user:pass
//	socks5://host:port
//	socks5://user:pass@host:port
//
// Returns ok=false if the line is blank, a comment, or uses an unsupported
// protocol scheme (this fork is SOCKS5-only).
func parseProxyURLLine(line string) (address, user, password string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", "", false
	}

	if idx := strings.Index(line, "://"); idx != -1 {
		scheme := line[:idx]
		if !strings.EqualFold(scheme, "socks5") {
			tlog("[proxy][url] unsupported scheme %q (only socks5 is supported); skipping %q\n", scheme, line)
			return "", "", "", false
		}
		rest := line[idx+3:]
		if at := strings.LastIndex(rest, "@"); at != -1 {
			cred := rest[:at]
			address = rest[at+1:]
			if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
				user, password = parts[0], parts[1]
			}
			return address, user, password, address != ""
		}
		address, user, password = parseProxyAddress(rest)
		return address, user, password, address != ""
	}

	address, user, password = parseProxyAddress(line)
	return address, user, password, address != ""
}

// maxProxyURLFetchBytes caps how much of a proxy list response we read,
// defending against a misbehaving or malicious endpoint returning an
// unbounded body.
const maxProxyURLFetchBytes = 10 * 1024 * 1024 // 10 MiB

// proxyURLHTTPClient is a custom HTTP client with redirect limits and
// timeouts, instead of the global http.DefaultClient which has no guards.
var proxyURLHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("stopped after 3 redirects")
		}
		return nil
	},
	Transport: &http.Transport{
		MaxIdleConns:       1,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	},
}

// fetchProxyURLLines fetches a proxy list from a URL and splits it into
// lines. It does not parse the lines — callers parse each line with
// parseProxyURLLine. Returns an error on network failure, non-200 status, or
// an empty body; never blocks longer than 30s.
func fetchProxyURLLines(ctx context.Context, url string) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := proxyURLHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyURLFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty response body")
	}

	lines := strings.Split(string(b), "\n")
	// Filter out empty lines
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// mergeProxyURLEntries parses each line and adds genuinely new addresses to
// state.Cache (mutating it in place). Already-cached addresses are left
// untouched — this function only ever adds, never updates or removes.
// Addresses present in state.Blacklist are always skipped, even if not yet
// cached, so a permanently-evicted address can never come back. maxTotal
// caps the total cache size; 0 means unlimited.
//
// mergeProxyURLEntries adds new entries from lines into state.Cache. lines is
// expected to have any api-verified entries first, followed by socks5-only
// entries (the order fetchAndMergeProxyURLs builds them in); apiOKCount is
// how many of the leading entries passed the API-reachability probe and
// should be cached with ProbeOK=true. Entries beyond apiOKCount (socks5-only,
// or callers that don't track probe results) get ProbeOK=false so the
// background reaper picks them up for retry.
//
// rankAddr (may be nil) reports the priority of a candidate address so the
// cache keeps the BEST entries across all sources rather than the first
// ones to arrive: when the cache is at maxTotal, a new line is added only
// if its rank exceeds the lowest-ranked cached entry, which is then evicted
// to make room. A nil rankAddr preserves the old behavior (at cap, remaining
// lines are skipped without evicting anything). Evict-one-add-one also keeps
// the TOTAL cache size bounded at maxTotal, which subsumes the 342-branch
// squatter-exclusion and hard-cap backstop (graded-below-bar squatters are
// the lowest-ranked entries and are the first eviction targets).
//
// gradeFor (may be nil) supplies the stage-1 grade for a newly added
// address so it is persisted with Score/Graded at insert time — otherwise a
// just-added entry would rank as ungraded (-1) and be the first eviction
// target of the very next line. When gradeFor returns a decidable grade it
// also decides ProbeOK (address-keyed), so skipped cached, duplicate,
// blacklisted, excluded, or invalid lines can never shift qualification
// status onto another address (342 review round 2); without a grade, the
// line-index / apiOKCount boundary is the fallback.
func mergeProxyURLEntries(state *ProxyURLState, lines []string, apiOKCount int, maxTotal int, rankAddr func(addr string) int, gradeFor func(addr string) (proxyURLGrade, bool)) (added int) {
	if state.Cache == nil {
		state.Cache = map[string]ProxyURLEntry{}
	}
	evictions := 0
	for i, line := range lines {
		address, user, password, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		if _, exists := state.Cache[address]; exists {
			continue
		}
		if _, blacklisted := state.Blacklist[address]; blacklisted {
			continue
		}
		if hostMatchesAny(state.ExcludePatterns, address) {
			continue
		}
		if maxTotal > 0 && len(state.Cache) >= maxTotal {
			// Cache full. With a rank function, evict the lowest-ranked
			// cached entry if this candidate is better — best-overall
			// selection across all sources, not first-come-first-served.
			// Evict-one-add-one also keeps the TOTAL cache size bounded at
			// maxTotal, and graded-below-bar squatters (rank 0 / ungraded
			// -1) are the first eviction targets.
			if rankAddr == nil {
				break
			}
			candidateRank := rankAddr(address)
			evictAddr, evictRank := lowestRankedCacheEntry(state)
			if evictAddr == "" || candidateRank <= evictRank {
				// No evictable entry, or this candidate is not better than
				// the worst one already cached. The production caller feeds
				// candidates RANK-SORTED (collectRankedCandidates, best
				// first), so once a candidate fails to outrank the lowest
				// cached entry, every later candidate is at most its rank
				// and cannot evict either — stop scanning instead of
				// re-scanning the whole cache per line (coderabbit review).
				break
			}
			delete(state.Cache, evictAddr)
			evictions++
		}
		entry := ProxyURLEntry{User: user, Password: password, ProbeOK: i < apiOKCount}
		if gradeFor != nil {
			if g, ok := gradeFor(address); ok {
				// Address-keyed ProbeOK: the grade's Qualified flag decides
				// it, independent of Decidable — a kill-switch-disabled
				// admission carries Qualified=true with Decidable=false
				// (stage 1 never ran), and must still be ProbeOK=true
				// (self-review finding). The Decidable && !Socks5Only gate
				// applies only to the persisted Score/Graded/Failed.
				entry.ProbeOK = g.Qualified
				applyProxyGradeToEntry(&entry, g)
			}
		}
		state.Cache[address] = entry
		added++
	}
	// One aggregate line per merge call (per fetch cycle), not one per
	// evicted address — the important buffer must not become a per-proxy
	// stream on a large cache (coderabbit review).
	if evictions > 0 {
		importantLogf("[proxy][url] cap eviction: %d entries evicted for higher-ranked candidates this cycle\n", evictions)
	}
	return added
}

// applyProxyGradeToEntry persists a stage-1 grade onto a cache entry: the
// Score/Graded/Failed fields, gated on a genuine decidable verdict that is
// not socks5-only (C1/C2: an empty or cancelled pass leaves the prior grade
// intact). Shared by the merge insert path and the fetch cache-update loop so
// the persist rule cannot drift (coderabbit review).
func applyProxyGradeToEntry(entry *ProxyURLEntry, g proxyURLGrade) {
	if g.Decidable && !g.Socks5Only {
		entry.Score = g.Score
		entry.Graded = true
		entry.Failed = capFailedList(g.Failed)
	}
}

// rankCacheEntry computes the tier priority of an already-cached entry from
// its PERSISTED grade. Ungraded entries (never graded, or graded in a cycle
// whose write was skipped) rank below every graded one, so cap eviction
// prefers dropping them first. This is distinct from the candidate-rank
// closure the caller supplies: a cached entry from a previous cycle is not
// in this cycle's grades map, so ranking it through the closure would
// collapse every old entry to -1 and evict good proxies ahead of worse ones.
func rankCacheEntry(e ProxyURLEntry) int {
	if e.Graded {
		return proxyTierRank(proxyGradeTier(e.Score))
	}
	return -1
}

// lowestRankedCacheEntry returns the address of the lowest-ranked cached
// entry and its rank, using rankCacheEntry on each persisted entry. Ties
// are broken arbitrarily (Go map order is randomized); equal ranks are
// interchangeable.
func lowestRankedCacheEntry(state *ProxyURLState) (string, int) {
	lowestAddr := ""
	lowestRank := int(^uint(0) >> 1) // max int
	for addr, e := range state.Cache {
		r := rankCacheEntry(e)
		if r < lowestRank {
			lowestRank = r
			lowestAddr = addr
		}
	}
	return lowestAddr, lowestRank
}

// mergeProxyURLCache adds entries from urlState.Cache into desiredSet for any
// address not already present, and records "url" provenance for those newly
// added addresses in sourceOf. An address already in desiredSet (from the
// primary --proxy_file / internal-config source) always wins — its entry and
// its sourceOf tag are left untouched. urlState may be nil (e.g. read error
// upstream), in which case this is a no-op.
func mergeProxyURLCache(desiredSet map[string]*connect.ProxySettings, sourceOf map[string]string, urlState *ProxyURLState) {
	if urlState == nil {
		return
	}
	// The stage-1 admission bar, resolved once: graded-below-bar entries
	// must not enter the desired set at all. Spawning them would hold a
	// goroutine and a context for a proxy the auth gate will never admit,
	// and the reaper would keep marking them ProbeOK=true while the gate
	// blocks them (finding H4). They stay in the cache (the fetch cycle
	// re-grades them next time); they just never launch.
	//
	// The filter is itself gated on cfg.Enabled: flipping the kill switch
	// off must bring previously-graded below-bar proxies BACK into the
	// desired set — that is exactly the mis-grading scenario the operator
	// is escaping from (finding NEW-2).
	cfg := resolveProxyTableProbeConfig()
	for addr, entry := range urlState.Cache {
		if _, exists := desiredSet[addr]; exists {
			continue
		}
		if cfg.Enabled && entry.Graded && entry.Score < cfg.PassBar {
			continue
		}
		settings := &connect.ProxySettings{Network: "tcp", Address: addr}
		if entry.User != "" || entry.Password != "" {
			settings.Auth = &proxy.Auth{User: entry.User, Password: entry.Password}
		}
		desiredSet[addr] = settings
		sourceOf[addr] = "url"
	}
}

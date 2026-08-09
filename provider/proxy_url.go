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
// caps the total cache size; 0 means unlimited. Once the cap is reached,
// remaining lines in this call are skipped without evicting any existing
// entry.
// mergeProxyURLEntries adds new entries from lines into state.Cache. lines is
// expected to have any api-verified entries first, followed by socks5-only
// entries (the order fetchAndMergeProxyURLs builds them in); apiOKCount is
// how many of the leading entries passed the API-reachability probe and
// should be cached with ProbeOK=true. Entries beyond apiOKCount (socks5-only,
// or callers that don't track probe results) get ProbeOK=false so the
// background reaper picks them up for retry.
// cacheSizeExcludingBelowBar returns the number of cache entries that count
// toward the maxTotal cap: every entry except graded-below-bar squatters.
// A below-bar entry is filtered out of the desired set by mergeProxyURLCache,
// so it can never launch — but if it counted toward the cap it would still
// block new candidates after the source dropped it (merging is add-only).
// Excluding it from the count lets fresh candidates in while the entry waits
// for a re-grade or the reaper (review #12; the tiers branch supersedes this
// with best-overall eviction). When the kill switch is off, below-bar
// entries are re-admitted and count normally.
func cacheSizeExcludingBelowBar(cache map[string]ProxyURLEntry, cfg proxyTableProbeConfig) int {
	n := 0
	for _, e := range cache {
		if cfg.Enabled && e.Graded && e.Score < cfg.PassBar {
			continue
		}
		n++
	}
	return n
}

// proxyURLCacheHardCapFactor bounds the TOTAL cache size (below-bar
// squatters included) at maxTotal * factor. cacheSizeExcludingBelowBar lets
// fresh candidates in past graded-below-bar squatters, but nothing ever
// removes a squatter that still passes stage 0 (the reaper resets its
// ProbeFails, it is not re-graded once the source drops it, and it never
// launches) — without a hard backstop proxy_url.json could grow without
// limit (Opus review finding 3). At the hard cap the merge evicts the
// OLDEST squatter to make room rather than dropping the candidate (review
// round 2).
const proxyURLCacheHardCapFactor = 2

// evictOldestBelowBarSquatter removes the graded-below-bar cache entry with
// the oldest LastProbe, if any, so an admissible candidate can still be
// admitted when the hard cap is reached. Squatters (Graded && Score <
// PassBar while the kill switch is on) never launch, so evicting them is
// strictly better than dropping the new candidate. Returns false when there
// is nothing to evict (cache at/over cap with no squatters — the merge then
// stops).
func evictOldestBelowBarSquatter(state *ProxyURLState, cfg proxyTableProbeConfig) bool {
	var oldestAddr string
	var oldest time.Time
	for addr, e := range state.Cache {
		if !cfg.Enabled || !e.Graded || e.Score >= cfg.PassBar {
			continue
		}
		if oldestAddr == "" || e.LastProbe.Before(oldest) {
			oldestAddr = addr
			oldest = e.LastProbe
		}
	}
	if oldestAddr == "" {
		return false
	}
	oldestEntry := state.Cache[oldestAddr]
	delete(state.Cache, oldestAddr)
	tlog("[proxy][url] hard cap eviction: below-bar squatter %s (score %.2f) evicted for a new candidate\n",
		oldestAddr, oldestEntry.Score)
	return true
}

// mergeProxyURLEntries adds new entries from lines into state.Cache. lines is
// expected to have any api-verified entries first, followed by socks5-only
// entries (the order fetchAndMergeProxyURLs builds them in); apiOKCount is
// how many of the leading entries passed the API-reachability probe and
// should be cached with ProbeOK=true. Entries beyond apiOKCount (socks5-only,
// or callers that don't track probe results) get ProbeOK=false so the
// background reaper picks them up for retry.
//
// qualified (may be nil) is an address-keyed set of stage-1-qualified
// addresses. When non-nil it decides ProbeOK at insert time instead of the
// raw line index / apiOKCount boundary, so skipped cached, duplicate,
// blacklisted, excluded, or invalid lines can never shift qualification
// status onto another address (review round 2).
func mergeProxyURLEntries(state *ProxyURLState, lines []string, apiOKCount int, maxTotal int, qualified map[string]bool) (added int) {
	if state.Cache == nil {
		state.Cache = map[string]ProxyURLEntry{}
	}
	// The cap counts non-squatter entries (see cacheSizeExcludingBelowBar).
	// Computed once: inserted entries are ungraded at insert time (grades are
	// persisted after the merge), so the count only grows by one per add.
	cfg := resolveProxyTableProbeConfig()
	capCount := len(state.Cache)
	hardCap := 0
	if maxTotal > 0 {
		capCount = cacheSizeExcludingBelowBar(state.Cache, cfg)
		if capCount > maxTotal {
			tlog("[proxy][url] cache over cap: %d non-squatter entries, maxTotal=%d (new entries will not be added)\n",
				capCount, maxTotal)
		}
		hardCap = maxTotal * proxyURLCacheHardCapFactor
	}
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
		// Soft cap first: if the non-squatter count is already at maxTotal,
		// stop without evicting (a squatter eviction would be wasted).
		if maxTotal > 0 && capCount >= maxTotal {
			break
		}
		// Hard backstop: the file itself must stay bounded even with
		// squatters excluded from the count. When a squatter exists, evict
		// the oldest one to make room for this candidate; only stop when
		// there is nothing left to evict.
		if hardCap > 0 && len(state.Cache) >= hardCap {
			if !evictOldestBelowBarSquatter(state, cfg) {
				break
			}
		}
		probeOK := i < apiOKCount
		if qualified != nil {
			probeOK = qualified[address]
		}
		state.Cache[address] = ProxyURLEntry{User: user, Password: password, ProbeOK: probeOK}
		capCount++
		added++
	}
	return added
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

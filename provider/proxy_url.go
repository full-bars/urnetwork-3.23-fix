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
}

// ProxyURLEntry records the auth (if any) for one address fetched from a URL
// source. Most public proxy lists provide unauthenticated entries.
type ProxyURLEntry struct {
	User       string    `json:"user,omitempty"`
	Password   string    `json:"password,omitempty"`
	ProbeOK    bool      `json:"probe_ok"`              // passed API reachability probe
	ProbeFails int       `json:"probe_fails,omitempty"` // consecutive API probe failures
	LastProbe  time.Time `json:"last_probe,omitempty"`  // last API probe time
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
		MaxIdleConns:        1,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
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
func mergeProxyURLEntries(state *ProxyURLState, lines []string, maxTotal int) (added int) {
	if state.Cache == nil {
		state.Cache = map[string]ProxyURLEntry{}
	}
	for _, line := range lines {
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
			break
		}
		state.Cache[address] = ProxyURLEntry{User: user, Password: password}
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
	for addr, entry := range urlState.Cache {
		if _, exists := desiredSet[addr]; exists {
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

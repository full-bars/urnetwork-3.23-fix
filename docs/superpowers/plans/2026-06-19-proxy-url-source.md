# Proxy URL Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the provider fetch a live proxy list URL on an interval, merge new (deduplicated) entries into the running proxy set without disturbing existing proxies, and optionally clean up dead URL-sourced entries on a separate, slower cadence — additive on top of the existing `--proxy_file` / internal-config workflows, never replacing them.

**Architecture:** A new persisted file `~/.urnetwork/proxy_url.json` holds configured source URLs and a cache of fetched addresses. Two new background tickers (modeled on the existing `runJWTRefresher` pattern in `provider/main.go:1162`) fetch sources and run scoped dead-proxy cleanup. The existing `ProxyReloader.reload()` (`provider/proxy_reload.go:155`) and the `provide()` startup path (`provider/main.go:1201`) both merge this cache into their desired proxy set via one new shared, pure function — the reloader's existing diff/cancel/drain logic is untouched.

**Tech Stack:** Go 1.25, standard library only (`net/http`, `encoding/json`, `time`) — no new dependencies.

## Global Constraints

- v1 supports plain-text proxy lists only (one `ip:port` / `ip:port:user:pass` / `socks5://[user:pass@]ip:port` entry per line). CSV/JSON are out of scope.
- URL-sourced proxies never require credentials — unlike `--proxy_file`'s `readProxySettingsFromFile`, which rejects entries missing a user/password. Do not change that existing function's behavior.
- The URL fetch cycle is **add-only**: it never removes a proxy. Removal only happens via the existing manual `proxy remove-dead` or the new automatic cleanup job, both scoped by source.
- Automatic daily cleanup defaults to **disabled** (`scope=none`). It must never touch a proxy whose source is `"file"` or `"internal"` unless scope is explicitly `"all"`.
- Every new background loop must select on `ctx.Done()` so it exits cleanly with the rest of the provider — follow the exact pattern in `runJWTRefresher` (`provider/main.go:1162-1199`).
- All new persisted JSON files use the same atomic temp-file-then-rename write pattern as `writeProxyStateTo` (`provider/proxy_state.go:76-99`).
- Reference design: `docs/design/proxy-url-source-design.md`.

---

## File Structure

| File | Responsibility |
|---|---|
| `provider/proxy_state.go` (modify) | Add `Source` field to `ProxyEntry`; add `tagProxySourceIfUnset`. |
| `provider/proxy_url.go` (new) | `ProxyURLState`/`ProxyURLEntry` types, disk read/write, `parseProxyURLLine`, `fetchProxyURLLines`, `mergeProxyURLEntries`, `mergeProxyURLCache`. |
| `provider/proxy_url_source.go` (new) | `fetchAndMergeProxyURLs`, `runProxyURLFetcher`, `removeDeadProxies`, `runProxyURLCleanupOnce`, `runProxyURLCleanup`. |
| `provider/proxy_reload.go` (modify) | `reload()` merges the URL cache into its desired set via `mergeProxyURLCache`. |
| `provider/main.go` (modify) | CLI usage/dispatch for `proxy add-source`/`proxy remove-source` and new `provide` flags; `provide()` wiring: resolve config, merge URL cache into the startup set, start the two new goroutines; `proxyRemoveDead` refactored to bucket removals by source. |
| `provider/proxy_url_test.go` (new) | Tests for parsing, fetch, and cache merge. |
| `provider/proxy_url_source_test.go` (new) | Tests for fetch-and-merge, cleanup bucketing/scope. |
| `provider/main_test.go` (modify) | Tests for `tagProxySourceIfUnset`. |

---

### Task 1: Tag proxy provenance in `proxy.state`

**Files:**
- Modify: `provider/proxy_state.go:28-32` (`ProxyEntry` struct)
- Modify: `provider/main_test.go` (add test)

**Interfaces:**
- Produces: `ProxyEntry.Source string` field; `tagProxySourceIfUnset(state *ProxyState, address, source string)`.

- [ ] **Step 1: Write the failing test**

Add to `provider/main_test.go`:

```go
func TestTagProxySourceIfUnset_SetsOnFirstCall(t *testing.T) {
	s := &ProxyState{Proxies: map[string]ProxyEntry{}}
	tagProxySourceIfUnset(s, "1.2.3.4:1080", "url")
	if got := s.Proxies["1.2.3.4:1080"].Source; got != "url" {
		t.Fatalf("expected source %q, got %q", "url", got)
	}
}

func TestTagProxySourceIfUnset_DoesNotOverwriteExisting(t *testing.T) {
	s := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {ID: 1, Source: "file"},
	}}
	tagProxySourceIfUnset(s, "1.2.3.4:1080", "url")
	if got := s.Proxies["1.2.3.4:1080"].Source; got != "file" {
		t.Fatalf("expected source to remain %q, got %q", "file", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestTagProxySourceIfUnset -v`
Expected: FAIL — `tagProxySourceIfUnset` is not defined, and `ProxyEntry` has no `Source` field (compile error).

- [ ] **Step 3: Write minimal implementation**

In `provider/proxy_state.go`, change the `ProxyEntry` struct (lines 28-32):

```go
// ProxyEntry records the stable ID and last-known health for one proxy.
type ProxyEntry struct {
	ID        int    `json:"id"`
	Health    string `json:"health"`               // "up", "dead", "recently_offline", "offline", "long_offline", "inactive"
	DownSince string `json:"down_since,omitempty"` // RFC3339, set when not up
	Source    string `json:"source,omitempty"`      // "file", "internal", or "url" — where this address was first added from
}
```

Append to the end of `provider/proxy_state.go`:

```go

// tagProxySourceIfUnset records where a proxy address was first added from
// ("file", "internal", or "url"). Once set, the tag is never overwritten —
// an address keeps its original provenance across reloads and restarts, so
// source-scoped dead-proxy cleanup stays accurate even if the same address
// later also appears in a different source.
func tagProxySourceIfUnset(state *ProxyState, address, source string) {
	entry := state.Proxies[address]
	if entry.Source == "" {
		entry.Source = source
	}
	state.Proxies[address] = entry
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run TestTagProxySourceIfUnset -v`
Expected: PASS (both subtests)

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_state.go provider/main_test.go
git commit -m "feat: tag proxy provenance (file/internal/url) in proxy.state"
```

---

### Task 2: Persisted URL-source state file

**Files:**
- Create: `provider/proxy_url.go`
- Create: `provider/proxy_url_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib only).
- Produces: `ProxyURLState{Sources []string, Cache map[string]ProxyURLEntry}`, `ProxyURLEntry{User, Password string}`, `proxyURLStatePath() (string, error)`, `readProxyURLState() (*ProxyURLState, error)`, `readProxyURLStateFrom(path string) (*ProxyURLState, error)`, `writeProxyURLState(*ProxyURLState) error`, `writeProxyURLStateTo(path string, s *ProxyURLState) error`.

- [ ] **Step 1: Write the failing test**

Create `provider/proxy_url_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"
)

func TestWriteReadProxyURLState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	s := &ProxyURLState{
		Sources: []string{"https://example.com/list.txt"},
		Cache: map[string]ProxyURLEntry{
			"1.2.3.4:1080": {},
			"5.6.7.8:1080": {User: "u", Password: "p"},
		},
	}

	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "https://example.com/list.txt" {
		t.Errorf("sources: got %v", got.Sources)
	}
	if len(got.Cache) != 2 {
		t.Errorf("cache: got %d entries, want 2", len(got.Cache))
	}
	if got.Cache["5.6.7.8:1080"].User != "u" {
		t.Errorf("cache entry user: got %q, want %q", got.Cache["5.6.7.8:1080"].User, "u")
	}
}

func TestReadProxyURLState_NotExist(t *testing.T) {
	s, err := readProxyURLStateFrom("/tmp/does-not-exist-proxy_url.json")
	if err != nil {
		t.Fatal(err)
	}
	if s.Cache == nil {
		t.Fatal("expected non-nil Cache map")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestWriteReadProxyURLState -v`
Expected: FAIL — package does not build (`ProxyURLState` undefined)

- [ ] **Step 3: Write minimal implementation**

Create `provider/proxy_url.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ProxyURLState is the on-disk record of configured live proxy URL sources
// and the addresses fetched from them so far. Unlike proxy.state, this file
// is additive-only by design: fetched addresses are only ever removed by
// removeDeadProxies (manual or automatic cleanup), never by a fetch cycle.
type ProxyURLState struct {
	Sources []string                 `json:"sources"`
	Cache   map[string]ProxyURLEntry `json:"cache"`
}

// ProxyURLEntry records the auth (if any) for one address fetched from a URL
// source. Most public proxy lists provide unauthenticated entries.
type ProxyURLEntry struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run "TestWriteReadProxyURLState|TestReadProxyURLState_NotExist" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_url.go provider/proxy_url_test.go
git commit -m "feat: add persisted proxy_url.json state (sources + fetch cache)"
```

---

### Task 3: Parse one proxy-list line (with optional `socks5://` prefix, no required credentials)

**Files:**
- Modify: `provider/proxy_url.go` (append)
- Modify: `provider/proxy_url_test.go` (append)

**Interfaces:**
- Consumes: `parseProxyAddress(string) (address, user, password string)` — existing function, `provider/main.go:2282`.
- Produces: `parseProxyURLLine(line string) (address, user, password string, ok bool)`.

- [ ] **Step 1: Write the failing test**

Append to `provider/proxy_url_test.go`:

```go
func TestParseProxyURLLine(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantAddr     string
		wantUser     string
		wantPassword string
		wantOK       bool
	}{
		{"plain host:port", "1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"host:port:user:pass", "1.2.3.4:1080:myuser:mypass", "1.2.3.4:1080", "myuser", "mypass", true},
		{"socks5 no creds", "socks5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"socks5 with creds", "socks5://myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080", "myuser", "mypass", true},
		{"SOCKS5 case-insensitive scheme", "SOCKS5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"blank line", "", "", "", "", false},
		{"comment line", "# 1.2.3.4:1080", "", "", "", false},
		{"unsupported scheme", "http://1.2.3.4:1080", "", "", "", false},
		{"whitespace padded", "  1.2.3.4:1080  ", "1.2.3.4:1080", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, user, password, ok := parseProxyURLLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if addr != tt.wantAddr {
				t.Errorf("address: got %q, want %q", addr, tt.wantAddr)
			}
			if user != tt.wantUser {
				t.Errorf("user: got %q, want %q", user, tt.wantUser)
			}
			if password != tt.wantPassword {
				t.Errorf("password: got %q, want %q", password, tt.wantPassword)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestParseProxyURLLine -v`
Expected: FAIL — `parseProxyURLLine` undefined (compile error)

- [ ] **Step 3: Write minimal implementation**

Append to `provider/proxy_url.go` (add `"fmt"` and `"strings"` to the existing import block):

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)
```

```go

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
			fmt.Printf("[proxy][url] unsupported scheme %q (only socks5 is supported); skipping %q\n", scheme, line)
			return "", "", "", false
		}
		rest := line[idx+3:]
		if at := strings.LastIndex(rest, "@"); at != -1 {
			cred := rest[:at]
			address = rest[at+1:]
			if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
				user, password = parts[0], parts[1]
			}
			return address, user, password, true
		}
		address, user, password = parseProxyAddress(rest)
		return address, user, password, true
	}

	address, user, password = parseProxyAddress(line)
	return address, user, password, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run TestParseProxyURLLine -v`
Expected: PASS (all 9 subtests)

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_url.go provider/proxy_url_test.go
git commit -m "feat: parse proxy-list lines with optional socks5:// prefix, no required creds"
```

---

### Task 4: Fetch a proxy list over HTTP

**Files:**
- Modify: `provider/proxy_url.go` (append)
- Modify: `provider/proxy_url_test.go` (append)

**Interfaces:**
- Produces: `fetchProxyURLLines(ctx context.Context, url string) ([]string, error)`.

- [ ] **Step 1: Write the failing test**

Append to `provider/proxy_url_test.go` (add `"context"`, `"net/http"`, `"net/http/httptest"`, `"strings"` to imports):

```go
func TestFetchProxyURLLines_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n5.6.7.8:1080\n"))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "1.2.3.4:1080" || lines[1] != "5.6.7.8:1080" {
		t.Fatalf("got %v", lines)
	}
}

func TestFetchProxyURLLines_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFetchProxyURLLines_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestFetchProxyURLLines_BodyTruncatedAtLimit(t *testing.T) {
	huge := strings.Repeat("a", maxProxyURLFetchBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(huge))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total > maxProxyURLFetchBytes {
		t.Fatalf("body not truncated: got %d bytes, want <= %d", total, maxProxyURLFetchBytes)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestFetchProxyURLLines -v`
Expected: FAIL — `fetchProxyURLLines` and `maxProxyURLFetchBytes` undefined (compile error)

- [ ] **Step 3: Write minimal implementation**

Append to `provider/proxy_url.go`. Update the import block to:

```go
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
)
```

```go

// maxProxyURLFetchBytes caps how much of a proxy list response we read,
// defending against a misbehaving or malicious endpoint returning an
// unbounded body.
const maxProxyURLFetchBytes = 10 * 1024 * 1024 // 10 MiB

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
	resp, err := http.DefaultClient.Do(req)
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

	return strings.Split(string(b), "\n"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run TestFetchProxyURLLines -v`
Expected: PASS (all 4 subtests)

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_url.go provider/proxy_url_test.go
git commit -m "feat: fetch proxy list bodies over HTTP with a 10 MiB cap and 30s timeout"
```

---

### Task 5: Merge fetched lines into the cache, and merge the cache into a desired-proxy-set

**Files:**
- Modify: `provider/proxy_url.go` (append)
- Modify: `provider/proxy_url_test.go` (append)

**Interfaces:**
- Consumes: `connect.ProxySettings{Network, Address string; Auth *proxy.Auth; Index int}` (existing type, `github.com/urnetwork/connect`); `proxy.Auth{User, Password string}` (`golang.org/x/net/proxy`).
- Produces: `mergeProxyURLEntries(state *ProxyURLState, lines []string, maxTotal int) (added int)`; `mergeProxyURLCache(desiredSet map[string]*connect.ProxySettings, sourceOf map[string]string, urlState *ProxyURLState)`.

- [ ] **Step 1: Write the failing test**

Append to `provider/proxy_url_test.go` (add `"github.com/urnetwork/connect"` and `"golang.org/x/net/proxy"` to imports):

```go
func TestMergeProxyURLEntries_AddsNewSkipsExisting(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080", // already present, not re-added
		"5.6.7.8:1080:user:pass",
		"# comment, skipped",
	}, 0)
	if added != 1 {
		t.Fatalf("added: got %d, want 1", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
	if state.Cache["5.6.7.8:1080"].User != "user" {
		t.Errorf("expected creds preserved, got %+v", state.Cache["5.6.7.8:1080"])
	}
}

func TestMergeProxyURLEntries_RespectsMaxTotal(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"5.6.7.8:1080",
		"9.9.9.9:1080",
	}, 2)
	if added != 1 {
		t.Fatalf("added: got %d, want 1 (cap of 2 total, 1 already present)", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
}

func TestMergeProxyURLCache_PrimarySourceWins(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{
		"1.2.3.4:1080": {Network: "tcp", Address: "1.2.3.4:1080"},
	}
	sourceOf := map[string]string{"1.2.3.4:1080": "file"}
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {User: "should-be-ignored"},
		"5.6.7.8:1080": {User: "u", Password: "p"},
	}}

	mergeProxyURLCache(desiredSet, sourceOf, urlState)

	if sourceOf["1.2.3.4:1080"] != "file" {
		t.Errorf("existing entry's source was overwritten: got %q", sourceOf["1.2.3.4:1080"])
	}
	if sourceOf["5.6.7.8:1080"] != "url" {
		t.Errorf("new entry not tagged url: got %q", sourceOf["5.6.7.8:1080"])
	}
	settings, ok := desiredSet["5.6.7.8:1080"]
	if !ok {
		t.Fatal("expected new address merged into desiredSet")
	}
	auth, ok := settings.Auth.(*proxy.Auth)
	if !ok || auth.User != "u" || auth.Password != "p" {
		t.Errorf("expected auth u/p, got %+v", settings.Auth)
	}
}

func TestMergeProxyURLCache_NilStateIsNoop(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{}
	sourceOf := map[string]string{}
	mergeProxyURLCache(desiredSet, sourceOf, nil)
	if len(desiredSet) != 0 {
		t.Fatalf("expected no-op, got %v", desiredSet)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run "TestMergeProxyURLEntries|TestMergeProxyURLCache" -v`
Expected: FAIL — `mergeProxyURLEntries` and `mergeProxyURLCache` undefined (compile error)

- [ ] **Step 3: Write minimal implementation**

Append to `provider/proxy_url.go`. Add `"github.com/urnetwork/connect"` and `"golang.org/x/net/proxy"` to the import block (note: this introduces an import-alias collision risk with the stdlib-shadowing `proxy` package name already used the same way in `main.go` — import it identically):

```go
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
```

```go

// mergeProxyURLEntries parses each line and adds genuinely new addresses to
// state.Cache (mutating it in place). Already-cached addresses are left
// untouched — this function only ever adds, never updates or removes.
// maxTotal caps the total cache size; 0 means unlimited. Once the cap is
// reached, remaining lines in this call are skipped without evicting any
// existing entry.
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run "TestMergeProxyURLEntries|TestMergeProxyURLCache" -v`
Expected: PASS (all 4 tests)

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_url.go provider/proxy_url_test.go
git commit -m "feat: merge fetched proxy lines into cache, and cache into desired proxy set"
```

---

### Task 6: Wire the cache merge into `ProxyReloader.reload()`

**Files:**
- Modify: `provider/proxy_reload.go:155-188` (inside `reload()`)

**Interfaces:**
- Consumes: `readProxyURLState() (*ProxyURLState, error)` (Task 2), `mergeProxyURLCache(...)` (Task 5), `tagProxySourceIfUnset(...)` (Task 1).
- Produces: `reload()`'s desired set now includes URL-sourced proxies; newly-added proxies get their `Source` tag set.

This task has no new pure function to unit test in isolation — `reload()` is already deeply coupled to global on-disk state and the running `ProxyReloader`, and has no existing tests today. Verify this task by build + the manual smoke test at the end (Task 10), and by Task 5's tests already covering the merge logic this step wires in.

- [ ] **Step 1: Modify `reload()`'s desired-set construction**

In `provider/proxy_reload.go`, replace lines 185-188:

```go
	desiredSet := make(map[string]*connect.ProxySettings, len(desired))
	for _, s := range desired {
		desiredSet[s.Address] = s
	}
```

with:

```go
	desiredSet := make(map[string]*connect.ProxySettings, len(desired))
	sourceOf := make(map[string]string, len(desired))
	primarySource := "internal"
	if r.sourcePath != "" {
		primarySource = "file"
	}
	for _, s := range desired {
		desiredSet[s.Address] = s
		sourceOf[s.Address] = primarySource
	}

	if urlState, err := readProxyURLState(); err != nil {
		fmt.Printf("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		mergeProxyURLCache(desiredSet, sourceOf, urlState)
	}
```

- [ ] **Step 2: Tag the source of newly-added proxies**

In the same file, in the "Start added proxies" loop, find this block:

```go
		stableID := resolveProxyID(r.state, settings.Address)
		settings.Index = stableID
		connect.RegisterProxy(stableID, settings.Address)
```

Replace it with:

```go
		stableID := resolveProxyID(r.state, settings.Address)
		settings.Index = stableID
		tagProxySourceIfUnset(r.state, settings.Address, sourceOf[settings.Address])
		connect.RegisterProxy(stableID, settings.Address)
```

- [ ] **Step 3: Build to confirm no compile errors**

Run: `cd provider && go build ./...`
Expected: builds cleanly with no errors.

- [ ] **Step 4: Run the full existing test suite to confirm no regressions**

Run: `cd provider && go test . -v 2>&1 | tail -40`
Expected: all existing tests still PASS (this change only adds to the desired set; it does not alter the diff/cancel/drain logic).

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_reload.go
git commit -m "feat: merge URL-sourced proxies into the hot-reload desired set"
```

---

### Task 7: Shared bucketed-removal logic for dead proxies

**Files:**
- Create: `provider/proxy_url_source.go`
- Create: `provider/proxy_url_source_test.go`

**Interfaces:**
- Consumes: `removeAddressesFromFile(path string, addresses []string) error` (existing, `provider/main.go:2549`), `readProxyConfig()`/`writeProxyConfig(*ProxyConfig)` (existing, `provider/main.go:2600,2628`), `parseProxyAddress` (existing), `readProxyURLState()`/`writeProxyURLState(*ProxyURLState) error` (Task 2), `acquireProxyLock() (func(), error)` (existing, `provider/proxy_reload.go:68`), `proxyReloadPath()`/`writeReloadTrigger(path string) error` (existing).
- Produces: `removeDeadProxies(state *ProxyState, addrsBySource map[string][]string) error`.

- [ ] **Step 1: Write the failing test**

Create `provider/proxy_url_source_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome redirects os.UserHomeDir() (and therefore every
// proxy*Path() helper) to a temp directory for the duration of the test.
func withTempHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir() reads this on Windows
	return dir
}

func TestRemoveDeadProxies_RoutesBySource(t *testing.T) {
	home := withTempHome(t)

	fileSourcePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(fileSourcePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeProxyConfig(&ProxyConfig{Servers: map[string]string{
		"3.3.3.3:1080": "",
	}}); err != nil {
		t.Fatal(err)
	}

	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}
	if err := writeProxyURLState(urlState); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Source: fileSourcePath, Proxies: map[string]ProxyEntry{}}

	err := removeDeadProxies(state, map[string][]string{
		"file":     {"1.1.1.1:1080"},
		"internal": {"3.3.3.3:1080"},
		"url":      {"4.4.4.4:1080"},
	})
	if err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(fileSourcePath)
	if got := string(b); got != "2.2.2.2:1080:u:p\n" {
		t.Errorf("file source: got %q", got)
	}

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["3.3.3.3:1080"]; ok {
		t.Errorf("internal source: 3.3.3.3:1080 should have been removed")
	}

	gotURLState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; ok {
		t.Errorf("url source: 4.4.4.4:1080 should have been removed from cache")
	}
}
```

> Note: `writeProxyConfig` in `provider/main.go:2628` currently has no return value (it `panic`s on error) — its signature is `func writeProxyConfig(proxyConfig *ProxyConfig)`, not `error`-returning. Use it exactly as today (no `err :=`):
>
> ```go
> writeProxyConfig(&ProxyConfig{Servers: map[string]string{
> 	"3.3.3.3:1080": "",
> }})
> ```
>
> Correct the test above accordingly before running it.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestRemoveDeadProxies_RoutesBySource -v`
Expected: FAIL — `removeDeadProxies` undefined (compile error)

- [ ] **Step 3: Write minimal implementation**

Create `provider/proxy_url_source.go`:

```go
package main

import (
	"fmt"
)

// removeDeadProxies removes the given addresses from whichever source they
// came from — a --proxy_file source, the internal config, or the URL
// cache — and triggers a hot-reload. addrsBySource groups addresses by their
// proxy.state Source tag ("file", "internal", or "url"); unrecognized keys
// are ignored. Used by both the interactive `proxy remove-dead` command and
// the automatic scoped cleanup job, so removal logic only lives in one place.
func removeDeadProxies(state *ProxyState, addrsBySource map[string][]string) error {
	if fileAddrs := addrsBySource["file"]; len(fileAddrs) > 0 {
		if state.Source == "" {
			fmt.Printf("[proxy] warning: %d proxies tagged source=file but no file source is configured; skipping\n", len(fileAddrs))
		} else if err := removeAddressesFromFile(state.Source, fileAddrs); err != nil {
			return fmt.Errorf("could not update proxy file: %w", err)
		}
	}

	if internalAddrs := addrsBySource["internal"]; len(internalAddrs) > 0 {
		proxyConfig := readProxyConfig()
		removeSet := map[string]bool{}
		for _, a := range internalAddrs {
			removeSet[a] = true
		}
		for proxyAddress := range proxyConfig.Servers {
			addr, _, _ := parseProxyAddress(proxyAddress)
			if removeSet[addr] {
				delete(proxyConfig.Servers, proxyAddress)
			}
		}
		writeProxyConfig(proxyConfig)
	}

	if urlAddrs := addrsBySource["url"]; len(urlAddrs) > 0 {
		urlState, err := readProxyURLState()
		if err != nil {
			return fmt.Errorf("could not read proxy_url.json: %w", err)
		}
		for _, a := range urlAddrs {
			delete(urlState.Cache, a)
		}
		if err := writeProxyURLState(urlState); err != nil {
			return fmt.Errorf("could not write proxy_url.json: %w", err)
		}
	}

	release, err := acquireProxyLock()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	reloadPath, err := proxyReloadPath()
	if err != nil {
		return fmt.Errorf("could not determine reload path: %w", err)
	}
	return writeReloadTrigger(reloadPath)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run TestRemoveDeadProxies_RoutesBySource -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_url_source.go provider/proxy_url_source_test.go
git commit -m "feat: add source-bucketed dead-proxy removal shared by manual and automatic cleanup"
```

---

### Task 8: Refactor `proxyRemoveDead` to use bucketed removal

**Files:**
- Modify: `provider/main.go:2450-2547` (`proxyRemoveDead`)

**Interfaces:**
- Consumes: `removeDeadProxies(state *ProxyState, addrsBySource map[string][]string) error` (Task 7).

- [ ] **Step 1: Replace the removal-application section**

In `provider/main.go`, replace the body of `proxyRemoveDead` from `if state.Source != "" {` (line 2512) through the `fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))` line (line 2546) with:

```go
	addrsBySource := map[string][]string{}
	for _, rp := range toRemove {
		source := rp.entry.Source
		if source == "" {
			// Entries tagged before this feature shipped (or otherwise
			// untagged) keep today's behavior: route by which workflow is
			// active, exactly as proxyRemoveDead always did.
			if state.Source != "" {
				source = "file"
			} else {
				source = "internal"
			}
		}
		addrsBySource[source] = append(addrsBySource[source], rp.addr)
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		shmLogFatal(62, "%v", err)
	}

	fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))
}
```

The function should now read, in full:

```go
func proxyRemoveDead(_ docopt.Opts) {
	state, err := readProxyState()
	if err != nil || state.StartedAt.IsZero() {
		shmLogFatal(60, "provider does not appear to be running")
	}

	uptime := time.Since(state.StartedAt)
	const deadConfirmDelay = 65 * time.Minute
	if uptime < deadConfirmDelay {
		shmLogFatal(61, "provider has only been running %s — need %s uptime before dead status is confirmed", formatDuration(uptime), formatDuration(deadConfirmDelay))
	}

	type removedProxy struct {
		addr  string
		entry ProxyEntry
	}
	var dead, inactive []removedProxy
	for addr, e := range state.Proxies {
		switch e.Health {
		case "dead":
			dead = append(dead, removedProxy{addr: addr, entry: e})
		case "inactive":
			inactive = append(inactive, removedProxy{addr: addr, entry: e})
		}
	}

	if len(dead) == 0 && len(inactive) == 0 {
		fmt.Println("No dead or inactive proxies found.")
		return
	}

	var toRemove []removedProxy

	if len(dead) > 0 {
		fmt.Printf("  %d proxies never authenticated (dead):\n", len(dead))
		for _, rp := range dead {
			fmt.Printf("    proxy[%d]  %s\n", rp.entry.ID, rp.addr)
		}
		fmt.Println()
		if confirm(fmt.Sprintf("Remove %d dead proxies?", len(dead))) {
			toRemove = append(toRemove, dead...)
		}
		fmt.Println()
	}

	if len(inactive) > 0 {
		fmt.Printf("  %d proxies offline 7+ days (inactive):\n", len(inactive))
		for _, rp := range inactive {
			fmt.Printf("    proxy[%d]  %s\n", rp.entry.ID, rp.addr)
		}
		fmt.Println()
		if confirm(fmt.Sprintf("Remove %d inactive proxies?", len(inactive))) {
			toRemove = append(toRemove, inactive...)
		}
		fmt.Println()
	}

	if len(toRemove) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	addrsBySource := map[string][]string{}
	for _, rp := range toRemove {
		source := rp.entry.Source
		if source == "" {
			if state.Source != "" {
				source = "file"
			} else {
				source = "internal"
			}
		}
		addrsBySource[source] = append(addrsBySource[source], rp.addr)
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		shmLogFatal(62, "%v", err)
	}

	fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))
}
```

- [ ] **Step 2: Build to confirm no compile errors**

Run: `cd provider && go build ./...`
Expected: builds cleanly. `removeAddressesFromFile` is still used (inside `removeDeadProxies` now, in `proxy_url_source.go`) so no unused-function errors.

- [ ] **Step 3: Run the full test suite**

Run: `cd provider && go test . -v 2>&1 | tail -40`
Expected: all tests PASS, including the new `TestRemoveDeadProxies_RoutesBySource` from Task 7.

- [ ] **Step 4: Commit**

```bash
git add provider/main.go
git commit -m "refactor: route proxyRemoveDead removals through shared bucketed-removal logic"
```

---

### Task 9: Automatic fetch and cleanup background loops

**Files:**
- Modify: `provider/proxy_url_source.go` (append)
- Modify: `provider/proxy_url_source_test.go` (append)

**Interfaces:**
- Consumes: `fetchProxyURLLines` (Task 4), `mergeProxyURLEntries` (Task 5), `readProxyURLState`/`writeProxyURLState` (Task 2), `proxyReloadPath`/`writeReloadTrigger` (existing), `readProxyState` (existing), `removeDeadProxies` (Task 7).
- Produces: `fetchAndMergeProxyURLs(ctx context.Context, urls []string, maxTotal int)`, `runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int)`, `runProxyURLCleanupOnce(scope string) (removed int)`, `runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration)`.

- [ ] **Step 1: Write the failing test**

Append to `provider/proxy_url_source_test.go` (add `"context"`, `"net/http"`, `"net/http/httptest"`, `"time"` to imports):

```go
func TestFetchAndMergeProxyURLs_PersistsAndTriggersReload(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n5.6.7.8:1080\n"))
	}))
	defer srv.Close()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cache) != 2 {
		t.Fatalf("cache: got %d entries, want 2", len(got.Cache))
	}

	reloadPath, _ := proxyReloadPath()
	seq, _ := readReloadSeq(reloadPath)
	if seq != 1 {
		t.Errorf("reload trigger: got seq %d, want 1", seq)
	}
}

func TestFetchAndMergeProxyURLs_NoOpOnFetchFailure(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cache) != 0 {
		t.Fatalf("expected no entries added on fetch failure, got %d", len(got.Cache))
	}
}

func TestRunProxyURLCleanupOnce_ScopeURL_OnlyTouchesURLSourced(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"4.4.4.4:1080": {Health: "dead", Source: "url"},
		"3.3.3.3:1080": {Health: "dead", Source: "internal"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"3.3.3.3:1080": ""}})

	removed := runProxyURLCleanupOnce("url")
	if removed != 1 {
		t.Fatalf("removed: got %d, want 1", removed)
	}

	gotURLState, _ := readProxyURLState()
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; ok {
		t.Error("expected url-sourced dead proxy to be removed from cache")
	}

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["3.3.3.3:1080"]; !ok {
		t.Error("internal-sourced dead proxy must NOT be removed when scope=url")
	}
}

func TestRunProxyURLCleanupOnce_ScopeNone_RemovesNothing(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"4.4.4.4:1080": {Health: "dead", Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("none")
	if removed != 0 {
		t.Fatalf("removed: got %d, want 0 when scope=none", removed)
	}
}

func TestRunProxyURLFetcher_StopsOnContextCancel(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runProxyURLFetcher(ctx, []string{srv.URL}, time.Hour, 0)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProxyURLFetcher did not stop after context cancellation")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run "TestFetchAndMergeProxyURLs|TestRunProxyURLCleanupOnce|TestRunProxyURLFetcher" -v`
Expected: FAIL — `fetchAndMergeProxyURLs`, `runProxyURLCleanupOnce`, `runProxyURLFetcher` undefined (compile error)

- [ ] **Step 3: Write minimal implementation**

Append to `provider/proxy_url_source.go`. Update the import block to:

```go
import (
	"context"
	"fmt"
	"time"
)
```

```go

// fetchAndMergeProxyURLs fetches every configured source, merges newly
// discovered addresses into the persisted cache (add-only — existing entries
// are never removed here), and triggers a hot-reload if anything new was
// found. A fetch failure for one URL logs a warning and is skipped; it never
// clears already-cached entries from that source.
func fetchAndMergeProxyURLs(ctx context.Context, urls []string, maxTotal int) {
	if len(urls) == 0 {
		return
	}

	state, err := readProxyURLState()
	if err != nil {
		fmt.Printf("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		state = &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}

	totalAdded := 0
	for _, url := range urls {
		lines, err := fetchProxyURLLines(ctx, url)
		if err != nil {
			fmt.Printf("[proxy][url] fetch failed for %s: %v (skipping this cycle)\n", url, err)
			continue
		}
		added := mergeProxyURLEntries(state, lines, maxTotal)
		totalAdded += added
		fmt.Printf("[proxy][url] fetched %s: +%d new proxies\n", url, added)
	}

	if totalAdded == 0 {
		return
	}
	if err := writeProxyURLState(state); err != nil {
		fmt.Printf("[proxy][url] warning: could not write proxy_url.json: %v\n", err)
		return
	}
	if reloadPath, err := proxyReloadPath(); err == nil {
		_ = writeReloadTrigger(reloadPath)
	}
}

// runProxyURLFetcher periodically fetches configured proxy list URLs and
// merges new entries into the running proxy set. The first fetch runs
// immediately; subsequent fetches run every refreshInterval. Exits when ctx
// is cancelled. A no-op if urls is empty.
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int) {
	if len(urls) == 0 {
		return
	}

	fetchAndMergeProxyURLs(ctx, urls, maxTotal)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchAndMergeProxyURLs(ctx, urls, maxTotal)
		}
	}
}

// runProxyURLCleanupOnce removes dead/inactive proxies whose source matches
// scope ("url" removes only url-sourced proxies; "all" removes any source;
// any other value, including "none", removes nothing and returns 0).
// Untagged entries (Source == "", from before this feature shipped) are
// never touched automatically. Returns the number of proxies removed.
func runProxyURLCleanupOnce(scope string) (removed int) {
	if scope != "url" && scope != "all" {
		return 0
	}

	state, err := readProxyState()
	if err != nil {
		fmt.Printf("[proxy][cleanup] warning: could not read proxy.state: %v\n", err)
		return 0
	}

	addrsBySource := map[string][]string{}
	for addr, e := range state.Proxies {
		if e.Health != "dead" && e.Health != "inactive" {
			continue
		}
		if e.Source == "" {
			continue
		}
		if scope == "url" && e.Source != "url" {
			continue
		}
		addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
		removed++
	}

	if removed == 0 {
		return 0
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		fmt.Printf("[proxy][cleanup] warning: %v\n", err)
		return 0
	}
	fmt.Printf("[proxy][cleanup] automatically removed %d dead/inactive proxies (scope=%s)\n", removed, scope)
	return removed
}

// runProxyURLCleanup runs runProxyURLCleanupOnce on a fixed interval until
// ctx is cancelled. A no-op (returns immediately without starting a ticker)
// when scope is "none" or any other disabling value — automatic cleanup is
// opt-in.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration) {
	if scope != "url" && scope != "all" {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProxyURLCleanupOnce(scope)
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd provider && go test . -run "TestFetchAndMergeProxyURLs|TestRunProxyURLCleanupOnce|TestRunProxyURLFetcher" -v`
Expected: PASS (all 5 tests)

- [ ] **Step 5: Run the full test suite**

Run: `cd provider && go test . -v 2>&1 | tail -60`
Expected: all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add provider/proxy_url_source.go provider/proxy_url_source_test.go
git commit -m "feat: add periodic URL fetch and scoped automatic dead-proxy cleanup loops"
```

---

### Task 10: CLI wiring — flags, subcommands, and `provide()` startup integration

**Files:**
- Modify: `provider/main.go` (usage string, dispatch, `provide()`, new `proxyAddSource`/`proxyRemoveSource` functions, three new small resolver helpers)

**Interfaces:**
- Consumes: everything from Tasks 1–9.
- Produces: `provider provide --proxy_url=...`, `provider proxy add-source <url>`, `provider proxy remove-source <url>`; env vars `PROXY_URL`, `PROXY_URL_REFRESH`, `PROXY_URL_MAX`, `PROXY_DEAD_CLEANUP_SCOPE`, `PROXY_DEAD_CLEANUP_INTERVAL` as fallbacks when the corresponding flag isn't passed.

This task is CLI glue with no isolated pure logic of its own (docopt parsing and the `provide()` startup path have no existing tests in this codebase). Verify it via build, the existing test suite, and the manual smoke test in Step 6.

- [ ] **Step 1: Add flags and subcommands to the usage string**

In `provider/main.go`, in the `usage := fmt.Sprintf(...)` block (starting line 524), change the `provide` usage block (lines 536-541) from:

```
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [-v...]
```

to:

```
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [--proxy_url=<proxy_url>...]
        [--proxy_url_refresh=<proxy_url_refresh>]
        [--proxy_url_max=<proxy_url_max>]
        [--proxy_dead_cleanup_scope=<proxy_dead_cleanup_scope>]
        [--proxy_dead_cleanup_interval=<proxy_dead_cleanup_interval>]
        [-v...]
```

Add two new subcommand lines after `provider proxy refresh [--force]` (line 553):

```
    provider proxy refresh [--force]
    provider proxy add-source <url>
    provider proxy remove-source <url>
    provider logs [-n <lines>]
```

Add the new options to the `Options:` block, after the `--proxy_file=<proxy_file>` line (line 574):

```
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_url=<proxy_url>          A live proxy list URL. Repeatable. Additive with --proxy_file / internal config. Also settable via PROXY_URL (comma-separated for multiple).
    --proxy_url_refresh=<dur>        How often to re-fetch --proxy_url sources and add new entries. Also settable via PROXY_URL_REFRESH. [default: 15m if unset]
    --proxy_url_max=<n>              Cap on total proxies sourced from --proxy_url. 0 = unlimited. Also settable via PROXY_URL_MAX.
    --proxy_dead_cleanup_scope=<s>   Automatic daily dead-proxy cleanup scope: none, url, or all. Also settable via PROXY_DEAD_CLEANUP_SCOPE. [default: none if unset]
    --proxy_dead_cleanup_interval=<dur>  How often automatic cleanup runs, when scope isn't none. Also settable via PROXY_DEAD_CLEANUP_INTERVAL. [default: 24h if unset]
    <url>                            A proxy list URL.
```

Do not use docopt's `[default: ...]` annotation syntax (which would always populate the flag and silently defeat the env-var fallback in Step 4) — the "if unset" wording above is documentation prose only, not a docopt default clause.

- [ ] **Step 2: Add dispatch for the new subcommands**

In `provider/main.go`, in `main()`'s dispatch block (around line 600-615), change:

```go
	if proxy, _ := opts.Bool("proxy"); proxy {
		if auth, _ := opts.Bool("auth"); auth {
			if add, _ := opts.Bool("add"); add {
				proxyAuthAdd(opts)
			} else if remove, _ := opts.Bool("remove"); remove {
				proxyAuthRemove(opts)
			}
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if removeDead, _ := opts.Bool("remove-dead"); removeDead {
			proxyRemoveDead(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		} else if refresh, _ := opts.Bool("refresh"); refresh {
			proxyRefresh(opts)
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
```

to:

```go
	if proxy, _ := opts.Bool("proxy"); proxy {
		if auth, _ := opts.Bool("auth"); auth {
			if add, _ := opts.Bool("add"); add {
				proxyAuthAdd(opts)
			} else if remove, _ := opts.Bool("remove"); remove {
				proxyAuthRemove(opts)
			}
		} else if addSource, _ := opts.Bool("add-source"); addSource {
			proxyAddSource(opts)
		} else if removeSource, _ := opts.Bool("remove-source"); removeSource {
			proxyRemoveSource(opts)
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if removeDead, _ := opts.Bool("remove-dead"); removeDead {
			proxyRemoveDead(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		} else if refresh, _ := opts.Bool("refresh"); refresh {
			proxyRefresh(opts)
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
```

`add-source` is checked before `add` deliberately: docopt's `opts.Bool("add")` would also report `true` for the `add-source` invocation since `add-source` is a separate literal token, not a suffix of `add` — but ordering it first removes any ambiguity if docopt's matching ever changes.

- [ ] **Step 3: Add config-resolution helpers**

Append near the bottom of `provider/main.go` (e.g. directly after `obfuscatePassword`, around line 2314), add `"strconv"` is already imported:

```go

// resolveDuration returns the --flag value if set and parseable, else the
// env var if set and parseable, else def. Used for settings that must work
// identically whether passed as a provide() flag or a Docker env var.
func resolveDuration(opts docopt.Opts, flag, envVar string, def time.Duration) time.Duration {
	if v, _ := opts.String(flag); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Printf("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Printf("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, envVar, def)
	}
	return def
}

// resolveInt is resolveDuration's integer counterpart.
func resolveInt(opts docopt.Opts, flag, envVar string, def int) int {
	if v, _ := opts.String(flag); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Printf("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Printf("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, envVar, def)
	}
	return def
}

// resolveString is resolveDuration's plain-string counterpart.
func resolveString(opts docopt.Opts, flag, envVar, def string) string {
	if v, _ := opts.String(flag); v != "" {
		return v
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return def
}

// resolveProxyURLs collects --proxy_url flag values, PROXY_URL env var
// values (comma-separated), and persisted sources from proxy_url.json
// (added via `proxy add-source`), deduplicated, in that priority order.
func resolveProxyURLs(opts docopt.Opts) []string {
	var urls []string

	if v, ok := opts["--proxy_url"]; ok && v != nil {
		switch vv := v.(type) {
		case []string:
			urls = append(urls, vv...)
		case string:
			if vv != "" {
				urls = append(urls, vv)
			}
		}
	}

	if envURLs := os.Getenv("PROXY_URL"); envURLs != "" {
		for _, u := range strings.Split(envURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
	}

	if urlState, err := readProxyURLState(); err != nil {
		fmt.Printf("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		urls = append(urls, urlState.Sources...)
	}

	seen := map[string]bool{}
	deduped := make([]string, 0, len(urls))
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			deduped = append(deduped, u)
		}
	}
	return deduped
}
```

- [ ] **Step 4: Wire resolution + background loops + startup-set merge into `provide()`**

In `provider/main.go`, the proxy-source-selection block currently reads (lines 1531-1547):

```go
	// Select the proxy source: external file (Workflow A) or internal config (Workflow B).
	proxyFile, _ := opts.String("--proxy_file")
	var allProxySettings []*connect.ProxySettings
	if proxyFile != "" {
		settings, err := readProxySettingsFromFile(proxyFile)
		if err != nil {
			shmLogFatal(20, "[proxy] could not read proxy file: %v", err)
		}
		if len(settings) == 0 {
			shmLogFatal(21, "[proxy] proxy file %s contained no valid proxies (expected one ip:port:user:pass per line)", proxyFile)
		}
		allProxySettings = settings
		proxyState.Source = proxyFile
	} else {
		allProxySettings = readProxySettings()
		proxyState.Source = ""
	}
```

Replace it with:

```go
	// Select the proxy source: external file (Workflow A) or internal config (Workflow B).
	proxyFile, _ := opts.String("--proxy_file")
	var allProxySettings []*connect.ProxySettings
	if proxyFile != "" {
		settings, err := readProxySettingsFromFile(proxyFile)
		if err != nil {
			shmLogFatal(20, "[proxy] could not read proxy file: %v", err)
		}
		if len(settings) == 0 {
			shmLogFatal(21, "[proxy] proxy file %s contained no valid proxies (expected one ip:port:user:pass per line)", proxyFile)
		}
		allProxySettings = settings
		proxyState.Source = proxyFile
	} else {
		allProxySettings = readProxySettings()
		proxyState.Source = ""
	}

	// Merge in any already-cached URL-sourced proxies (Workflow A/B + URL
	// source are additive, not mutually exclusive). proxySourceOf records
	// each address's provenance for tagProxySourceIfUnset below.
	primarySource := "internal"
	if proxyFile != "" {
		primarySource = "file"
	}
	proxyDesiredSet := make(map[string]*connect.ProxySettings, len(allProxySettings))
	proxySourceOf := make(map[string]string, len(allProxySettings))
	for _, s := range allProxySettings {
		proxyDesiredSet[s.Address] = s
		proxySourceOf[s.Address] = primarySource
	}
	if urlState, err := readProxyURLState(); err != nil {
		fmt.Printf("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		mergeProxyURLCache(proxyDesiredSet, proxySourceOf, urlState)
	}
	allProxySettings = allProxySettings[:0]
	for _, s := range proxyDesiredSet {
		allProxySettings = append(allProxySettings, s)
	}
```

Then find the per-proxy registration loop a little further down (lines 1578-1581):

```go
		for _, proxySettings := range allProxySettings {
			stableID := resolveProxyID(proxyState, proxySettings.Address)
			proxySettings.Index = stableID
			connect.RegisterProxy(stableID, proxySettings.Address)
```

and change it to:

```go
		for _, proxySettings := range allProxySettings {
			stableID := resolveProxyID(proxyState, proxySettings.Address)
			proxySettings.Index = stableID
			tagProxySourceIfUnset(proxyState, proxySettings.Address, proxySourceOf[proxySettings.Address])
			connect.RegisterProxy(stableID, proxySettings.Address)
```

Finally, find where the other background goroutines are started (lines 1270-1273):

```go
	go runOutageWatcher(ctx, watcherName, os.Getenv("URNETWORK_ALERT_WEBHOOK"))
	go runHealthHeartbeat(ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE"))

	go runJWTRefresher(ctx, apiUrl)
```

and add right after the `runJWTRefresher` line:

```go
	go runJWTRefresher(ctx, apiUrl)

	proxyURLs := resolveProxyURLs(opts)
	proxyURLRefresh := resolveDuration(opts, "--proxy_url_refresh", "PROXY_URL_REFRESH", 15*time.Minute)
	proxyURLMax := resolveInt(opts, "--proxy_url_max", "PROXY_URL_MAX", 0)
	cleanupScope := resolveString(opts, "--proxy_dead_cleanup_scope", "PROXY_DEAD_CLEANUP_SCOPE", "none")
	cleanupInterval := resolveDuration(opts, "--proxy_dead_cleanup_interval", "PROXY_DEAD_CLEANUP_INTERVAL", 24*time.Hour)
	go runProxyURLFetcher(ctx, proxyURLs, proxyURLRefresh, proxyURLMax)
	go runProxyURLCleanup(ctx, cleanupScope, cleanupInterval)
```

- [ ] **Step 5: Add `proxyAddSource` and `proxyRemoveSource`**

In `provider/main.go`, add these two functions directly after `proxyRefresh` (after line 2448, before `func proxyRemoveDead`):

```go
func proxyAddSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)
	if url == "" {
		shmLogFatal(70, "no URL provided")
	}

	state, err := readProxyURLState()
	if err != nil {
		shmLogFatal(71, "could not read proxy_url.json: %v", err)
	}
	for _, existing := range state.Sources {
		if existing == url {
			fmt.Printf("source already added: %s\n", url)
			return
		}
	}
	state.Sources = append(state.Sources, url)
	if err := writeProxyURLState(state); err != nil {
		shmLogFatal(72, "could not write proxy_url.json: %v", err)
	}

	fmt.Printf("added source: %s\nfetching now...\n", url)
	// maxTotal=0 here: the cap configured for the running provide() process
	// (--proxy_url_max) applies to its own background fetcher, not to this
	// one-shot CLI fetch. The next scheduled fetch will resume honoring it.
	fetchAndMergeProxyURLs(context.Background(), []string{url}, 0)
	fmt.Println("done.")
}

func proxyRemoveSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)

	state, err := readProxyURLState()
	if err != nil {
		shmLogFatal(73, "could not read proxy_url.json: %v", err)
	}

	kept := make([]string, 0, len(state.Sources))
	found := false
	for _, existing := range state.Sources {
		if existing == url {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		fmt.Printf("source not found: %s\n", url)
		return
	}

	state.Sources = kept
	if err := writeProxyURLState(state); err != nil {
		shmLogFatal(74, "could not write proxy_url.json: %v", err)
	}
	fmt.Printf("removed source: %s\n", url)
	fmt.Println("note: previously fetched proxies from this source remain running; use 'proxy remove-dead' to prune any that go dead.")
}
```

- [ ] **Step 6: Build, run full test suite, and manual smoke test**

Run: `cd provider && go build ./... && go test . -v 2>&1 | tail -80`
Expected: builds cleanly, all tests PASS.

Manual smoke test (run from `provider/`):

```bash
go build -o /tmp/provider_smoke .
HOME=$(mktemp -d) /tmp/provider_smoke proxy add-source "http://127.0.0.1:1/does-not-matter"
```

Expected output: `added source: http://127.0.0.1:1/does-not-matter`, then `fetching now...`, then a `[proxy][url] fetch failed for ... (skipping this cycle)` warning (connection refused, since nothing is listening on `127.0.0.1:1`), then `done.` — confirming the command persists the source and attempts an immediate fetch without crashing on failure.

```bash
HOME=$same_temp_dir /tmp/provider_smoke proxy remove-source "http://127.0.0.1:1/does-not-matter"
```

Expected output: `removed source: http://127.0.0.1:1/does-not-matter` followed by the note about previously-fetched proxies remaining.

- [ ] **Step 7: Commit**

```bash
git add provider/main.go
git commit -m "feat: wire proxy URL source flags, env vars, subcommands, and background loops into provide()"
```

---

### Task 11: Correct and finalize documentation

**Files:**
- Modify: `docs/Proxy-URL-Sources.md`
- Modify: `docs/Configuration.md`
- Modify: `README.md`
- Modify: `docs/design/proxy-url-source-design.md`

**Interfaces:** None — documentation only.

- [ ] **Step 1: Remove "planned" framing now that the feature is implemented**

In `docs/Proxy-URL-Sources.md`, remove the entire `> [!NOTE] **Status: Planned.** ...` block at the top of the file (the note block directly under the `# 🌐 Proxy URL Sources` heading).

- [ ] **Step 2: Correct the cleanup-scope flag default annotation**

In `docs/Proxy-URL-Sources.md`, the flags table already says `none` is the default for `--proxy_dead_cleanup_scope` — confirm it still reads:

```
| `--proxy_dead_cleanup_scope=url\|all\|none` | `PROXY_DEAD_CLEANUP_SCOPE` | `none` | Which proxies the **automatic** daily cleanup is allowed to remove. `none` disables it entirely (manual `proxy remove-dead` still works regardless). |
```

No change needed if it already matches — this step is a verification checkpoint, not necessarily an edit.

- [ ] **Step 3: Add the `add-source`/`remove-source` CLI examples**

In `docs/Proxy-URL-Sources.md`, under the `### 🐧 Binary / Linux Service` section, confirm the existing example commands match the real flag/subcommand names implemented in Task 10:

```sh
urnetwork provide --proxy_url=https://example.com/your-proxy-list.txt
```

```sh
urnet-tools proxy add-source https://example.com/your-proxy-list.txt
urnet-tools proxy remove-source https://example.com/your-proxy-list.txt
```

These already match Task 10's implementation exactly — no edit needed unless a name was changed during implementation, in which case update this page to match.

- [ ] **Step 4: Remove "(planned)" tags from `docs/Configuration.md`**

In `docs/Configuration.md`, remove ` *(planned)*` from each of the five new env var rows added for this feature (`PROXY_URL`, `PROXY_URL_REFRESH`, `PROXY_URL_MAX`, `PROXY_DEAD_CLEANUP_SCOPE`, `PROXY_DEAD_CLEANUP_INTERVAL`).

- [ ] **Step 5: Remove "(planned)" tag from `README.md`**

In `README.md`, remove ` *(planned)*` from the "Start Here" table row: `| Feed the provider a live proxy list URL | [Proxy URL Sources](docs/Proxy-URL-Sources.md) |`.

- [ ] **Step 6: Correct the design doc's Docker env var translation claim**

In `docs/design/proxy-url-source-design.md`, the "Docker environment variables" section currently states env vars are "Translated by the existing startup scripts... the same way today's env vars map to flags — no new Docker plumbing required." This turned out to be inaccurate: implementation reads `PROXY_URL`, `PROXY_URL_REFRESH`, `PROXY_URL_MAX`, `PROXY_DEAD_CLEANUP_SCOPE`, and `PROXY_DEAD_CLEANUP_INTERVAL` directly via `os.Getenv` inside `provide()` (matching the existing `URNETWORK_PROFILE`/`URNETWORK_REPORT_URL` pattern), not via shell-script flag translation. Replace that sentence with:

```
Read directly via `os.Getenv` inside `provide()` as a fallback whenever the
corresponding `--proxy_*` flag isn't passed — the same pattern already used
by `URNETWORK_PROFILE` and `URNETWORK_REPORT_URL`. No startup-script changes
are needed; setting the env var in `docker run` is sufficient.
```

- [ ] **Step 7: Commit**

```bash
git add docs/Proxy-URL-Sources.md docs/Configuration.md README.md docs/design/proxy-url-source-design.md
git commit -m "docs: mark proxy URL source feature shipped, correct env var mechanism"
```

---

## Final Verification

- [ ] Run the full provider test suite one more time: `cd provider && go test . -v -race 2>&1 | tail -100` — expect all tests PASS, no race warnings.
- [ ] Run `cd provider && go vet ./...` — expect no issues.
- [ ] Confirm `git log --oneline` on this branch shows 11 commits, one per task, each independently buildable.

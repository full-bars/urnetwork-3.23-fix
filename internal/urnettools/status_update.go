package urnettools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// statusUpdateState is the cached answer to "is there a newer release?".
// The update check is time boxed and cached so `urnet-tools status` never
// blocks or fails because of the network.
type statusUpdateState struct {
	Tag         string    `json:"tag"` // "" when unavailable
	At          time.Time `json:"at"`
	Unavailable bool      `json:"unavailable,omitempty"`
}

const (
	// statusUpdateCacheTTL bounds how often we hit GitHub from status.
	statusUpdateCacheTTL  = 10 * time.Minute
	statusUpdateTimeout   = 2 * time.Second
	statusUpdateCacheFile = "update-check.json"
)

// checkStatusUpdateFn allows tests to stub update checking.
var checkStatusUpdateFn = checkStatusUpdate

// checkStatusUpdate returns the latest published release tag, boxed to a
// 2 second budget and cached in the state dir for 10 minutes. It never
// fails the status command: an offline or slow box reports unavailable.
func checkStatusUpdate(stateDir string) statusUpdateState {
	if fresh, ok := readStatusUpdateCache(stateDir); ok {
		return fresh
	}
	state := statusUpdateState{At: time.Now(), Unavailable: true}
	if info, err := fetchLatestReleaseWithin(statusUpdateTimeout); err == nil {
		state.Tag = info.Tag
		state.Unavailable = false
	}
	writeStatusUpdateCache(stateDir, state)
	return state
}

// writeStatusUpdateCache persists the check so the next status call in the
// TTL window does not hit the network. It is best-effort; a read-only state
// dir just skips caching.
func writeStatusUpdateCache(stateDir string, s statusUpdateState) {
	if stateDir == "" {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(stateDir, statusUpdateCacheFile), b, 0o600)
}

// readStatusUpdateCache returns the cached state when it is still within TTL.
func readStatusUpdateCache(stateDir string) (statusUpdateState, bool) {
	if stateDir == "" {
		return statusUpdateState{}, false
	}
	b, err := os.ReadFile(filepath.Join(stateDir, statusUpdateCacheFile))
	if err != nil {
		return statusUpdateState{}, false
	}
	var s statusUpdateState
	if err := json.Unmarshal(b, &s); err != nil {
		return statusUpdateState{}, false
	}
	if time.Since(s.At) > statusUpdateCacheTTL {
		return statusUpdateState{}, false
	}
	return s, true
}

// fetchLatestReleaseWithin walks the release list under a single deadline across
// all pages so status never blocks waiting for GitHub.
func fetchLatestReleaseWithin(timeout time.Duration) (*releaseInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{}
	var skipped []string
	scanned := 0
	for page := 1; page <= maxReleasePages; page++ {
		releases, err := fetchReleasePageWithContext(ctx, client, page)
		if err != nil {
			return nil, fmt.Errorf("fetch latest release: %w", err)
		}
		for _, rj := range releases {
			scanned++
			if reason := skipReason(rj); reason != "" {
				if len(skipped) < 5 {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", rj.TagName, reason))
				}
				continue
			}
			return releaseFromJSON(rj), nil
		}
		if len(releases) < releasePageSize {
			break
		}
	}
	detail := ""
	if len(skipped) > 0 {
		detail = "; skipped " + strings.Join(skipped, ", ")
		if scanned > len(skipped) {
			detail += fmt.Sprintf(" and %d more", scanned-len(skipped))
		}
	}
	if scanned == 0 {
		return nil, fmt.Errorf("no stable release with a verified provider tarball found: the release listing is empty%s", detail)
	}
	return nil, fmt.Errorf("no stable release with a verified provider tarball found in the newest %d releases%s", scanned, detail)
}

// fetchReleasePageWithContext lists one page of releases under a context deadline.
func fetchReleasePageWithContext(ctx context.Context, client *http.Client, page int) ([]releaseJSON, error) {
	api := fmt.Sprintf("%s/releases?per_page=%d&page=%d", repoAPIBase, releasePageSize, page)
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch releases page %d: %w", page, err)
	}
	if k, v := githubAuthHeader(); k != "" {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch releases page %d: %w", page, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == 429 {
		return nil, fmt.Errorf("fetch releases page %d: rate-limited (status %d); set GITHUB_TOKEN for 5000 req/h quota", page, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch releases page %d: status %d", page, resp.StatusCode)
	}
	var releases []releaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases page %d: %w", page, err)
	}
	return releases, nil
}

// statusUpdateParts renders the update line and its fix hint. The hint goes
// on its own indented line so an 80 column screen never wraps mid-command.
func statusUpdateParts(installed string, u statusUpdateState) (main, hint string) {
	if u.Unavailable {
		return "update: installed " + versionOrDash(installed) + "; latest unknown; update check unavailable", ""
	}
	latest := u.Tag
	compare := compareVersionTags(installed, latest)
	switch {
	case installed == "":
		return "update: installed unknown; latest " + latest, ""
	case compare < 0:
		return "update: installed " + installed + "; latest " + latest, "run urnet-tools update"
	case compare > 0:
		return "update: installed " + installed + "; latest " + latest, "running newer than published"
	default:
		return "update: installed " + installed + "; latest " + latest, ""
	}
}

// statusClip truncates s to width runes if longer than width.
func statusClip(s string, width int) string {
	if width <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) > width {
		return string([]rune(s)[:width])
	}
	return s
}

func versionOrDash(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

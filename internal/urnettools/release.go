package urnettools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// releaseInfo describes the latest release fetched from GitHub.
type releaseInfo struct {
	Tag    string
	Digest string // sha256 of the urnetwork-provider-<tag>.tar.gz asset
	URL    string
}

// releaseAsset is the subset of the GitHub release asset JSON we need.
type releaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// releaseJSON is the subset of the GitHub release JSON we need.
type releaseJSON struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// fetchLatestRelease queries the fork's GitHub releases/latest endpoint and
// returns the tag + tarball sha256 digest for the provider asset.
func fetchLatestRelease() (*releaseInfo, error) {
	const api = "https://api.github.com/repos/full-bars/urnetwork-3.23-fix/releases/latest"
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(api)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: status %d", resp.StatusCode)
	}
	var rj releaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&rj); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rj.TagName == "" {
		return nil, fmt.Errorf("release response missing tag_name")
	}
	info := &releaseInfo{
		Tag: rj.TagName,
		URL: fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/urnetwork-provider-%s.tar.gz", rj.TagName, rj.TagName),
	}
	// The release API digest field is "sha256:<hex>"; strip the prefix and
	// match the exact asset name. A missing asset or missing digest is an
	// ERROR, not a silent skip — an unverified download would be executed
	// as the provider user (free-review critical).
	wantName := "urnetwork-provider-" + rj.TagName + ".tar.gz"
	info.Digest = digestForAsset(rj.Assets, wantName)
	if info.Digest == "" {
		return nil, fmt.Errorf("release %s: asset %s has no sha256 digest; refusing unverified download", rj.TagName, wantName)
	}
	return info, nil
}

// digestForAsset finds the sha256 digest (hex, "sha256:" prefix stripped)
// for the named asset. Returns "" when the asset is absent or carries no
// digest — callers treat that as a hard refusal, never a silent skip.
func digestForAsset(assets []releaseAsset, wantName string) string {
	for _, a := range assets {
		if a.Name == wantName {
			return strings.TrimPrefix(a.Digest, "sha256:")
		}
	}
	return ""
}

// fetchReleaseByTag queries the GitHub releases/tags/<tag> endpoint and
// returns the tarball sha256 digest for the provider asset. Used when the
// user passes --tag without --digest so the update is always verified
// against the release API's recorded digest.
func fetchReleaseByTag(tag string) (*releaseInfo, error) {
	api := fmt.Sprintf("https://api.github.com/repos/full-bars/urnetwork-3.23-fix/releases/tags/%s", tag)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(api)
	if err != nil {
		return nil, fmt.Errorf("fetch release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release %s: status %d", tag, resp.StatusCode)
	}
	var rj releaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&rj); err != nil {
		return nil, fmt.Errorf("decode release %s: %w", tag, err)
	}
	info := &releaseInfo{
		Tag: tag,
		URL: fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/urnetwork-provider-%s.tar.gz", tag, tag),
	}
	wantName := "urnetwork-provider-" + tag + ".tar.gz"
	info.Digest = digestForAsset(rj.Assets, wantName)
	if info.Digest == "" {
		return nil, fmt.Errorf("release %s: asset %s has no sha256 digest; refusing unverified download", tag, wantName)
	}
	return info, nil
}

// releaseCacheTTL bounds how often we hit the GitHub API.
const releaseCacheTTL = 5 * time.Minute

// cachedLatest caches the latest release lookup so repeated invocations in
// a short window don't hammer the API.
var (
	cachedLatest     *releaseInfo
	cachedLatestTime time.Time
)

// latestRelease returns the latest release, using the short cache.
func latestRelease() (*releaseInfo, error) {
	if cachedLatest != nil && time.Since(cachedLatestTime) < releaseCacheTTL {
		return cachedLatest, nil
	}
	info, err := fetchLatestRelease()
	if err != nil {
		return nil, err
	}
	cachedLatest = info
	cachedLatestTime = time.Now()
	return info, nil
}

package main

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/urnetwork/connect"
)

// Log lines for URL-sourced proxies. A fetch cycle already prints several detail
// lines (per-source probe results, the cached-skip count, the tier breakdown),
// but none said plainly what happened to the pool, and the quietest case, "every
// address was already known", was the easiest to miss. One headline per cycle
// says it, and one launch line says when URL proxies actually start.

// urlCycleStats is what one URL fetch cycle did to the pool.
type urlCycleStats struct {
	Sources       int // URL sources configured
	Failed        int // sources whose fetch failed this cycle
	Admitted      int // new qualified proxies admitted to the pool
	Held          int // new entries cached below the bar, for the reaper to re-probe
	AlreadyKnown  int // addresses skipped because the cache already had them
	Rejected      int // new addresses that probed below the bar or as socks5-only
	PoolQualified int // qualified proxies in the pool now
	PoolCached    int // all cached entries now
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// String is the cycle headline.
func (s urlCycleStats) String() string {
	pool := fmt.Sprintf("%d qualified of %d cached", s.PoolQualified, s.PoolCached)
	if s.Sources > 0 && s.Failed >= s.Sources {
		return fmt.Sprintf("⚠️ [proxy][url] cycle: every source failed (%d of %d); pool unchanged at %s", s.Failed, s.Sources, pool)
	}
	from := "from " + plural(s.Sources, "source", "sources")
	if s.Failed > 0 {
		from = fmt.Sprintf("from %d of %d sources, %d failed", s.Sources-s.Failed, s.Sources, s.Failed)
	}
	detail := fmt.Sprintf("%d already known, %d rejected", s.AlreadyKnown, s.Rejected)
	if s.Held > 0 {
		detail += fmt.Sprintf(", %d held for re-probe", s.Held)
	}
	if s.Admitted > 0 {
		return fmt.Sprintf("➕ [proxy][url] cycle: +%d new to the pool %s (%s); pool now %s", s.Admitted, from, detail, pool)
	}
	return fmt.Sprintf("✔️ [proxy][url] cycle: nothing new %s (%s); pool %s", from, detail, pool)
}

// urlLaunchLine says how many URL-sourced proxies a reload is starting, and how
// many are held until the file proxies finish warming up. Empty when none were
// added.
func urlLaunchLine(added, held int) string {
	if added <= 0 {
		return ""
	}
	if held <= 0 {
		return fmt.Sprintf("🚀 [proxy][url] launching %d new URL-sourced proxies", added)
	}
	return fmt.Sprintf("🚀 [proxy][url] launching %d of %d new URL-sourced proxies (%d held until the file proxies finish warming up)", added-held, added, held)
}

// reloadSourceBreakdown is the " (url 12, file 2)" appended to the reload summary,
// so an added proxy is attributed to where it came from. Empty when nothing was
// added. The "reloaded: +N added" prefix of the line is unchanged.
func reloadSourceBreakdown(added []*connect.ProxySettings, sourceOf map[string]string) string {
	if len(added) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, s := range added {
		src := sourceOf[s.Key()]
		if src == "" {
			src = "other"
		}
		counts[src]++
	}
	order := []string{"url", "file", "internal"}
	seen := map[string]bool{}
	var parts []string
	for _, src := range order {
		if n := counts[src]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", src, n))
			seen[src] = true
		}
	}
	var rest []string
	for src := range counts {
		if !seen[src] && src != "other" {
			rest = append(rest, src)
		}
	}
	sort.Strings(rest)
	for _, src := range rest {
		parts = append(parts, fmt.Sprintf("%s %d", src, counts[src]))
	}
	if n := counts["other"]; n > 0 {
		parts = append(parts, fmt.Sprintf("other %d", n))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// urlSourceStats is what one URL source did in a fetch cycle. A source's "already
// known" count includes addresses another source probed earlier in the same
// cycle, so a proxy listed by two sources is attributed to the first.
type urlSourceStats struct {
	Label    string
	Failed   bool // the fetch failed
	Lines    int  // parseable proxy lines the source listed
	Known    int  // already in the cache, or probed earlier this cycle
	Dead     int  // new, but failed the probe outright and were dropped
	Rejected int  // new, probed below the bar or as socks5-only
	Added    int  // new and admitted to the pool
	Held     int  // new and cached below the bar, for the reaper to re-probe
}

// String is the per-source line.
func (s urlSourceStats) String() string {
	const prefix = "📥 [proxy][url] source "
	if s.Failed {
		return fmt.Sprintf("%s%s: fetch failed", prefix, s.Label)
	}
	verdict := "nothing new"
	if s.Added > 0 {
		verdict = fmt.Sprintf("+%d new", s.Added)
	}
	held := ""
	if s.Held > 0 {
		held = fmt.Sprintf(", %d held for re-probe", s.Held)
	}
	return fmt.Sprintf("%s%s: %s of %d listed (%d already known, %d rejected, %d dead%s)", prefix, s.Label, verdict, s.Lines, s.Known, s.Rejected, s.Dead, held)
}

// urlSourceLabelMax caps a source label so one very long URL cannot swamp a line.
const urlSourceLabelMax = 64

// urlLabelSecretSegment is what a redacted credential-bearing path segment
// becomes. Fixed-width so a label cannot leak the original length either.
const urlLabelSecretSegment = "[redacted]"

// urlCredentialPathKeywords are path segments that signal the NEXT segment is a
// credential (the common /token/<secret>/... and /key/<key> shapes). Lowercase.
var urlCredentialPathKeywords = map[string]bool{
	"token": true, "tokens": true, "access_token": true, "access-token": true,
	"key": true, "keys": true, "apikey": true, "api_key": true, "api-key": true,
	"secret": true, "secrets": true, "auth": true, "authorization": true,
	"password": true, "passwd": true, "pwd": true, "sig": true, "signature": true,
	"credential": true, "credentials": true, "bearer": true,
}

// urlLabelSegmentIsSecret reports whether a single path segment should be
// redacted: a long opaque blob (a token/key), or the segment right after a
// credential keyword. Filenames (which carry a dot) and short identifiers are
// left intact so non-sensitive path disambiguation still works.
func urlLabelSegmentIsSecret(segment string, prevWasKeyword bool) bool {
	if segment == "" {
		return false
	}
	if prevWasKeyword {
		return true
	}
	if strings.Contains(segment, ".") {
		return looksLikeDottedToken(segment)
	}
	if len(segment) < 32 {
		return false
	}
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		case r == '/':
			// Only reachable from a percent-encoded slash inside one segment
			// (the caller splits on literal slashes). It is part of the blob, not
			// a sign the segment is an ordinary path piece.
		default:
			return false // a normal path char (%, ~, .) means it is not a bare token
		}
	}
	return true
}

// looksLikeDottedToken reports a dotted segment that is a token, not a filename:
// a JWT is three base64url parts, the first two long. A filename has a short
// extension part (http.txt, list.2026.09.24.txt), so it never qualifies.
func looksLikeDottedToken(segment string) bool {
	parts := strings.Split(segment, ".")
	if len(parts) < 3 {
		return false
	}
	base64url := func(s string) bool {
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			default:
				return false
			}
		}
		return true
	}
	for i, p := range parts {
		if i < 2 && (len(p) < 10 || !base64url(p)) {
			return false
		}
		if i >= 2 && !base64url(p) {
			return false // a signature may be empty (alg none) but is still base64url
		}
	}
	return true
}

// redactSourcePath replaces credential-bearing segments of a URL path with a
// fixed placeholder, keeping the non-sensitive segments that disambiguate one
// source from another. Query strings and userinfo never reach here (the caller
// strips them), but a token can also sit in the path itself
// (…/token/SECRET/list), which this covers.
//
// path must be the ESCAPED path (URL.EscapedPath). Splitting the decoded one
// turns an encoded slash inside a token (/token/abc%2FSECRET/list) into two
// segments and masks only the first. Each segment is decoded for judging it, so
// an encoded keyword (%74oken) still counts, and kept as written when it is not
// redacted.
func redactSourcePath(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	prevWasKeyword := false
	for i, seg := range segments {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			decoded = seg
		}
		if urlLabelSegmentIsSecret(decoded, prevWasKeyword && i > 0) {
			segments[i] = urlLabelSecretSegment
			prevWasKeyword = false
			continue
		}
		prevWasKeyword = urlCredentialPathKeywords[strings.ToLower(decoded)]
	}
	return strings.Join(segments, "/")
}

// unparseableSourceLabel is the label for a source URL that url.Parse rejects
// (a stray % in a token, for one). It cannot be trusted to have a safe path, so
// only the host survives: scheme, credentials and path are dropped.
func unparseableSourceLabel(label string) string {
	// Any scheme prefix goes, whatever its slash count: url.Parse accepts
	// scheme:/path with an empty host, and a label with no scheme at all is still
	// cut at its first slash, so a path never survives into the label.
	rest := urlSchemePrefix.ReplaceAllString(label, "")
	authority, tail := rest, ""
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		authority, tail = rest[:j], rest[j:]
	}
	// An '@' after the first slash with none before it: the userinfo may hold an
	// unescaped slash (user:pa/ss@host), so the authority cannot be trusted to be
	// a host at all.
	if !strings.Contains(authority, "@") && strings.Contains(tail, "@") {
		return urlLabelUnparseable
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if authority == "" {
		return urlLabelUnparseable
	}
	return authority
}

// urlSchemePrefix matches "https://", "https:/" and "https:///": a scheme and
// the slashes after it.
var urlSchemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:/+`)

// urlLabelUnparseable stands in for a source URL that cannot be safely shown.
const urlLabelUnparseable = "[unparseable source]"

// urlSourceLabels names each source by host and path only. Source URLs often
// carry an API token in the query string or credentials in the userinfo, and a
// label lands in the important log buffer, so neither is ever included.
// Credential-bearing path segments are redacted too. Sources that would share a
// label are numbered.
func urlSourceLabels(urls []string) []string {
	labels := make([]string, len(urls))
	seen := map[string]int{}
	for i, raw := range urls {
		label := raw
		if cut := strings.IndexAny(label, "?#"); cut >= 0 {
			// A "?#" inside the userinfo (before the '@') is part of a
			// credential, not a query: cutting there would leak the credential
			// start as a "host" (e.g. "user:pa?ss@host" -> "user:pa"). Many
			// real sources carry a token in the query string after a normal
			// userinfo, so only distrust a cut that precedes the '@'.
			if at := strings.IndexByte(label, '@'); at >= 0 && cut < at {
				label = urlLabelUnparseable
				seen[label]++
				labels[i] = label
				continue
			}
			label = label[:cut]
		}
		if u, err := url.Parse(label); err == nil && u.Host != "" {
			if u.User == nil && strings.Contains(u.EscapedPath(), "@") {
				// No userinfo was parsed but the path holds an '@': a password with an
				// unescaped slash (user:12345/ss@host) parses as a host and a path.
				// The "host" may be the start of the credential, so show neither.
				label = urlLabelUnparseable
			} else {
				label = u.Host + redactSourcePath(strings.TrimSuffix(u.EscapedPath(), "/"))
			}
		} else {
			label = unparseableSourceLabel(label)
		}
		if len(label) > urlSourceLabelMax {
			label = label[:urlSourceLabelMax-3] + "..."
		}
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s #%d", label, n)
		}
		labels[i] = label
	}
	return labels
}

package main

import (
	"fmt"
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

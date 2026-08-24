package main

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"sort"
)

// Paid/file-list proxy grading (design note 2026-08-09).
//
// The URL-source machinery grades every address it admits (stage-1 table
// probe, A-F tiers), but paid/file-list proxies — which come from
// --proxy_file or the internal config and bypass the URL admission gate by
// construction (isURLSourced == false) — were never graded at all. This
// sweep grades EVERY non-URL proxy the box serves with the SAME stage-1
// table probe and the same proxyTableProbeConfig, so the operator sees the
// quality distribution of what the paid lists actually deliver, and the
// grade is available for resource prioritization (roadmap #2) and
// dashboard surfacing (roadmap #3).
//
// SAFETY: this is READ-ONLY with respect to the proxy lifecycle. Grades
// live in proxy.state ProxyEntry (Score/Graded/Failed/LastGraded) and are
// never consulted by admission, eviction, give-up, or cleanup — the "never
// reject" property is structural: paid/file proxies already bypass the
// stage-1 gate, and the sweep only ever writes grade fields. A graded F
// keeps serving exactly as it did before it was graded.
//
// Cadence: one pass every proxyReaperInterval tick, re-probing only
// entries whose LastGraded is older than the reaper stale threshold
// (1-3h, pressure-scaled) — the same window the URL reaper uses, so paid
// grading rides the existing stale sweep cadence. Kill switch:
// proxy_probe.json enabled=false disables the table probe here too
// (a full skip, mirroring the fetch-side invariant).

// gradeTarget is one paid/file proxy scheduled for this grading pass: its
// address, credentials (when resolvable), and the LastGraded snapshot it was
// collected under (so the apply phase can detect a concurrent refresh). Shared
// between the collector, the per-tick budget sorter, and the probe fan-out.
type gradeTarget struct {
	addr             string
	user             string
	password         string
	snapshotGradedAt time.Time
}

// runPaidProxyGrader drives the paid/file-proxy grade sweep on the reaper
// ticker cadence. The pass itself is split out so it can be exercised
// directly in tests without waiting on proxyReaperInterval.
func runPaidProxyGrader(ctx context.Context, apiHost string, apiPort uint16) {
	ticker := time.NewTicker(proxyReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runPaidProxyGradeOnce(ctx, apiHost, apiPort)
	}
}

// runPaidProxyGradeOnce performs a single paid/file-proxy grading pass:
// collect the stale non-URL desired proxies, table-probe them outside the
// state lock, then apply the grades atomically. Mirrors the URL reaper's
// collect/probe/apply split so a slow probe batch never blocks reloads or
// the heartbeat.
func runPaidProxyGradeOnce(ctx context.Context, apiHost string, apiPort uint16) {
	probeCfg := resolveProxyTableProbeConfig()
	// Paid-only probe knobs: a cheap stage-0 SOCKS5-greeting liveness gate so a
	// dead paid proxy is dropped in ONE dial (no sample block wasted), and a
	// START-SMALL base width so a clearly-good or clearly-dead proxy settles at
	// 6 dials instead of the full sample width — bandwidth is spent only in the
	// borderline middle, which grows toward the cap. These are set here (not in
	// the shared proxy_probe.json) because URL admission must keep its own
	// stage-0 + full-width behavior unchanged.
	probeCfg.Stage0Liveness = true
	if probeCfg.MinSampleWidth <= 0 || probeCfg.MinSampleWidth >= probeCfg.SampleWidth {
		probeCfg.MinSampleWidth = 6
	}
	if !probeCfg.Enabled {
		// Kill switch: stage-1 table probing is off globally. Paid
		// grading must be a full skip too — the operator turned the
		// probes off because the probes themselves are the problem.
		return
	}

	var targets []gradeTarget

	// Collect the non-URL desired set under the lock, then probe outside
	// it. The desired set is what the box intends to serve: the source
	// file (--proxy_file, always credentialed) or the internal config.
	func() {
		proxyStateMu.Lock()
		defer proxyStateMu.Unlock()

		state, err := readProxyState()
		if err != nil {
			tlog("[proxy][grade] warning: could not read proxy.state: %v\n", err)
			return
		}
		// Collect the paid/file target set from the TRACKED proxy.state entries
		// (the authoritative runtime list of every proxy actually serving),
		// merging each address's credentials from the config readers. Previously,
		// when state.Source=="" the collector read the static proxy.json config,
		// which is empty at runtime (proxies come from a file/internal reload into
		// proxy.state, not the static config), so it collected ZERO targets every
		// sweep and paid proxies stayed ungraded forever even though they were
		// tracked and running.
		//
		// Credentials: ProxyEntry (proxy.state) does not carry user/pass, so we
		// resolve each tracked address's settings (with Auth) from the same
		// readers the probe needs; a paid proxy with no resolvable creds is still
		// graded (the dial may succeed without auth).
		credsByAddr, desiredSetTrusted := paidDesiredSet(state)

		var desired []*connect.ProxySettings
		if len(state.Proxies) > 0 {
			for addr, entry := range state.Proxies {
				// Collect predicate MUST mirror the apply predicate (membership
				// in this same union), or a tracked-but-not-desired entry is
				// dialed every tick and discarded at apply before LastGraded
				// advances — a permanent never-graded budget squatter that can
				// starve real paid targets of all 200 slots (Opus review HIGH-1,
				// proven with a two-tick harness).
				//
				// Membership IS the gradability condition WHEN the union is
				// trustworthy (file read fine or no source file). When the
				// source file is UNREADABLE we cannot know ownership, so we
				// keep grading tracked entries whose tag says paid/file-owned
				// (collect-fix behavior; ReadErrorStillProbesTracked) and only
				// drop explicitly URL-tagged ones. "File wins" still applies:
				// an address in the union is graded regardless of its tag.
				inDesired := credsByAddr[addr] != nil
				if !inDesired && desiredSetTrusted {
					continue
				}
				if !inDesired && !desiredSetTrusted && entry.Source == "url" {
					continue
				}
				ps := &connect.ProxySettings{Network: "tcp", Address: addr}
				// Creds may be unresolvable here (unreadable file / not in the
				// union): the proxy is still graded, the dial may succeed
				// without auth (collect-fix invariant).
				if creds := credsByAddr[addr]; creds != nil && creds.Auth != nil {
					ps.Auth = creds.Auth
				}
				desired = append(desired, ps)
			}
		} else if state.Source != "" {
			desired, err = readProxySettingsFromFile(state.Source)
			if err != nil {
				tlog("[proxy][grade] warning: %v\n", err)
				return
			}
		} else {
			desired = readProxySettings()
		}

		// PAID window, not the URL window: paid proxies are stable and the
		// operator pays for their probe bandwidth, so they are re-probed far
		// less often (6h calm / 3h hot vs the URL 3h/1h).
		paidStaleAfter := paidStaleThreshold(currentPressure())
		now := time.Now()
		for _, s := range desired {
			// Only proxies the box actually tracks (has a ProxyEntry) are
			// graded: the reload path creates the entry when the proxy
			// launches, so a later sweep grades it. Requiring an entry
			// here AND at apply prevents ghost entries for proxies
			// removed between collect and apply (a concurrent reload or
			// operator delete) — the URL reaper applies the same rule
			// ("removed by a concurrent writer").
			entry, ok := state.Proxies[s.Address]
			if !ok {
				continue
			}
			// The desired set IS the ownership definition: anything in
			// the file/internal set is served as a file proxy (file wins
			// in mergeProxyURLCache), so it is graded here even if a
			// stale first-seen tag says "url" (independent review HIGH finding).
			if !entry.LastGraded.IsZero() && now.Sub(entry.LastGraded) < paidStaleAfter {
				continue // fresh grade; ride the paid stale window
			}
			// Earn-skip (delta-based): a paid proxy with RECENT billable
			// traffic is demonstrably alive — the backend is routing real
			// sessions through it. Probing it would spend paid bandwidth to
			// learn what the traffic already proves. The signal is the
			// per-address earn tracker (positive billable delta within
			// paidEarnWindow), NEVER the raw cumulative counter: a
			// cumulative-only check would let a proxy that earned once
			// early then died look "earning" forever and never be re-probed
			// (Sonnet review finding 2c).
			//
			// Hard ceiling: even an actively-earning proxy is force-probed
			// at least once per paidForceProbeCeiling (24h) so the fail-fast
			// path can never be starved — "earning" suppresses probes, but
			// only for a bounded time (findings 2c/4b). A NEVER-graded
			// proxy (LastGraded zero) is always force-probed: earn-skip
			// must never prevent the FIRST grade, or an earning proxy with
			// no grade stays ungraded forever (review CRITICAL).
			earnedRecently := globalPerProxyEarnTracker.EarnedSince(s.Address, paidEarnWindow)
			forceProbeDue := entry.LastGraded.IsZero() || now.Sub(entry.LastGraded) >= paidForceProbeCeiling
			if earnedRecently && !forceProbeDue {
				continue // earning and not past the ceiling — save the bandwidth
			}
			t := gradeTarget{addr: s.Address, snapshotGradedAt: entry.LastGraded}
			if s.Auth != nil {
				t.user = s.Auth.User
				t.password = s.Auth.Password
			}
			targets = append(targets, t)
		}
	}()

	if len(targets) == 0 {
		return
	}

	// PER-TICK PROBE BUDGET (maxPaidProbesPerTick). The collector above keeps
	// every stale/eligible paid proxy; when the fleet is large that can be
	// thousands per 5-minute tick, which is the ~22h full-sweep problem. Cap
	// this pass at MaxPaidProbesPerTick, probing the OLDEST-STALE-FIRST
	// (smallest snapshotGradedAt = longest since last grade, never-graded
	// first) and deferring the rest to a later tick. Self-throttling and
	// bounded: a 4000-proxy sweep at 200/tick finishes in ~20 ticks
	// (~100 minutes) while paying at most 200 paid probes per 5 minutes.
	// (Design 2026-08-23.)
	targets = applyPaidProbeBudget(targets, probeCfg.MaxPaidProbesPerTick)

	// Probe in parallel under the same pressure-scaled semaphore the fetch
	// uses; each individual table pass is sequential through its proxy.
	sem := make(chan struct{}, scaledProbeConcurrency(currentPressure()))
	type gradeResult struct {
		addr             string
		snapshotGradedAt time.Time
		user             string
		password         string
		table            tableProbeResult
	}
	results := make([]gradeResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t gradeTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = gradeResult{
				addr:             t.addr,
				snapshotGradedAt: t.snapshotGradedAt,
				user:             t.user,
				password:         t.password,
				table:            probeTableThroughProxy(ctx, t.addr, t.user, t.password, apiHost, apiPort, probeCfg),
			}
		}(i, t)
	}
	wg.Wait()
	// Capture the probe-completion time once, so every entry's LastGraded
	// reflects when its probe finished (not when the apply phase happened
	// to run under the lock) — the delta is small, but under load the
	// apply can be delayed by lock contention (review nit).
	probeDone := time.Now()

	// Apply the grades atomically. Only the grade fields are touched:
	// Health/DownSince/Source/AuthFailures and the proxy lifecycle are
	// never modified, and nothing here gates, evicts, or gives up on any
	// proxy.
	func() {
		if ctx.Err() != nil {
			// A cancelled sweep carries no verdict (finding C1): in-flight
			// probes bailed early, so persisting now would mark up to
			// maxPaidProbesPerTick proxies Pending / advance their clocks for
			// "we were shutting down" rather than evidence. Leave state alone;
			// the next tick re-collects (Opus review MEDIUM-1).
			return
		}
		proxyStateMu.Lock()
		defer proxyStateMu.Unlock()

		state, err := readProxyState()
		if err != nil {
			tlog("[proxy][grade] warning: could not read proxy.state: %v\n", err)
			return
		}
		// Re-read the CURRENT settings so a concurrent reload that changed an
		// address's credentials (or removed it) between collect and apply
		// cannot have a stale-creds probe result persisted (coderabbit
		// review). On mismatch the result is skipped entirely — no grade, no
		// LastGraded advance — so the next sweep probes with current settings.
		//
		// This apply-time set is built the way the COLLECT side builds it
		// (union of the source file, when set, AND the internal config),
		// because the collector's desired set is every tracked proxy in
		// state.Proxies — including URL-admitted and mixed file+internal
		// addresses. A single-source either/or here would let such an
		// address be collected + dialed every pass yet fail this lookup and
		// hit continue: never graded, LastGraded never advancing, and the
		// paid-budget sort keeping its never-graded flag first forever
		// (Sonnet review CRITICAL).
		current := map[string]connect.ProxySettings{}
		desiredSet, fileOK := paidDesiredSet(state)
		for addr, s := range desiredSet {
			current[addr] = *s
		}
		changed := false
		graded := 0
		pending := 0
		tierChanges := 0
		for _, r := range results {
			entry, ok := state.Proxies[r.addr]
			if !ok {
				continue // removed by a concurrent writer; do not resurrect (independent review MEDIUM)
			}
			if entry.LastGraded.After(r.snapshotGradedAt) {
				continue // refreshed by a concurrent pass; do not clobber
			}
			s, ok := current[r.addr]
			if !ok {
				// Mirror the collect-side fallback so apply cannot discard what
				// collect dialed. When fileOK is false the source-file leg of the
				// union is untrusted (unreadable/empty), so membership alone cannot
				// prove "not paid-owned"; collect therefore probes every tracked
				// non-URL-tagged entry without creds. Rejecting it HERE -- before
				// LastGraded advances -- makes it a permanent squatter: dialed every
				// tick, never graded, its never-graded flag keeping it first in the
				// paid-budget sort and starving graded targets of the per-tick
				// budget. This is the exact inverse of the fileOK=false predicates
				// on the collect side (proxy_grade_paid.go collect filter).
				if fileOK || entry.Source == "url" {
					continue // removed from the desired set mid-pass; do not grade
				}
				s = connect.ProxySettings{Network: "tcp", Address: r.addr}
			}
			if !paidGradeSettingsMatch(s, r.user, r.password) {
				// Credentials changed mid-pass: the probe ran against the
				// OLD settings, so neither the grade nor the staleness
				// clock may move (the next sweep probes the new settings).
				continue
			}
			// Advance the staleness clock on ANY completed pass so a
			// DNS-gutted (undecidable) pass does not re-probe every tick;
			// the grade itself is persisted only on a decidable verdict.
			entry.LastGraded = probeDone
			// PENDING always reflects the CURRENT pass's outcome, never a
			// lingering value from an earlier one. Reset here so (a) a
			// decidable pass clears a stale Pending without a special-case
			// guard, (b) a fully-unreachable pass (Total==0, below) clears a
			// Pending set when the proxy was previously reachable-but-
			// undecidable — otherwise an unreachable proxy would be mislabeled
			// "could not evaluate from this box" forever. Re-set to true only
			// in the reachable-but-undecidable branch.
			entry.Pending = false
			if r.table.Decidable {
				oldTier := ""
				oldScore := entry.Score
				wasGraded := entry.Graded // capture BEFORE the write below (HIGH-1)
				if entry.Graded {
					oldTier = proxyGradeTier(entry.Score)
				}
				newTier := proxyGradeTier(r.table.Score)
				entry.Score = r.table.Score
				entry.Graded = true
				entry.Failed = capFailedList(r.table.Failed)
				graded++
				if oldTier != newTier {
					tierChanges++
					importantLogf("[proxy][grade] paid %s graded %s (score %.2f, %d/%d)\n",
						r.addr, newTier, r.table.Score, r.table.OK, r.table.SampleWidth)
					// Per-address delta line into grades.log history too.
					emitProxyGradeDelta(r.addr, oldTier, newTier, oldScore, r.table.Score, wasGraded)
				}
			} else if r.table.Total > 0 {
				// REACHABLE-but-undecidable: the probe dialed through the
				// proxy (or at least reached it) but could not produce a
				// confident verdict — the box's DNS could not answer most of
				// the intended sample, or the proxy is so strict/rate-limited
				// that a through-proxy answer could not be confirmed. This is
				// NOT a grade and NOT "ungraded (never reached)": it is an
				// honest "could not evaluate from this box". Persist a
				// distinct pending status so the operator does not mistake a
				// method artifact for proxy quality — and so the summary can
				// report it (design 2026-08-23).
				entry.Pending = true
				// A pending pass does NOT write a grade: Score/Graded stay at
				// their prior values (or absent for a never-graded proxy), so
				// a pending proxy is never labelled with a wrong tier.
				pending++
			}
			state.Proxies[r.addr] = entry
			changed = true
		}
		if graded > 0 || pending > 0 {
			// One aggregate line per pass, matching the reaper's summary
			// convention (the important buffer must not become a
			// per-proxy stream on a large file list).
			importantLogf("[proxy][grade] graded %d paid/file proxies (%d pending, %d tier changes)\n",
				graded, pending, tierChanges)
		}
		if changed {
			if err := writeProxyState(state); err != nil {
				tlog("[proxy][grade] warning: could not write proxy.state: %v\n", err)
			}
		}
	}()
}

// paidDesiredSet returns the addresses the paid grader OWNS: the UNION of the
// live source file (when state.Source is set) and the internal config. This
// union is the single source of truth for paid ownership and MUST be used by
// every site that needs it — the collect phase (which targets to dial), the
// apply phase (which results are gradable), and the summary (whose ownership
// bucketing must agree with the writer). A previous either/or construction in
// the summary let internal-config addresses under a set state.Source be
// reported ungraded with the wrong staleness window (Opus review MEDIUM-2);
// centralizing guarantees the three sites cannot drift again. File-read errors
// are logged and skipped: apply proceeds with the internal config, mirroring
// the collect side.
// The second return (fileOK) reports whether the source-file leg of the union
// could be trusted: false means state.Source was SET but its file could not be
// read, so membership in the set alone cannot prove "not paid-owned" — the
// collector must then fall back to grading tracked non-URL-tagged entries
// (TestPaidProxyGrader_ReadErrorStillProbesTracked pins this).
func paidDesiredSet(state *ProxyState) (map[string]*connect.ProxySettings, bool) {
	out := map[string]*connect.ProxySettings{}
	fileOK := true
	if state.Source != "" {
		cf, err := readProxySettingsFromFile(state.Source)
		switch {
		case err != nil:
			// Unreadable: membership cannot prove non-ownership.
			fileOK = false
			tlog("[proxy][grade] warning: %v (paid desired set proceeds with internal config only; file %s unreadable)\n", err, state.Source)
		case len(cf) == 0:
			// Readable but EMPTY: it proves nothing about ownership either —
			// the operator's file may be momentarily empty mid-edit — so keep
			// the fallback path (EmptySourceFileStillProbesTracked).
			fileOK = false
		default:
			for _, s := range cf {
				out[s.Address] = s
			}
		}
	}
	for _, s := range readProxySettings() {
		if _, ok := out[s.Address]; !ok {
			out[s.Address] = s
		}
	}
	return out, fileOK
}

// paidGradeSettingsMatch reports whether the address's current settings
// carry the same credentials the sweep probed with. Used at apply time to
// reject results whose probe ran against settings that have since changed.
func paidGradeSettingsMatch(s connect.ProxySettings, user, password string) bool {
	if s.Auth == nil {
		return user == "" && password == ""
	}
	return s.Auth.User == user && s.Auth.Password == password
}

// applyPaidProbeBudget caps a graded target list at budget, keeping the
// oldest-stale-first (never-graded and longest-since-graded first). budget<=0
// disables the cap. The sort is stable so same-staleness targets keep their
// (deterministic) collection order.
func applyPaidProbeBudget(targets []gradeTarget, budget int) []gradeTarget {
	if budget <= 0 || len(targets) <= budget {
		return targets
	}
	sort.SliceStable(targets, func(i, j int) bool {
		// Zero (never graded) sorts before any timestamp: a never-graded paid
		// proxy is the most urgent to evaluate. Then oldest LastGraded first.
		if targets[i].snapshotGradedAt.IsZero() != targets[j].snapshotGradedAt.IsZero() {
			return targets[i].snapshotGradedAt.IsZero()
		}
		return targets[i].snapshotGradedAt.Before(targets[j].snapshotGradedAt)
	})
	return targets[:budget]
}

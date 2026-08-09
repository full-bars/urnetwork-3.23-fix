package main

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
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
	if !probeCfg.Enabled {
		// Kill switch: stage-1 table probing is off globally. Paid
		// grading must be a full skip too — the operator turned the
		// probes off because the probes themselves are the problem.
		return
	}

	type gradeTarget struct {
		addr             string
		user             string
		password         string
		snapshotGradedAt time.Time
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
		var desired []*connect.ProxySettings
		if state.Source != "" {
			desired, err = readProxySettingsFromFile(state.Source)
			if err != nil {
				// readProxySettingsFromFile already wraps the path in its
				// error; do not re-state it (double-wrap in the log).
				tlog("[proxy][grade] warning: %v\n", err)
				return
			}
		} else {
			desired = readProxySettings()
		}

		staleAfter := reaperStaleThreshold(currentPressure())
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
			if !entry.LastGraded.IsZero() && now.Sub(entry.LastGraded) < staleAfter {
				continue // fresh grade; ride the 1-3h stale window
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
				table:            probeTableThroughProxy(ctx, t.addr, t.user, t.password, probeCfg),
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
		proxyStateMu.Lock()
		defer proxyStateMu.Unlock()

		state, err := readProxyState()
		if err != nil {
			tlog("[proxy][grade] warning: could not read proxy.state: %v\n", err)
			return
		}
		// Re-read the CURRENT desired settings so a concurrent reload that
		// changed an address's credentials (or removed it) between collect
		// and apply cannot have a stale-creds probe result persisted
		// (coderabbit review). On mismatch the result is skipped entirely —
		// no grade, no LastGraded advance — so the next sweep probes with
		// the current settings.
		current := map[string]connect.ProxySettings{}
		if state.Source != "" {
			cur, err := readProxySettingsFromFile(state.Source)
			if err != nil {
				tlog("[proxy][grade] warning: %v (skipping apply)\n", err)
				return
			}
			for _, s := range cur {
				current[s.Address] = *s
			}
		} else {
			for _, s := range readProxySettings() {
				current[s.Address] = *s
			}
		}
		changed := false
		graded := 0
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
				continue // removed from the desired set mid-pass; do not grade
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
			if r.table.Decidable {
				oldTier := ""
				oldScore := entry.Score
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
					emitProxyGradeDelta(r.addr, oldTier, newTier, oldScore, r.table.Score, entry.Graded)
				}
			}
			state.Proxies[r.addr] = entry
			changed = true
		}
		if graded > 0 {
			// One aggregate line per pass, matching the reaper's summary
			// convention (the important buffer must not become a
			// per-proxy stream on a large file list).
			importantLogf("[proxy][grade] graded %d paid/file proxies (%d tier changes)\n", graded, tierChanges)
		}
		if changed {
			if err := writeProxyState(state); err != nil {
				tlog("[proxy][grade] warning: could not write proxy.state: %v\n", err)
			}
		}
	}()
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

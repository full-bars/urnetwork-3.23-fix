package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"
)

// Evidence for the incidents the provider cannot explain afterwards.
//
// A provider that is heading into a memory spiral stops answering anything
// within minutes: pprof times out, the log ring floods, the status files freeze.
// By the time anyone looks, the evidence of what piled up is gone. So the
// provider watches two cheap numbers (goroutine count and heap against its soft
// limit) and, when either shows the build-up, writes a goroutine profile and a
// heap profile to disk while it can still run. It is read-only diagnostics, a
// few captures a day at most, to ~/.urnetwork/incidents/. URNETWORK_INCIDENT_CAPTURE=0
// turns it off.

const (
	incidentSampleInterval = 10 * time.Second
	incidentWindow         = 30 * time.Minute
	// incidentMinHistory is how much history a goroutine baseline needs; a young
	// process has no baseline to grow from.
	incidentMinHistory = 10 * time.Minute
	// incidentStartupRamp: after a start the goroutine count climbs for a while
	// as the proxies authenticate (14k to 21k over 40 minutes on the canary), so
	// the growth rule does not judge against a baseline taken inside the ramp.
	// The heap rules have no such delay.
	incidentStartupRamp = 45 * time.Minute
	// incidentGrowthRatio over the window's lowest count, and never under the
	// absolute floor, is a build-up. 1.5x of a steady 23k was a normal day on the
	// canary that stalled; 38k, then 61k, was the way into the spiral.
	incidentGrowthRatio  = 1.5
	incidentGoroutineMin = 20000
	incidentHeapNear     = 0.9
	incidentHeapOver     = 1.2
	// one capture per reason per cooldown, one per spacing at all, and a daily
	// ceiling, so a long incident leaves a handful of profiles, not a full disk
	incidentReasonCooldown = 30 * time.Minute
	incidentSpacing        = 60 * time.Second
	incidentMaxPerDay      = 6
	incidentKeep           = 8
)

type incidentSample struct {
	at         time.Time
	goroutines int
	heapFrac   float64
}

// incidentTracker is the pure decision: feed it samples, it says when to capture.
type incidentTracker struct {
	first        time.Time // the first sample seen: when this process started sampling
	samples      []incidentSample
	lastByReason map[string]time.Time
	last         time.Time
	captures     []time.Time
}

func newIncidentTracker() *incidentTracker {
	return &incidentTracker{lastByReason: map[string]time.Time{}}
}

// observe records the sample and reports whether to capture now, and why.
func (self *incidentTracker) observe(s incidentSample) (string, bool) {
	if self.first.IsZero() {
		self.first = s.at
	}
	cutoff := s.at.Add(-incidentWindow)
	keep := self.samples[:0]
	for _, old := range self.samples {
		if old.at.After(cutoff) {
			keep = append(keep, old)
		}
	}
	self.samples = append(keep, s)

	reason := ""
	switch {
	case s.heapFrac >= incidentHeapOver:
		reason = "heap-over-limit"
	case s.heapFrac >= incidentHeapNear:
		reason = "heap-near-limit"
	case self.goroutineGrowth(s):
		reason = "goroutine-growth"
	}
	if reason == "" {
		return "", false
	}
	if !self.last.IsZero() && s.at.Sub(self.last) < incidentSpacing {
		return "", false
	}
	if at, ok := self.lastByReason[reason]; ok && s.at.Sub(at) < incidentReasonCooldown {
		return "", false
	}
	dayAgo := s.at.Add(-24 * time.Hour)
	recent := self.captures[:0]
	for _, at := range self.captures {
		if at.After(dayAgo) {
			recent = append(recent, at)
		}
	}
	self.captures = recent
	if len(self.captures) >= incidentMaxPerDay {
		return "", false
	}
	self.last = s.at
	self.lastByReason[reason] = s.at
	self.captures = append(self.captures, s.at)
	return reason, true
}

func (self *incidentTracker) goroutineGrowth(s incidentSample) bool {
	if s.goroutines < incidentGoroutineMin || len(self.samples) < 2 {
		return false
	}
	if s.at.Sub(self.first) < incidentStartupRamp {
		return false
	}
	oldest := self.samples[0].at
	if s.at.Sub(oldest) < incidentMinHistory {
		return false
	}
	floor := s.goroutines
	for _, old := range self.samples {
		floor = min(floor, old.goroutines)
	}
	return float64(s.goroutines) >= incidentGrowthRatio*float64(floor)
}

func incidentCaptureEnabled() bool {
	return os.Getenv("URNETWORK_INCIDENT_CAPTURE") != "0"
}

// runIncidentCapture samples every 10 seconds and captures on a build-up.
func runIncidentCapture(ctx context.Context) {
	if !incidentCaptureEnabled() {
		return
	}
	tracker := newIncidentTracker()
	ticker := time.NewTicker(incidentSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sample := incidentSample{at: time.Now(), goroutines: runtime.NumGoroutine(), heapFrac: heapFracFn()}
		reason, ok := tracker.observe(sample)
		if !ok {
			continue
		}
		dir, err := captureIncident(sample.at, reason, sample)
		if err != nil {
			critLog("[incident] capture for %s failed: %v\n", reason, err)
			continue
		}
		critLog("[incident] %s: %d goroutines, heap at %.0f%% of its limit; profiles in %s\n", reason, sample.goroutines, sample.heapFrac*100, dir)
	}
}

var incidentCaptureMu sync.Mutex

// captureIncident writes the profiles into a new directory and prunes the old
// ones. It writes to disk only: the ramlog pipe may be the thing that is stuck.
func captureIncident(now time.Time, reason string, s incidentSample) (string, error) {
	incidentCaptureMu.Lock()
	defer incidentCaptureMu.Unlock()

	base, err := oomCapDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(base, "incidents")
	dir := filepath.Join(root, now.UTC().Format("20060102T150405Z")+"-"+reason)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	// The goroutine profile allocates a record per goroutine (tens of MiB at 60k)
	// and stops the world; past the heap limit the box cannot afford that, and
	// the growth and near-limit captures before it carry the same answer.
	goroutineProfile := "written"
	if reason == "heap-over-limit" {
		goroutineProfile = "skipped"
	} else {
		var goroutines strings.Builder
		if p := pprof.Lookup("goroutine"); p != nil {
			// a dropped write would leave an empty profile that reads as complete
			if err := p.WriteTo(&goroutines, 1); err != nil { // grouped by stack: small, and the counts are the point
				return dir, err
			}
		}
		if err := writeFileSynced(filepath.Join(dir, "goroutines.txt"), []byte(goroutines.String())); err != nil {
			return dir, err
		}
		if err := writeFileSynced(filepath.Join(dir, "summary.txt"), []byte(summarizeGoroutineProfile(goroutines.String(), 15))); err != nil {
			return dir, err
		}
	}

	heap, err := os.OpenFile(filepath.Join(dir, "heap.pb.gz"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return dir, err
	}
	if p := pprof.Lookup("heap"); p != nil {
		if err := p.WriteTo(heap, 0); err != nil {
			heap.Close()
			return dir, err
		}
	}
	if err := heap.Sync(); err != nil {
		heap.Close()
		return dir, err
	}
	if err := heap.Close(); err != nil {
		return dir, err
	}

	// runtime/metrics, not ReadMemStats: ReadMemStats stops the world, and a
	// second pause on top of the goroutine profile is exactly what a starved
	// process cannot afford.
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/in-use:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	value := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	meta := fmt.Sprintf("goroutine_profile="+goroutineProfile+"\ntime=%s\nreason=%s\ngoroutines=%d\nheap_frac=%.3f\nheap_objects_mib=%d\nheap_inuse_mib=%d\nsys_mib=%d\ngc_cycles=%d\nversion=%s\n",
		now.UTC().Format(time.RFC3339), reason, s.goroutines, s.heapFrac,
		value(0)>>20, value(1)>>20, value(2)>>20, value(3), Version)
	if err := writeFileSynced(filepath.Join(dir, "meta.txt"), []byte(meta)); err != nil {
		return dir, err
	}

	pruneIncidents(root, incidentKeep)
	return dir, nil
}

func writeFileSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// pruneIncidents keeps the newest keep directories (names sort by time).
func pruneIncidents(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > keep {
		_ = os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}

// summarizeGoroutineProfile ranks the stacks of a debug=1 goroutine profile by
// how many goroutines share them and prints the top n with their first frames,
// which is usually the whole answer to "what piled up".
func summarizeGoroutineProfile(profile string, n int) string {
	type group struct {
		count  int
		frames []string
	}
	var groups []group
	for _, block := range strings.Split(profile, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) == 0 {
			continue
		}
		// the first block also carries the "goroutine profile: total N" header
		var count int
		start := -1
		for i, l := range lines {
			if _, err := fmt.Sscanf(l, "%d @", &count); err == nil && count > 0 {
				start = i
				break
			}
		}
		if start < 0 {
			continue
		}
		g := group{count: count}
		for _, l := range lines[start+1:] {
			l = strings.TrimPrefix(l, "#")
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			g.frames = append(g.frames, l)
			if len(g.frames) == 4 {
				break
			}
		}
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].count > groups[j].count })
	if len(groups) > n {
		groups = groups[:n]
	}
	var b strings.Builder
	for _, g := range groups {
		fmt.Fprintf(&b, "%d goroutines\n", g.count)
		for _, f := range g.frames {
			fmt.Fprintf(&b, "    %s\n", f)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "no goroutine groups found\n"
	}
	return b.String()
}

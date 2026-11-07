package main

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// extender_tally.go counts what the extender has done for clients, over the
// last 24 hours, the last 7 days and its lifetime, and keeps the count across
// restarts and reboots.
//
// What is counted is requests served, not distinct clients. The serving core
// counts requests per service (extender.ExtenderServer.ServiceCounts); nothing
// records who asked, and this does not start: remembering client addresses
// would be a privacy cost the tally does not need. The three services that are
// clients being helped are `proxy` and `forward` (traffic relayed for a client)
// and `feed` (the directory a client bootstraps from). `probe` is the
// platform's own verification of this extender, and `gossip` and `other` are
// not client traffic, so none of those count.

const (
	extenderTallyFileName = "extender_tally.json"
	// buckets are hours; an hour is fine enough for "last 24h" to be honest
	// and coarse enough that a week is 168 numbers
	extenderTallyBucket = time.Hour
	// hours older than this are folded into the lifetime and dropped
	extenderTallyKeep = 8 * 24 * time.Hour
)

// extenderTallyWindows is what the status and the health line print.
type extenderTallyWindows struct {
	Last24h  uint64 `json:"last_24h"`
	Last7d   uint64 `json:"last_7d"`
	Lifetime uint64 `json:"lifetime"`
}

type extenderTally struct {
	mu   sync.Mutex
	path string
	now  func() time.Time

	// requests per hour, keyed by the hour's unix start
	hours    map[int64]uint64
	lifetime uint64

	// the cumulative count last read from the running role, in this process
	last    uint64
	haveRef bool
	dirty   bool
}

type extenderTallyFile struct {
	Hours    map[int64]uint64 `json:"hours"`
	Lifetime uint64           `json:"lifetime"`
}

func newExtenderTally(path string) *extenderTally {
	return &extenderTally{path: path, now: time.Now, hours: map[int64]uint64{}}
}

// extenderRequestsServed is the cumulative requests served for clients out of
// one role's service counts.
func extenderRequestsServed(serviceCounts map[string]uint64) uint64 {
	return serviceCounts["proxy"] + serviceCounts["forward"] + serviceCounts["feed"]
}

// Observe folds a cumulative reading into the buckets. The first reading of a
// role is its baseline: whatever it already counted is attributed to now. A
// reading lower than the last means the counters restarted, so it is a new
// baseline and counts in full.
func (t *extenderTally) Observe(total uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delta := total
	if t.haveRef && t.last <= total {
		delta = total - t.last
	}
	t.last = total
	t.haveRef = true
	if delta == 0 {
		return
	}
	hour := t.now().Truncate(extenderTallyBucket).Unix()
	t.hours[hour] += delta
	t.lifetime += delta
	t.dirty = true
	t.pruneLocked()
}

// Rebase forgets the last reading: the role was replaced and its counters
// start from zero again.
func (t *extenderTally) Rebase() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = 0
	t.haveRef = false
}

func (t *extenderTally) pruneLocked() {
	cutoff := t.now().Add(-extenderTallyKeep).Unix()
	for hour := range t.hours {
		if hour < cutoff {
			delete(t.hours, hour)
			t.dirty = true
		}
	}
}

func (t *extenderTally) bucketCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.hours)
}

// Windows reports the three totals as of now.
func (t *extenderTally) Windows() extenderTallyWindows {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	since24 := now.Add(-24 * time.Hour).Truncate(extenderTallyBucket).Unix()
	since7 := now.Add(-7 * 24 * time.Hour).Truncate(extenderTallyBucket).Unix()
	w := extenderTallyWindows{Lifetime: t.lifetime}
	for hour, n := range t.hours {
		if since24 < hour {
			w.Last24h += n
		}
		if since7 < hour {
			w.Last7d += n
		}
	}
	return w
}

// Load reads the saved tally. A missing file is a fresh start; a damaged one is
// an error and leaves the tally empty, and the next save replaces it.
func (t *extenderTally) Load() error {
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file extenderTallyFile
	if err := json.Unmarshal(b, &file); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if file.Hours != nil {
		t.hours = file.Hours
	}
	t.lifetime = file.Lifetime
	return nil
}

// Save writes the tally if it changed since the last save.
func (t *extenderTally) Save() error {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	file := extenderTallyFile{Hours: make(map[int64]uint64, len(t.hours)), Lifetime: t.lifetime}
	for hour, n := range t.hours {
		file.Hours[hour] = n
	}
	t.mu.Unlock()
	b, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(t.path, b, 0600); err != nil {
		return err
	}
	t.mu.Lock()
	t.dirty = false
	t.mu.Unlock()
	return nil
}

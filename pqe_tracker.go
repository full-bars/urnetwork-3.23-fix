package connect

import (
	"sync"
	"time"
)

// PQETracker records per-peer e2e session open/close events by key-exchange
// family (post-quantum vs classical) and reports them as lifetime and rolling
// window counts. Feed it from the encryption session manager on every session
// establish and reap; a periodic [pqe] line samples it.
//
// "PQE session" means a per-peer TLS session that negotiated a hybrid
// post-quantum key exchange (X25519MLKEM768 or another ML-KEM hybrid). These
// counters are about *session establishment events* in a window, not
// concurrently-active sessions (the live active counts cover that separately).
//
// Windows are sliding time buckets evaluated against each event's open time, so
// a session that started 3h ago counts in the "last 24h" and "lifetime" buckets
// but not "last 1h".
type PQETracker struct {
	mu sync.Mutex
	// opens/rooms of pqe + classical session-open timestamps.
	pqeOpens     []time.Time
	clasOpens    []time.Time
	pqeActive    int
	clasActive   int
	pqeLifetime  int64
	clasLifetime int64
}

// newPQETracker returns an empty tracker.
func newPQETracker() *PQETracker {
	return &PQETracker{}
}

// trim drops opens older than maxAge (e.g. 8 days) so memory stays bounded
// while lifetime is tracked separately. Called with mu held.
func (t *PQETracker) trim(now time.Time, maxAge time.Duration) {
	cut := now.Add(-maxAge)
	i := 0
	for i < len(t.pqeOpens) && t.pqeOpens[i].Before(cut) {
		i++
	}
	t.pqeOpens = t.pqeOpens[i:]
	j := 0
	for j < len(t.clasOpens) && t.clasOpens[j].Before(cut) {
		j++
	}
	t.clasOpens = t.clasOpens[j:]
}

// NoteOpen records an e2e session established. pqe true = post-quantum.
func (t *PQETracker) NoteOpen(pqe bool) {
	t.mu.Lock()
	now := time.Now()
	if pqe {
		t.pqeOpens = append(t.pqeOpens, now)
		t.pqeActive++
		t.pqeLifetime++
	} else {
		t.clasOpens = append(t.clasOpens, now)
		t.clasActive++
		t.clasLifetime++
	}
	t.trim(now, 8*24*time.Hour)
	t.mu.Unlock()
}

// NoteClose records an e2e session torn down. pqe true = post-quantum.
func (t *PQETracker) NoteClose(pqe bool) {
	t.mu.Lock()
	if pqe {
		if t.pqeActive > 0 {
			t.pqeActive--
		}
	} else if t.clasActive > 0 {
		t.clasActive--
	}
	t.mu.Unlock()
}

// Counts is a snapshot for the [pqe] log line.
type PQECounts struct {
	ActivePQE    int64
	ActiveClas   int64
	PQEHour      int64
	PQEDay       int64
	PQEWeek      int64
	PQELifetime  int64
	ClasHour     int64
	ClasDay      int64
	ClasWeek     int64
	ClasLifetime int64
}

// Snapshot returns windowed + lifetime counts as of now.
func (t *PQETracker) Snapshot() PQECounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	hourCut := now.Add(-1 * time.Hour)
	dayCut := now.Add(-24 * time.Hour)
	weekCut := now.Add(-7 * 24 * time.Hour)
	c := PQECounts{
		ActivePQE: int64(t.pqeActive), ActiveClas: int64(t.clasActive),
		PQELifetime: t.pqeLifetime, ClasLifetime: t.clasLifetime,
	}
	count := func(slice []time.Time, cut time.Time) int64 {
		var n int64
		for _, ts := range slice {
			if ts.After(cut) {
				n++
			}
		}
		return n
	}
	c.PQEHour = count(t.pqeOpens, hourCut)
	c.PQEDay = count(t.pqeOpens, dayCut)
	c.PQEWeek = count(t.pqeOpens, weekCut)
	c.ClasHour = count(t.clasOpens, hourCut)
	c.ClasDay = count(t.clasOpens, dayCut)
	c.ClasWeek = count(t.clasOpens, weekCut)
	return c
}

package connect

import (
	"crypto/tls"
	"sort"
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
//
// The tracker also records the handshake stages that lead to a session: an
// attempt (a handshake epoch started), a TLS establishment (handshake
// completed, negotiated curve known), and a verification (peer identity proof
// passed, which is what promotes the session to "up"). A failing TLS handshake
// shows up as attempts minus established, an identity failure as a separate
// count, and the per-curve histogram shows which group peers actually
// negotiated — the only way to see a fallback to a classical curve, since a
// single boolean cannot distinguish "no classical sessions" from "nothing was
// ever measured".
type PQETracker struct {
	mu sync.Mutex
	// opens/rooms of pqe + classical session-open timestamps.
	pqeOpens     []time.Time
	clasOpens    []time.Time
	pqeActive    int
	clasActive   int
	pqeLifetime  int64
	clasLifetime int64

	// Handshake stages. Attempts and established are per handshake epoch (a
	// rekey counts again); verified is per session (first verified epoch
	// only), so verified is comparable with the open counters.
	attempts       int64
	established    int64
	verified       int64
	identityFailed int64

	// curveOpens is the lifetime histogram of negotiated TLS curves at
	// establishment, keyed by curve name ("X25519MLKEM768", "X25519", "none").
	curveOpens map[string]int64
}

// newPQETracker returns an empty tracker.
func newPQETracker() *PQETracker {
	return &PQETracker{curveOpens: map[string]int64{}}
}

// curveName is the histogram label for a negotiated TLS curve. Curve 0 means
// the handshake reported no key-exchange group (synthetic/test epochs), which
// is a distinct outcome from a classical group and must not be folded into it.
func curveName(curve tls.CurveID) string {
	if curve == 0 {
		return "none"
	}
	return curve.String()
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

// NoteAttempt records a handshake epoch starting.
func (t *PQETracker) NoteAttempt() {
	t.mu.Lock()
	t.attempts++
	t.mu.Unlock()
}

// NoteEstablished records a completed TLS handshake and the curve it
// negotiated. Per handshake, not per session: a rekey counts again.
func (t *PQETracker) NoteEstablished(curve tls.CurveID) {
	t.mu.Lock()
	t.established++
	t.curveOpens[curveName(curve)]++
	t.mu.Unlock()
}

// NoteIdentityFailed records a session whose handshake completed but whose
// peer identity proof did not verify.
func (t *PQETracker) NoteIdentityFailed() {
	t.mu.Lock()
	t.identityFailed++
	t.mu.Unlock()
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
	t.verified++
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

// CurveCount is one row of the negotiated-curve histogram.
type CurveCount struct {
	Curve string
	Count int64
}

// PQECounts is a snapshot for the [pqe] log line.
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

	// Handshake stages, lifetime. Attempts-Established is failed TLS
	// handshakes; IdentityFailed counts completed handshakes whose peer
	// identity proof did not verify.
	Attempts       int64
	Established    int64
	Verified       int64
	IdentityFailed int64
	// Curves is the lifetime negotiated-curve histogram, highest count first.
	Curves []CurveCount
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
		Attempts: t.attempts, Established: t.established,
		Verified: t.verified, IdentityFailed: t.identityFailed,
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
	if 0 < len(t.curveOpens) {
		c.Curves = make([]CurveCount, 0, len(t.curveOpens))
		for name, n := range t.curveOpens {
			c.Curves = append(c.Curves, CurveCount{Curve: name, Count: n})
		}
		sort.Slice(c.Curves, func(i, j int) bool {
			if c.Curves[i].Count != c.Curves[j].Count {
				return c.Curves[i].Count > c.Curves[j].Count
			}
			return c.Curves[i].Curve < c.Curves[j].Curve
		})
	}
	return c
}

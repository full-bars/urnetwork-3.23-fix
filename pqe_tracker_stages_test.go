package connect

import (
	"crypto/tls"
	"testing"
)

// TestPQETrackerStagesAndCurves covers the handshake-stage counters and the
// negotiated-curve histogram, including the "none" bucket for a handshake that
// reported no group and the tie-break ordering of the histogram rows.
func TestPQETrackerStagesAndCurves(t *testing.T) {
	tr := newPQETracker()

	// A fresh tracker must report an empty histogram, not a nil row, so the
	// log line renders "curves: " without panicking.
	if c := tr.Snapshot(); len(c.Curves) != 0 || c.Attempts != 0 || c.Established != 0 || c.Verified != 0 {
		t.Fatalf("fresh tracker counts: %+v", c)
	}

	tr.NoteAttempt()
	tr.NoteAttempt()
	tr.NoteAttempt()
	tr.NoteEstablished(tls.X25519MLKEM768)
	tr.NoteEstablished(tls.X25519)
	tr.NoteEstablished(0) // handshake reported no group

	c := tr.Snapshot()
	if c.Attempts != 3 {
		t.Fatalf("attempts=%d want 3", c.Attempts)
	}
	if c.Established != 3 {
		t.Fatalf("established=%d want 3", c.Established)
	}
	if c.Verified != 0 {
		t.Fatalf("verified=%d want 0 before any identity proof", c.Verified)
	}

	// One session verifies per family; NoteOpen is the verification hook.
	tr.NoteOpen(true)
	tr.NoteOpen(false)
	c = tr.Snapshot()
	if c.Verified != 2 {
		t.Fatalf("verified=%d want 2", c.Verified)
	}
	if c.PQELifetime != 1 || c.ClasLifetime != 1 {
		t.Fatalf("lifetime pqe=%d clas=%d want 1/1", c.PQELifetime, c.ClasLifetime)
	}

	// Three curves, each seen once: the histogram sorts by count desc then
	// name asc, so the labels are deterministic across runs.
	want := []string{"X25519", "X25519MLKEM768", "none"}
	if len(c.Curves) != len(want) {
		t.Fatalf("curves=%v want %d rows", c.Curves, len(want))
	}
	for i, name := range want {
		if c.Curves[i].Curve != name || c.Curves[i].Count != 1 {
			t.Fatalf("curve row %d = %+v want %s=1 (rows %v)", i, c.Curves[i], name, c.Curves)
		}
	}

	// A repeated curve accumulates instead of adding a row, and a busy curve
	// outranks the lexicographic tie-break.
	tr.NoteEstablished(tls.X25519)
	tr.NoteEstablished(tls.X25519)
	c = tr.Snapshot()
	if len(c.Curves) != 3 {
		t.Fatalf("curves=%v want 3 rows after repeats", c.Curves)
	}
	if c.Curves[0].Curve != "X25519" || c.Curves[0].Count != 3 {
		t.Fatalf("top curve = %+v want X25519=3", c.Curves[0])
	}

	// Identity failures are their own bucket: they must not be folded into
	// either family, because the session never reached "up".
	tr.NoteIdentityFailed()
	tr.NoteIdentityFailed()
	c = tr.Snapshot()
	if c.IdentityFailed != 2 {
		t.Fatalf("identity_failed=%d want 2", c.IdentityFailed)
	}
	if c.Verified != 2 || c.PQELifetime != 1 || c.ClasLifetime != 1 {
		t.Fatalf("identity failure changed session counts: %+v", c)
	}
}

// TestCurveNameDistinguishesNoneFromClassical pins the label mapping: curve 0
// means "no group reported", which must not be reported as a classical group.
func TestCurveNameDistinguishesNoneFromClassical(t *testing.T) {
	if got := curveName(0); got != "none" {
		t.Fatalf("curveName(0)=%q want none", got)
	}
	if got := curveName(tls.X25519); got != tls.X25519.String() {
		t.Fatalf("curveName(X25519)=%q want %q", got, tls.X25519.String())
	}
	if got := curveName(tls.X25519MLKEM768); got != tls.X25519MLKEM768.String() {
		t.Fatalf("curveName(hybrid)=%q want %q", got, tls.X25519MLKEM768.String())
	}
	if curveName(tls.X25519) == curveName(0) {
		t.Fatal("none and classical must not share a label")
	}
}

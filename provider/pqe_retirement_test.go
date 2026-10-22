package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// resetPQEGlobals clears the registry and the retired accumulator so a test
// starts from a known state. Serial by construction: these are package-level
// globals shared with the live provider.
func resetPQEGlobals(t *testing.T) {
	t.Helper()
	encryptionManagers.mu.Lock()
	encryptionManagers.set = nil
	encryptionManagers.mu.Unlock()
	pqeRetired.mu.Lock()
	pqeRetired.counts = connect.PQECounts{}
	pqeRetired.curves = map[string]int64{}
	pqeRetired.mu.Unlock()
}

// TestPQETotalCountsKeepsRetiredLifetime pins the counter contract: a proxy's
// manager disappears when it exits, but its lifetime counts must survive, or
// the exported _total series decreases and rate() reads a reset.
func TestPQETotalCountsKeepsRetiredLifetime(t *testing.T) {
	resetPQEGlobals(t)
	t.Cleanup(func() { resetPQEGlobals(t) })

	retirePQE(connect.PQECounts{
		PQELifetime: 5, ClasLifetime: 1,
		Attempts: 9, Established: 7, Verified: 6, IdentityFailed: 1,
		Curves: []connect.CurveCount{{Curve: "X25519", Count: 7}},
	})

	c := pqeTotalCounts()
	if c.PQELifetime != 5 || c.ClasLifetime != 1 {
		t.Fatalf("lifetime pqe=%d clas=%d want 5/1", c.PQELifetime, c.ClasLifetime)
	}
	if c.Attempts != 9 || c.Established != 7 || c.Verified != 6 || c.IdentityFailed != 1 {
		t.Fatalf("stages attempts=%d ok=%d verified=%d failed=%d want 9/7/6/1",
			c.Attempts, c.Established, c.Verified, c.IdentityFailed)
	}
	if len(c.Curves) != 1 || c.Curves[0].Curve != "X25519" || c.Curves[0].Count != 7 {
		t.Fatalf("curves=%v want [X25519=7]", c.Curves)
	}

	// Retirement must not fabricate live state: active and windowed counts
	// describe sessions that are still running.
	if c.ActivePQE != 0 || c.ActiveClas != 0 || c.PQEHour != 0 || c.ClasHour != 0 || c.PQEDay != 0 {
		t.Fatalf("retirement leaked into live gauges: %+v", c)
	}
}

// TestPQETotalCountsMergesRepeatedRetirement covers a second proxy exiting with
// the same curve: rows merge instead of duplicating, and totals add.
func TestPQETotalCountsMergesRepeatedRetirement(t *testing.T) {
	resetPQEGlobals(t)
	t.Cleanup(func() { resetPQEGlobals(t) })

	retirePQE(connect.PQECounts{
		PQELifetime: 2, Attempts: 3,
		Curves: []connect.CurveCount{{Curve: "X25519", Count: 2}, {Curve: "X25519MLKEM768", Count: 1}},
	})
	retirePQE(connect.PQECounts{
		PQELifetime: 4, Attempts: 5,
		Curves: []connect.CurveCount{{Curve: "X25519", Count: 4}},
	})

	c := pqeTotalCounts()
	if c.PQELifetime != 6 || c.Attempts != 8 {
		t.Fatalf("totals pqe=%d attempts=%d want 6/8", c.PQELifetime, c.Attempts)
	}
	if len(c.Curves) != 2 {
		t.Fatalf("curves=%v want 2 rows", c.Curves)
	}
	// Sorted by count desc: X25519=6 then X25519MLKEM768=1.
	if c.Curves[0].Curve != "X25519" || c.Curves[0].Count != 6 {
		t.Fatalf("top row=%+v want X25519=6", c.Curves[0])
	}
	if c.Curves[1].Curve != "X25519MLKEM768" || c.Curves[1].Count != 1 {
		t.Fatalf("second row=%+v want X25519MLKEM768=1", c.Curves[1])
	}
}

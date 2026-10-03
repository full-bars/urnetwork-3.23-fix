package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// During a HotSwap the parent keeps draining after the candidate has written
// its own run.marker. The parent's hourly heartbeat used to read the marker
// outside the lock and write it back, so it could land on top of the
// candidate's fresh marker with the parent's older OOMKills baseline and a peak
// counted from the parent's draining pool; the next start then counted a kill
// the candidate had already acted on a second time. A marker this process did
// not write must be left exactly as it is.
func TestOOMCapUpdatePeakLeavesAnotherStartsMarkerAlone(t *testing.T) {
	withTempHome(t)
	oomCapRecordStart(2000, "boot-A", 0, oomT0) // the parent's own start
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.marker")

	// The candidate promotes itself: its marker, written by another process.
	candidate := oomMarker{BootID: "boot-A", OOMKills: 3, Proxies: 7, StartedUnix: oomT0.Add(time.Minute).Unix(), LastSeenUnix: oomT0.Add(time.Minute).Unix()}
	if err := oomWriteJSON(path, candidate); err != nil {
		t.Fatal(err)
	}

	// The draining parent's heartbeat and a peak from its large pool.
	oomCapUpdatePeak(3800, oomT0.Add(2*time.Hour))

	var got oomMarker
	if !oomReadJSON(path, &got) {
		t.Fatal("marker unreadable")
	}
	if got != candidate {
		t.Fatalf("the parent changed the candidate's marker:\n got  %+v\n want %+v", got, candidate)
	}
}

// The same process still refreshes and raises the marker it wrote.
func TestOOMCapUpdatePeakStillUpdatesItsOwnMarker(t *testing.T) {
	withTempHome(t)
	oomCapRecordStart(2000, "boot-A", 0, oomT0)
	oomCapUpdatePeak(3800, oomT0.Add(2*time.Hour))

	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	var got oomMarker
	if !oomReadJSON(filepath.Join(dir, "run.marker"), &got) {
		t.Fatal("marker unreadable")
	}
	if got.Proxies != 3800 || got.LastSeenUnix != oomT0.Add(2*time.Hour).Unix() {
		t.Fatalf("own marker must carry the peak and the heartbeat, got %+v", got)
	}
}

// A process with no recorded start of its own owns no marker. When its own
// marker write failed, the file on disk belongs to some other start, and
// refreshing it would vouch for a process that is not this one.
func TestOOMCapUpdatePeakRefusesAMarkerWhenItRecordedNoStart(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.marker")
	other := oomMarker{BootID: "boot-A", OOMKills: 3, Proxies: 7, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix(), StartID: "other-start"}
	if err := oomWriteJSON(path, other); err != nil {
		t.Fatal(err)
	}

	oomCapUpdatePeak(3800, oomT0.Add(2*time.Hour))

	var got oomMarker
	if !oomReadJSON(path, &got) {
		t.Fatal("marker unreadable")
	}
	if got != other {
		t.Fatalf("a process with no recorded start changed another start's marker:\n got  %+v\n want %+v", got, other)
	}
}

// StartedUnix has one second of resolution, so a parent and a candidate that
// record their starts in the same second share it. The marker must still be
// told apart by the per-start token.
func TestOOMCapUpdatePeakTellsTwoStartsInTheSameSecondApart(t *testing.T) {
	withTempHome(t)
	oomCapRecordStart(2000, "boot-A", 0, oomT0) // the parent
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.marker")
	var parent oomMarker
	if !oomReadJSON(path, &parent) || parent.StartID == "" {
		t.Fatalf("a start must write its token, got %+v", parent)
	}

	// The candidate records its start in the same second, from another process.
	candidate := oomMarker{BootID: "boot-A", OOMKills: 3, Proxies: 7, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix(), StartID: "candidate-start"}
	if err := oomWriteJSON(path, candidate); err != nil {
		t.Fatal(err)
	}

	oomCapUpdatePeak(3800, oomT0.Add(2*time.Hour))

	var got oomMarker
	if !oomReadJSON(path, &got) {
		t.Fatal("marker unreadable")
	}
	if got != candidate {
		t.Fatalf("the parent changed a same-second candidate's marker:\n got  %+v\n want %+v", got, candidate)
	}
}

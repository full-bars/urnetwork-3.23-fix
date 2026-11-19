package urnettools

import (
	"testing"
	"time"
)

func statusGoldenProvider() Provider {
	return Provider{
		User:     "urnet",
		Unit:     "urnetwork.service",
		Binary:   "/usr/local/bin/urnetwork",
		Version:  "v3.23.0-fix.35.11",
		StateDir: "/home/urnet/.urnetwork",
		Running:  true,
	}
}

func goldenUpdate() statusUpdateState {
	return statusUpdateState{Tag: "v3.23.0-fix.35.11", At: time.Now(), Unavailable: false}
}

func TestStatusGoldenHealthy(t *testing.T) {
	origOpts := statusLiveOptsFn
	statusLiveOptsFn = func() liveOpts { return liveOpts{Color: false, ASCII: true, Width: 80} }
	defer func() { statusLiveOptsFn = origOpts }()

	snap := loadSnapshotFixture(t, "node_snapshot_v1.json")
	snap.Proxies.Degraded = 0
	p := statusGoldenProvider()
	d := statusData{
		Provider: p,
		Snapshot: snap,
		Rows:     makeStatusRows(p, snap, ExtenderReading{Known: true, Running: true}, nil),
		Update:   goldenUpdate(),
		ExitCode: 0,
	}
	out, err := formatStatusSummary(d, true)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "status/healthy", out)
}

func TestStatusGoldenWarning(t *testing.T) {
	origOpts := statusLiveOptsFn
	statusLiveOptsFn = func() liveOpts { return liveOpts{Color: false, ASCII: true, Width: 80} }
	defer func() { statusLiveOptsFn = origOpts }()

	snap := loadSnapshotFixture(t, "node_snapshot_v1.json")
	snap.State = "degraded"
	snap.Proxies.Degraded = 2
	p := statusGoldenProvider()
	d := statusData{
		Provider: p,
		Snapshot: snap,
		Rows:     makeStatusRows(p, snap, ExtenderReading{Known: true, Activating: true}, nil),
		Update:   goldenUpdate(),
		ExitCode: 1,
	}
	out, err := formatStatusSummary(d, true)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "status/warning", out)
}

func TestStatusGoldenBroken(t *testing.T) {
	origOpts := statusLiveOptsFn
	statusLiveOptsFn = func() liveOpts { return liveOpts{Color: false, ASCII: true, Width: 80} }
	defer func() { statusLiveOptsFn = origOpts }()

	snap := loadSnapshotFixture(t, "node_snapshot_v1.json")
	snap.Proxies.Dead = 3
	p := statusGoldenProvider()
	p.Running = false
	d := statusData{
		Provider: p,
		Snapshot: snap,
		Rows:     makeStatusRows(p, snap, ExtenderReading{Known: true, Running: true}, nil),
		Update:   goldenUpdate(),
		ExitCode: 2,
	}
	out, err := formatStatusSummary(d, true)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "status/broken", out)
}

func TestStatusGoldenUnreachable(t *testing.T) {
	origOpts := statusLiveOptsFn
	statusLiveOptsFn = func() liveOpts { return liveOpts{Color: false, ASCII: true, Width: 80} }
	defer func() { statusLiveOptsFn = origOpts }()

	p := statusGoldenProvider()
	d := statusData{
		Provider: p,
		Snapshot: nil,
		Rows:     makeStatusRows(p, nil, ExtenderReading{}, nil),
		Update:   statusUpdateState{Unavailable: true},
		ExitCode: 3,
	}
	out, err := formatStatusSummary(d, true)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "status/unreachable", out)
}

func TestStatusGoldenNarrowTerminal(t *testing.T) {
	origOpts := statusLiveOptsFn
	statusLiveOptsFn = func() liveOpts { return liveOpts{Color: false, ASCII: true, Width: 40} }
	defer func() { statusLiveOptsFn = origOpts }()

	snap := loadSnapshotFixture(t, "node_snapshot_v1.json")
	snap.Proxies.Degraded = 2
	p := statusGoldenProvider()
	d := statusData{
		Provider: p,
		Snapshot: snap,
		Rows:     makeStatusRows(p, snap, ExtenderReading{Known: true, Running: true}, nil),
		Update:   goldenUpdate(),
		ExitCode: 1,
	}
	out, err := formatStatusSummary(d, true)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "status/narrow_40", out)
}

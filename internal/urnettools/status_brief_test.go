package urnettools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// statusDataFor builds a statusData with preset rows for exit-code tests.
func statusDataFor(rows []statusRow, snap *NodeSnapshot, version string) statusData {
	return statusData{Provider: Provider{Version: version}, Snapshot: snap, Rows: rows}
}

func TestStatusExitCode(t *testing.T) {
	live := &NodeSnapshot{State: "flowing", Version: "v1"}
	cases := []struct {
		name string
		d    statusData
		want int
	}{
		{
			name: "healthy",
			d: statusDataFor([]statusRow{
				{"Version", statusOK, "v1"},
				{"Health", statusOK, "flowing"},
			}, live, "v1"),
			want: 0,
		},
		{
			name: "warning",
			d: statusDataFor([]statusRow{
				{"Version", statusOK, "v1"},
				{"Health", statusWarn, "degraded"},
			}, live, "v1"),
			want: 1,
		},
		{
			name: "broken",
			d: statusDataFor([]statusRow{
				{"Version", statusOK, "v1"},
				{"Health", statusBad, "provider stopped"},
			}, live, "v1"),
			want: 2,
		},
		{
			name: "unreachable",
			d: statusDataFor([]statusRow{
				{"Version", statusUnknown, "unknown"},
				{"Health", statusUnknown, "live status unavailable"},
			}, nil, "v1"),
			want: 3,
		},
	}
	for _, c := range cases {
		if got := statusExitCode(c.d); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestStatusBriefLine(t *testing.T) {
	d := statusData{
		Provider: Provider{Version: "v1"},
		Snapshot: &NodeSnapshot{State: "flowing", Version: "v1"},
		Rows: []statusRow{
			{"Version", statusOK, "v1"},
			{"Health", statusOK, "flowing"},
			{"Proxies", statusOK, "10 up, 2 degraded, 0 connecting, 0 dead"},
			{"Extender", statusOK, "active"},
		},
	}
	got := statusBriefLine(d)
	want := "ok v1 10/12 proxies extender active"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("brief line must be a single clean line: %q", got)
	}
}

func TestStatusBriefLineStates(t *testing.T) {
	cases := []struct {
		name string
		d    statusData
		want string
	}{
		{
			name: "unreachable",
			d: statusData{
				Provider: Provider{Version: "v1"},
				Snapshot: nil,
				Rows: []statusRow{
					{"Version", statusUnknown, "unknown"},
					{"Health", statusUnknown, "live status unavailable"},
					{"Proxies", statusUnknown, "unavailable"},
					{"Extender", statusUnknown, "unavailable"},
				},
			},
			want: "unreachable v1 0/0 proxies extender unavailable",
		},
		{
			name: "broken",
			d: statusData{
				Provider: Provider{Version: "v1"},
				Snapshot: &NodeSnapshot{State: "stopped"},
				Rows: []statusRow{
					{"Health", statusBad, "provider stopped"},
					{"Proxies", statusOK, "0 up, 0 degraded, 0 connecting, 0 dead"},
					{"Extender", statusUnknown, "unavailable"},
				},
			},
			want: "bad v1 0/0 proxies extender unavailable",
		},
	}
	for _, c := range cases {
		if got := statusBriefLine(c.d); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStatusBriefExitCodeWiring(t *testing.T) {
	p := Provider{User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"": fixtureRaw(t, "node_snapshot_v1.json")}, true)
	var err error
	out := captureStdout(t, func() {
		err = cmdStatus([]string{"--brief"})
	})
	if !strings.Contains(out, "warn ") || !strings.Contains(out, "proxies") {
		t.Fatalf("brief output = %q", out)
	}
	var be briefExitError
	if !errors.As(err, &be) {
		t.Fatalf("want a briefExitError, got %T (%v)", err, err)
	}
	// The fixture has 2 degraded proxies, which is a warning, not an error.
	if be.ExitCode() != 1 {
		t.Fatalf("warning provider must exit 1, got %d", be.ExitCode())
	}
}

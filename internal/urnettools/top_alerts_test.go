package urnettools

import (
	"testing"
	"time"
)

func TestTopAlerts(t *testing.T) {
	fd, limit := 71, 100
	mem := uint64(100)
	threads, tasksMax := uint64(71), uint64(100)
	swapUsed, swapTotal := uint64(300), uint64(1000)
	cases := []struct {
		name, key string
		snap      *NodeSnapshot
		ext       ExtenderReading
		in        *NodeInternals
	}{
		{name: "restart", key: "restart", snap: &NodeSnapshot{RestartPending: true}},
		{name: "degraded", key: "degraded", snap: &NodeSnapshot{State: "degraded"}},
		{name: "fds", key: "fds", snap: &NodeSnapshot{Resources: SnapshotResources{OpenFDs: &fd, FDLimit: &limit}}},
		{name: "memory", key: "memory", snap: &NodeSnapshot{Resources: SnapshotResources{HeapInuseBytes: 101, MemLimitBytes: &mem}}},
		{name: "proxies", key: "proxies", snap: &NodeSnapshot{Proxies: SnapshotProxies{Configured: 10, Up: 4}}},
		{name: "threads", key: "threads", snap: &NodeSnapshot{Resources: SnapshotResources{Threads: &threads, TasksMax: &tasksMax}}},
		{name: "swap", key: "swap", snap: &NodeSnapshot{Resources: SnapshotResources{SwapUsedBytes: &swapUsed, SwapTotalBytes: &swapTotal}}},
		{name: "extender", key: "extender-listen", ext: ExtenderReading{Source: SourceControlSocket, Known: true, Setting: "on"}},
		{name: "route", key: "extender-route", ext: ExtenderReading{Source: SourceControlSocket, Known: true, Running: true, BindRoute: "neither"}},
		{name: "refusing", key: "extender-refusing", ext: ExtenderReading{Source: SourceControlSocket, Known: true, RoleState: "tcp_unavailable"}},
		{name: "sigpipe", key: "sigpipe", in: &NodeInternals{SigpipeRisk: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alerts := topAlerts(tc.snap, tc.ext, tc.in, time.Time{})
			for _, a := range alerts {
				if a.key == tc.key && a.hint != "" {
					return
				}
			}
			t.Fatalf("missing alert %s: %+v", tc.key, alerts)
		})
	}
	if got := topAlerts(nil, ExtenderReading{}, nil, time.Time{}); len(got) != 0 {
		t.Fatalf("unknown values produced alerts: %+v", got)
	}
	fd = 70
	if got := topAlerts(&NodeSnapshot{Resources: SnapshotResources{OpenFDs: &fd, FDLimit: &limit}}, ExtenderReading{}, nil, time.Time{}); len(got) != 0 {
		t.Fatalf("70%% is not above 70%%: %+v", got)
	}
	threads = 70
	if got := topAlerts(&NodeSnapshot{Resources: SnapshotResources{Threads: &threads, TasksMax: &tasksMax}}, ExtenderReading{}, nil, time.Time{}); len(got) != 0 {
		t.Fatalf("70%% is not above 70%%: %+v", got)
	}
	swapUsed = 250
	if got := topAlerts(&NodeSnapshot{Resources: SnapshotResources{SwapUsedBytes: &swapUsed, SwapTotalBytes: &swapTotal}}, ExtenderReading{}, nil, time.Time{}); len(got) != 0 {
		t.Fatalf("25%% is not above 25%%: %+v", got)
	}
}

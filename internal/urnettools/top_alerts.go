package urnettools

import (
	"fmt"
	"github.com/urnetwork/connect/internal/tui"
	"sort"
	"time"
)

// topAlert has a stable key so the caller can dismiss an alert until it clears.
type topAlert struct {
	key   string
	level topLevel
	text  string
	hint  string
}

// topAlerts only evaluates reported values. An absent reading is not a fault.
func topAlerts(s *NodeSnapshot, e ExtenderReading, in *NodeInternals, now time.Time) []topAlert {
	var out []topAlert
	add := func(key string, level topLevel, text, hint string) {
		out = append(out, topAlert{key, level, text, hint})
	}
	if s != nil {
		if s.RestartPending {
			add("restart", topWarn, "Restart pending", "urnet-tools status")
		}
		if s.State == "degraded" {
			parked := ""
			if s.Proxies.Parked != nil {
				parked = fmt.Sprintf(" (%d parked)", *s.Proxies.Parked)
			}
			add("degraded", topWarn, "Node degraded"+parked, "urnet-tools status")
		}
		r := s.Resources
		if r.OpenFDs != nil && r.FDLimit != nil && *r.FDLimit > 0 && float64(*r.OpenFDs)/float64(*r.FDLimit) > .7 {
			add("fds", topWarn, "File descriptors above 70%", "urnet-tools status")
		}
		if r.MemLimitBytes != nil && *r.MemLimitBytes > 0 && r.HeapInuseBytes > *r.MemLimitBytes {
			add("memory", topWarn, "Heap exceeds Go memory limit", "urnet-tools status")
		}
		if r.Threads != nil && r.TasksMax != nil && *r.TasksMax > 0 && float64(*r.Threads)/float64(*r.TasksMax) > .7 {
			add("threads", topWarn, "Threads above 70% of the task cap", "urnet-tools status")
		}
		if r.SwapUsedBytes != nil && r.SwapTotalBytes != nil && *r.SwapTotalBytes > 0 && float64(*r.SwapUsedBytes)/float64(*r.SwapTotalBytes) > .25 {
			add("swap", topWarn, "Swap in use above 25%", "urnet-tools status")
		}
		if s.Proxies.Configured > 0 && float64(s.Proxies.Up)/float64(s.Proxies.Configured) < .5 {
			add("proxies", topWarn, "Fewer than half of configured proxies are up", "urnet-tools status")
		}
	}
	if e.Source == SourceControlSocket && e.Known {
		if e.Setting == "on" && !e.Running && !e.Activating {
			add("extender-listen", topBad, "Extender enabled but not running", "urnet-tools extender status")
		}
		if e.Running && e.BindRoute == "neither" {
			add("extender-route", topBad, "Extender port route missing", "urnet-tools extender status")
		}
		if e.RoleState == "tcp_unavailable" || e.RoleState == "unavailable" || e.RoleState == "error" {
			add("extender-refusing", topBad, "Extender refusing connections", "urnet-tools extender status")
		}
	}
	if in != nil && in.SigpipeRisk {
		add("sigpipe", topBad, "Unit not protected against SIGPIPE restarts", "urnet-tools update")
	}
	return out
}

func (m *topModel) alertRows() int {
	return min(len(topAlerts(m.snap, m.extender, m.rt.cur, m.now())), 2)
}

// drawAlertBanner draws into a caller-owned region; layout is wired separately.
func (m *topModel) drawAlertBanner(b *tui.Buffer) {
	alerts := topAlerts(m.snap, m.extender, m.rt.cur, m.now())
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].level > alerts[j].level })
	for i := 0; i < min(len(alerts), b.Height()); i++ {
		a := alerts[i]
		b.Put(fmt.Sprintf("%d/%d %s | %s", i+1, len(alerts), a.text, a.hint), 0, i, m.style(a.level))
	}
}

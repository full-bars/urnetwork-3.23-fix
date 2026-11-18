package urnettools

import (
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// sigpipeRestartRisk checks whether the provider's unit is not protected against
// SIGPIPE restarts, evaluated by the tool, cached for 60 s, never on the render path.
func sigpipeRestartRisk(p Provider, now time.Time) bool {
	if runtime.GOOS != "linux" || p.Unit == "" || p.Supervisor == "openrc" {
		return false
	}
	state, err := showSigpipeState(p, runSigpipeSystemctl)
	if err != nil {
		return false
	}
	return state.risk()
}

const sigpipeCacheTTL = 60 * time.Second

var (
	sigpipeCacheMu sync.Mutex
	sigpipeCache   = map[string]struct {
		at   time.Time
		risk bool
	}{}
)

// sigpipeRestartRiskCached returns the cached risk for a provider's unit.
func sigpipeRestartRiskCached(p Provider, now time.Time) bool {
	key := p.Unit + "::" + p.User
	sigpipeCacheMu.Lock()
	defer sigpipeCacheMu.Unlock()
	if c, ok := sigpipeCache[key]; ok && now.Sub(c.at) < sigpipeCacheTTL {
		return c.risk
	}
	risk := sigpipeRestartRisk(p, now)
	sigpipeCache[key] = struct {
		at   time.Time
		risk bool
	}{at: now, risk: risk}
	return risk
}

var _ = exec.Command

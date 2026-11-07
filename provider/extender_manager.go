package main

import (
	"context"
	"os"
	"sync"
	"time"
)

// extenderManager owns the extender role for the life of the process. The role
// itself (extender_role.go) is one running supervisor; the manager decides
// whether there should be one right now and starts or stops it to match:
//
//   - the operator's setting (`urnet-tools extender on|off`, on by default)
//   - whether the direct slot is enabled: only the direct slot's egress is this
//     host's own address, which is the address the carriers answer on
//
// It also holds the late-bound activation inputs, which only exist once the
// direct slot has authenticated, and hands them to whichever role is running.
// Every change of state is announced in the log with the reason, so there is
// never a silent "off".
type extenderManager struct {
	mu sync.Mutex

	ctx           context.Context
	environ       func(string) string
	directEnabled func() bool
	newConfig     func() extenderRoleConfig
	startRole     func(ctx context.Context, config extenderRoleConfig) (*extenderRole, error)

	role   *extenderRole
	inputs *extenderActivationInputs

	// the last reason the role is not running, so a periodic check does not
	// repeat a line that has not changed
	lastOffLine string
	// why the role is off, for status. Empty while it runs.
	offReason string

	// requests served for clients, kept across restarts (extender_tally.go)
	tally         *extenderTally
	tallyLoaded   bool
	tallyLastSave time.Time
}

var globalExtenderManager = newExtenderManager()

func newExtenderManager() *extenderManager {
	return &extenderManager{
		environ:       os.Getenv,
		directEnabled: isDirectEnabled,
		newConfig:     defaultExtenderRoleConfig,
		startRole:     startExtenderRole,
	}
}

// extenderManagerPollInterval is how often the manager re-reads whether direct
// providing is enabled, which `provider direct on|off` changes without telling
// the manager.
const extenderManagerPollInterval = 15 * time.Second

// Start binds the manager to the process and starts the role if it should run.
// It returns at once; the role binds its ports asynchronously. The role is
// closed when ctx ends, and wg is held until it has closed.
func (m *extenderManager) Start(ctx context.Context, wg *sync.WaitGroup) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Reconcile("startup")

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(extenderManagerPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				m.stop("the provider is shutting down")
				m.SaveTally()
				return
			case <-ticker.C:
				m.Reconcile("periodic check")
				m.sampleTally(m.Role())
			}
		}
	}()
}

// Reconcile starts or stops the role so it matches the setting and the direct
// slot. Safe to call from any goroutine, any number of times.
func (m *extenderManager) Reconcile(why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileLocked(why)
}

func (m *extenderManager) reconcileLocked(why string) {
	if m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	on, source := extenderSetting(m.environ)
	if bad := extenderUnrecognisedEnv(m.environ); bad != "" && m.role == nil {
		importantLogf("[extender] ignoring %s=%q: it is not on or off, so the default (on) applies. Use `urnet-tools extender off` to turn the extender off.\n", extenderEnv, bad)
	}
	direct := m.directEnabled()

	switch {
	case on && direct:
		m.offReason = ""
		m.lastOffLine = ""
		if m.role == nil {
			m.startLocked(why, source)
		}
	case m.role != nil:
		reason := m.notRunningReason(on, source, direct)
		m.stopLocked(reason)
	default:
		reason := m.notRunningReason(on, source, direct)
		m.offReason = reason
		line := "[extender] off: " + reason
		if line != m.lastOffLine {
			m.lastOffLine = line
			importantLogf("%s\n", line)
		}
	}
}

func (m *extenderManager) notRunningReason(on bool, source string, direct bool) string {
	if !on {
		return "turned off by " + source + ". Turn it back on with: urnet-tools extender on"
	}
	if !direct {
		return "direct providing is disabled, and the extender only runs on the direct slot. Turn direct back on with: provider direct on"
	}
	return "unknown"
}

func (m *extenderManager) startLocked(why string, source string) {
	importantLogf("[extender] starting (%s; setting from %s). It listens for censored clients on tcp 443, udp 443 and dns 4053, then asks the platform to list this host.\n", why, source)
	role, err := m.startRole(m.ctx, m.newConfig())
	if err != nil {
		m.offReason = "the role could not start: " + err.Error()
		line := "[extender] FAILED to start: " + err.Error() + ". The extender is not running. It will be retried in " + extenderManagerPollInterval.String() + "."
		if line != m.lastOffLine {
			m.lastOffLine = line
			importantLogf("%s\n", line)
		}
		return
	}
	m.role = role
	m.lastOffLine = ""
	// the new role's counters start from zero
	m.tallyRef().Rebase()
	if m.inputs != nil {
		role.StartActivation(m.inputs)
	}
}

func (m *extenderManager) stop(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(reason)
}

func (m *extenderManager) stopLocked(reason string) {
	if m.role == nil {
		return
	}
	role := m.role
	m.sampleTallyLocked(role)
	m.role = nil
	m.offReason = reason
	// the stop line carries the reason, so the periodic check must not repeat
	// it as an "off:" line
	m.lastOffLine = "[extender] off: " + reason
	importantLogf("[extender] stopping: %s\n", reason)
	// CloseAndWait joins the supervisor, which never takes the manager lock
	role.CloseAndWait()
	importantLogf("[extender] stopped. It no longer listens, and the platform will stop sending clients to this host once its record expires.\n")
}

// AttachActivation hands the manager what the activation needs from the direct
// slot: the api url, a getter for the live client jwt and a direct-only
// strategy. They exist only after the slot authenticates, which is after the
// role starts, so the activation is attached late. The activation stops when
// the slot's context ends (for example `provider direct off`).
func (m *extenderManager) AttachActivation(inputs *extenderActivationInputs) {
	m.mu.Lock()
	m.inputs = inputs
	role := m.role
	m.mu.Unlock()
	if role != nil {
		role.StartActivation(inputs)
	}
	go func() {
		<-inputs.ctx.Done()
		m.DetachActivation(inputs)
	}()
}

// DetachActivation drops the inputs of a slot that ended and stops the
// activation that used them.
func (m *extenderManager) DetachActivation(inputs *extenderActivationInputs) {
	m.mu.Lock()
	if m.inputs == inputs {
		m.inputs = nil
	}
	role := m.role
	m.mu.Unlock()
	if role != nil {
		role.StopActivation(inputs)
	}
}

// Role is the running role, nil when the extender is off.
func (m *extenderManager) Role() *extenderRole {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.role
}

// OffReason is why the role is not running, empty while it runs.
func (m *extenderManager) OffReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.role != nil {
		return ""
	}
	return m.offReason
}

// tallyRef returns the tally, loading it from disk the first time. Caller holds
// m.mu or is the only user.
func (m *extenderManager) tallyRef() *extenderTally {
	if m.tally == nil {
		path, err := providerStatePath(extenderTallyFileName)
		if err != nil {
			path = ""
		}
		m.tally = newExtenderTally(path)
	}
	if !m.tallyLoaded {
		m.tallyLoaded = true
		if m.tally.path != "" {
			if err := m.tally.Load(); err != nil {
				importantLogf("[extender] the saved request tally %s could not be read (%v); counting from zero. The file will be replaced.\n", m.tally.path, err)
			}
		}
	}
	return m.tally
}

// sampleTally folds the running role's request counters into the tally and
// saves it at most once a minute.
func (m *extenderManager) sampleTally(role *extenderRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sampleTallyLocked(role)
}

func (m *extenderManager) sampleTallyLocked(role *extenderRole) {
	tally := m.tallyRef()
	if role != nil {
		tally.Observe(extenderRequestsServed(role.snapshot().ServiceCounts))
	}
	if tally.path == "" || time.Since(m.tallyLastSave) < time.Minute {
		return
	}
	m.tallyLastSave = time.Now()
	if err := tally.Save(); err != nil {
		tlog("[extender] could not save the request tally to %s: %v\n", tally.path, err)
	}
}

func (m *extenderManager) tallyWindows() extenderTallyWindows {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tallyRef().Windows()
}

// SaveTally writes the tally now, for shutdown.
func (m *extenderManager) SaveTally() {
	m.mu.Lock()
	defer m.mu.Unlock()
	tally := m.tallyRef()
	if tally.path == "" {
		return
	}
	if err := tally.Save(); err != nil {
		tlog("[extender] could not save the request tally to %s: %v\n", tally.path, err)
	}
}

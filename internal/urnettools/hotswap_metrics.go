package urnettools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// hotswapDeclineReason maps a hotswap preflight/trigger error to a
// short metric label. The labels are intentionally terse because they
// become Prometheus label values.
func hotswapDeclineReason(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrHotSwapNotSupported):
		return "version_old"
	case errors.Is(err, ErrHotSwapUnitNotNotify):
		return "unit_not_notify"
	case errors.Is(err, ErrHotSwapLowMemory):
		return "low_memory"
	case errors.Is(err, ErrHotSwapNeedsRestart):
		return "needs_restart"
	case strings.Contains(err.Error(), "query systemd unit type"):
		return "unit_query_error"
	case strings.Contains(err.Error(), "not yet supported on Windows"):
		return "windows"
	case strings.Contains(err.Error(), "no valid PID"):
		return "trigger_pid"
	case strings.Contains(err.Error(), "find process"):
		return "trigger_find"
	case strings.Contains(err.Error(), "send SIGUSR2"):
		return "trigger_signal"
	default:
		return "other"
	}
}

// --- Hotswap outcome counters (written to provider state dir) ---

var hotswapDeclineMu sync.Mutex

// hotswapCountsFile holds the outcome counters inside the provider's state
// directory.
const hotswapCountsFile = ".hotswap_declines.json"

// hotswapDeclineCounts is the on-disk format for hotswap outcome counters,
// persisted in <stateDir>/.hotswap_declines.json. The provider reads this
// file and exposes the values as Prometheus
// urnet_hotswap_outcomes_total{reason="..."} counters.
type hotswapDeclineCounts struct {
	Counts map[string]int64 `json:"counts"`
}

// recordHotswapDecline increments a decline reason counter in the
// provider's state directory. Best-effort: metric bookkeeping must not
// abort an update.
func recordHotswapDecline(stateDir string, reason string) {
	if reason == "" {
		return
	}
	bumpHotswapCounter(stateDir, reason)
}

// recordHotswapSuccess increments the success counter in the provider's
// state directory. Called once verification confirms the new version is
// running.
func recordHotswapSuccess(stateDir string) {
	bumpHotswapCounter(stateDir, "success")
}

// bumpHotswapCounter increments one outcome counter. The CLI runs as root
// and the state directory belongs to the provider user, so the temp file
// goes through writeStateFile (O_NOFOLLOW): os.WriteFile follows a symlink
// planted at the temp path and would aim a root write at any file on the
// box. rename(2) replaces a symlink at the final path rather than writing
// through it.
func bumpHotswapCounter(stateDir, reason string) {
	if stateDir == "" {
		return
	}
	hotswapDeclineMu.Lock()
	defer hotswapDeclineMu.Unlock()

	dc := hotswapDeclineCounts{Counts: readHotswapDeclines(stateDir)}
	if dc.Counts == nil {
		dc.Counts = make(map[string]int64)
	}
	dc.Counts[reason]++

	data, err := json.Marshal(dc)
	if err != nil {
		return
	}
	tmp := hotswapCountsFile + ".tmp"
	// Write the temp file through a descriptor-pinned handle (the state dir
	// opened O_NOFOLLOW): the write and its ownership are relative to one
	// open directory, so a state dir that IS a symlink (or is swapped for
	// one) cannot aim a root write at another tree. rename(2) below then
	// replaces a symlink at the final path rather than writing through it,
	// and the owner handoff already happened on the descriptor — the old
	// separate chownLikeStateOwner by path is gone.
	// A counter that cannot be recorded is not fatal to the caller, but it must
	// not vanish without a trace: a state dir that cannot be opened (for
	// example a symlinked one) would otherwise stop the metric with no signal.
	h, err := openStateDirHandle(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hotswap] warn: decline counter %q not recorded: %v\n", reason, err)
		return
	}
	defer h.Close()
	if err := h.writeOwned(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[hotswap] warn: decline counter %q not recorded: %v\n", reason, err)
		return
	}
	if err := h.rename(tmp, hotswapCountsFile); err != nil {
		fmt.Fprintf(os.Stderr, "[hotswap] warn: decline counter %q not recorded: %v\n", reason, err)
	}
}

// readHotswapDeclines reads the outcome counters from the provider's
// state directory. Returns nil when the file does not exist or is corrupt.
func readHotswapDeclines(stateDir string) map[string]int64 {
	if stateDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(stateDir, hotswapCountsFile))
	if err != nil {
		return nil
	}
	var dc hotswapDeclineCounts
	if err := json.Unmarshal(data, &dc); err != nil || dc.Counts == nil {
		return nil
	}
	return dc.Counts
}

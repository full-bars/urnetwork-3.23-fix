//go:build !linux

package urnettools

// OpenRC is a Linux init system; every entry point is a no-op elsewhere so
// the cross-platform call sites (cmdStart/cmdStop/cmdRestart, auto-start,
// auto-update, restartProvider, hotSwapUnitOK, renderStatusBase) compile
// unchanged on darwin and windows.

func openrcActive() bool { return false }

func openrcServiceInstalled() bool { return false }

func openrcRouteLifecycle(_ string, _ []string, _, _ bool) (bool, error) {
	return false, nil
}

func openrcRouteAutoStart(_ string, _ []string, _, _ bool) (bool, error) {
	return false, nil
}

func openrcRouteAutoUpdate(_ string, _ []string, _ bool) (bool, error) {
	return false, nil
}

func openrcRestartService(_ Provider) (bool, error) { return false, nil }

func openrcHotSwapDecline(_ Provider) error { return nil }

func renderOpenRCStatus() error { return nil }

// openrcServiceName mirrors the Linux constant. It is a NAME, not behaviour:
// restart_escalation.go interpolates it into operator-facing guidance, and
// that call site is reachable on every platform (the escalation is decided
// from the discovered Provider, not from GOOS). Declaring it const keeps the
// shared wording identical without dragging the Linux logic along. Off Linux
// the string is never passed to rc-service, because every routing helper
// above declines first.
const openrcServiceName = "urnetwork"

// openrcTailServiceLogs is the off-Linux no-op. The `handled` return stays
// false so cmdLogs falls through to the normal per-platform log handling
// instead of reporting a service that does not exist here. Linux declares
// this in openrc.go; without this stub the shared cmdLogs call site
// (legacy_cmds.go) fails to compile for windows and darwin, which breaks
// every release that builds those targets.
func openrcTailServiceLogs(_ Provider, _ int) (bool, error) { return false, nil }

// openrcTreeOwnedByRoot is the off-Linux no-op. There is no OpenRC tree away
// from Linux, so no install destination can fall under it; installBinary
// therefore keeps its chown-to-user behaviour on windows and darwin, which
// is exactly what the session-service update path wants there.
func openrcTreeOwnedByRoot(string) bool { return false }

// openrcStagedRootToolPath is the off-Linux no-op (see openrc.go); no staged
// root tool exists away from Linux.
func openrcStagedRootToolPath() string { return "" }

// providerSupervisedByOpenRCFn reports whether a provider process is
// supervised by OpenRC's supervise-daemon; off Linux there is no OpenRC, so
// it is always false. A var to mirror the Linux seam.
var providerSupervisedByOpenRCFn = func(_ Provider) bool { return false }

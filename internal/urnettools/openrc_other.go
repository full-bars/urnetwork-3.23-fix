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

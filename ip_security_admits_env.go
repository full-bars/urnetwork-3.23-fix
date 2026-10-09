package connect

import (
	"os"
	"strings"
	"sync"

	"github.com/urnetwork/glog"
)

var (
	dpiAdmitsEnvOnce sync.Once
	// Both default OFF: enabling the application-standard admits and the
	// privileged-port signature check changes what a production provider drops.
	// Roll them out per node with URNETWORK_DPI_ADMITS=on and
	// URNETWORK_DPI_PRIVILEGED_BT=1.
	dpiAdmitsEnabled       = false
	dpiPrivilegedBtEnabled = false
)

func initDpiAdmitsEnv() {
	dpiAdmitsEnvOnce.Do(func() {
		rawAdmits := os.Getenv("URNETWORK_DPI_ADMITS")
		if strings.EqualFold(rawAdmits, "on") {
			dpiAdmitsEnabled = true
		}
		rawPrivBt := os.Getenv("URNETWORK_DPI_PRIVILEGED_BT")
		if rawPrivBt == "1" {
			dpiPrivilegedBtEnabled = true
		}
		glog.Infof(
			"[security] dpi configuration URNETWORK_DPI_ADMITS=%q (admits=%t) URNETWORK_DPI_PRIVILEGED_BT=%q (privileged_bt=%t)",
			rawAdmits,
			dpiAdmitsEnabled,
			rawPrivBt,
			dpiPrivilegedBtEnabled,
		)
	})
}

// DpiAdmitsEnabled reports whether protocol standard and endpoint admits are enabled.
func DpiAdmitsEnabled() bool {
	initDpiAdmitsEnv()
	return dpiAdmitsEnabled
}

// DpiPrivilegedBtEnabled reports whether BitTorrent signature inspection on privileged ports is enabled.
func DpiPrivilegedBtEnabled() bool {
	initDpiAdmitsEnv()
	return dpiPrivilegedBtEnabled
}

func resetDpiAdmitsEnvForTest() {
	dpiAdmitsEnvOnce = sync.Once{}
	dpiAdmitsEnabled = false
	dpiPrivilegedBtEnabled = false
}

func applyDpiAdmitsEnvToDmca(settings *DmcaSecurityPolicySettings) {
	if !DpiAdmitsEnabled() {
		settings.App = nil
		settings.Gaming = nil
		settings.Messaging = nil
	}
	if !DpiPrivilegedBtEnabled() {
		settings.InspectPrivilegedSignatures = false
	}
}

func applyDpiAdmitsEnvToWebStandard(settings *WebStandardSettings) {
	if !DpiAdmitsEnabled() {
		settings.Turn = false
		settings.Rtp = false
		settings.Rtcp = false
	}
}

func applyDpiAdmitsEnvToCfaa(settings *CfaaSecurityPolicySettings) {
	if !DpiAdmitsEnabled() {
		settings.AllowTelegramCalls = false
	}
}

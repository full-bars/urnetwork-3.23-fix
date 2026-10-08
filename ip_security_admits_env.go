package connect

import (
	"os"
	"strings"
	"sync"

	"github.com/urnetwork/glog"
)

var (
	dpiAdmitsEnvOnce       sync.Once
	dpiAdmitsEnabled       = true
	dpiPrivilegedBtEnabled = true
)

func initDpiAdmitsEnv() {
	dpiAdmitsEnvOnce.Do(func() {
		rawAdmits := os.Getenv("URNETWORK_DPI_ADMITS")
		if strings.EqualFold(rawAdmits, "off") {
			dpiAdmitsEnabled = false
		}
		rawPrivBt := os.Getenv("URNETWORK_DPI_PRIVILEGED_BT")
		if rawPrivBt == "0" {
			dpiPrivilegedBtEnabled = false
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
	dpiAdmitsEnabled = true
	dpiPrivilegedBtEnabled = true
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

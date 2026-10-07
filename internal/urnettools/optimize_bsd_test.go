package urnettools

import (
	"strings"
	"testing"
)

// The FreeBSD tuning set must contain no Linux-only key. A Linux name here
// would apply as a no-op on FreeBSD while the command still reported success,
// which is the exact failure optimizeFor used to route there.
func TestBSDSysctlWritesAreFreeBSDKeys(t *testing.T) {
	for _, w := range bsdSysctlWrites() {
		key := w[0]
		for _, linuxOnly := range []string{
			"net.core.", "net.ipv4.", "fs.file-max", "net.netfilter.",
		} {
			if strings.HasPrefix(key, linuxOnly) {
				t.Errorf("bsdSysctlWrites contains linux-only key %q", key)
			}
		}
		if !strings.HasPrefix(key, "net.inet.") && !strings.HasPrefix(key, "kern.") {
			t.Errorf("bsdSysctlWrites key %q is neither net.inet.* nor kern.*", key)
		}
	}
	// Every tuned key must have an rc.conf variable to persist through, or it
	// silently reverts at the next reboot.
	rcVars := bsdRcConfVars()
	for _, w := range bsdSysctlWrites() {
		if _, ok := rcVars[w[0]]; !ok {
			t.Errorf("tuned key %q has no rc.conf variable to persist it", w[0])
		}
	}
}

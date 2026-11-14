package urnettools

import (
	"strings"
	"testing"
)

func TestBSDCronTargetsProvider(t *testing.T) {
	for _, tc := range []struct {
		p           Provider
		flag, value string
	}{
		{Provider{User: "alice", StateDir: "/home/alice/.urnetwork"}, "--state-dir", "/home/alice/.urnetwork"},
		{Provider{User: "bob"}, "--user", "bob"},
	} {
		selector, err := bsdUpdateSelector(tc.p)
		if err != nil || len(selector) != 2 || selector[0] != tc.flag || selector[1] != tc.value {
			t.Fatalf("selector: %v, %v", selector, err)
		}
		cmd, err := bsdUpdateCronLine("/bin/tool", "/logs", "daily", selector)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cmd, "'"+tc.flag+"' '"+tc.value+"'") {
			t.Fatalf("missing selector: %s", cmd)
		}
	}
	if _, err := bsdUpdateSelector(Provider{}); err == nil {
		t.Fatal("missing selector accepted")
	}
}

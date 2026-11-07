package urnettools

import (
	"bytes"
	"strings"
	"testing"
)

func TestFreeBSDSetupWarnsBeforeConfirmation(t *testing.T) {
	cfg := &SystemPlatformConfigurer{PlatformName: "freebsd"}
	plan, err := cfg.PlanRequirement("", "provider")
	if err != nil {
		t.Fatal(err)
	}
	for _, accept := range []bool{false, true} {
		var out bytes.Buffer
		runs := 0
		err := executeExtenderSetupPlan(&out, plan, false, false, func() (bool, error) {
			text := out.String()
			for _, want := range []string{"all users", "/etc/sysctl.conf", "restore", "previous"} {
				if !strings.Contains(text, want) {
					t.Errorf("before confirmation missing %q: %s", want, text)
				}
			}
			return accept, nil
		}, func(setupCommand) error { runs++; return nil })
		if accept && (err != nil || runs != 1) {
			t.Fatalf("accepted: runs=%d err=%v", runs, err)
		}
		if !accept && (err == nil || runs != 0) {
			t.Fatalf("declined: runs=%d err=%v", runs, err)
		}
	}
}

package urnettools

import "testing"

func TestFreeBSDPlatformPlanIsEmpty(t *testing.T) {
	cfg := &SystemPlatformConfigurer{PlatformName: "freebsd"}
	for _, user := range []string{"root", "provider"} {
		plan, err := cfg.PlanRequirement("", user)
		if err != nil || len(plan) != 0 {
			t.Fatalf("user=%s plan=%v err=%v", user, plan, err)
		}
	}
}

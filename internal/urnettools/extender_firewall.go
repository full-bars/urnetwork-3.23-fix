package urnettools

import (
	"fmt"
	"strings"
)

// extenderUFWPlan finds rules that are not explicitly allowed from anywhere.
// It does not infer reachability or account for precedence of other rules.
func extenderUFWPlan(status string) ([]setupCommand, error) {
	lines := strings.Split(status, "\n")
	active := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "Status: active" {
			active = true
		}
	}
	if !active {
		return nil, fmt.Errorf("ufw is not active or its status could not be read")
	}
	var plan []setupCommand
	for _, port := range []string{"443/tcp", "443/udp", "4053/udp"} {
		allowed := false
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) == 4 && f[0] == port && f[1] == "ALLOW" && f[2] == "IN" && f[3] == "Anywhere" {
				allowed = true
			}
		}
		if !allowed {
			plan = append(plan, setupCommand{name: "ufw", args: []string{"allow", port}})
		}
	}
	return plan, nil
}

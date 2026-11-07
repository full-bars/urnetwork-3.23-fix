package urnettools

import (
	"fmt"
	"os/exec"
	"strings"
)

// FirewallKind identifies the active firewall manager.
type FirewallKind string

const (
	FirewallUnknown    FirewallKind = "unknown"
	FirewallNone       FirewallKind = "none"
	FirewallFirewalld  FirewallKind = "firewalld"
	FirewallUFW        FirewallKind = "ufw"
	FirewallNftables   FirewallKind = "nftables"
	FirewallIptables   FirewallKind = "iptables"
	FirewallPF         FirewallKind = "pf"
	FirewallIPFW       FirewallKind = "ipfw"
	FirewallWindowsAdv FirewallKind = "windows-advfirewall"
)

// FirewallStatus describes the detected firewall state and rules.
type FirewallStatus struct {
	Kind            FirewallKind   `json:"kind"`
	Active          bool           `json:"active"`
	PortsOpen       bool           `json:"ports_open"`
	PlannedCommands []setupCommand `json:"-"`
	SuggestedRules  []string       `json:"suggested_rules"`
	AppliedRules    []string       `json:"applied_rules,omitempty"`
	Message         string         `json:"message"`
}

// FirewallManager provides firewall detection and rule configuration.
type FirewallManager struct {
	Platform string
	ExecCmd  func(name string, args ...string) ([]byte, error)
	LookPath func(file string) (string, error)
}

// DefaultFirewallManager returns a FirewallManager for the given platform.
func DefaultFirewallManager(platform string) *FirewallManager {
	return &FirewallManager{
		Platform: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
		LookPath: exec.LookPath,
	}
}

// Detect checks the active firewall according to platform-specific ordering.
func (fm *FirewallManager) Detect() FirewallStatus {
	switch fm.Platform {
	case "windows":
		return fm.detectWindows()
	case "freebsd":
		return fm.detectFreeBSD()
	case "darwin":
		return fm.detectDarwin()
	default: // "linux"
		return fm.detectLinux()
	}
}

func (fm *FirewallManager) detectLinux() FirewallStatus {
	// 1. firewalld
	if _, err := fm.LookPath("firewall-cmd"); err == nil {
		if out, err := fm.ExecCmd("firewall-cmd", "--state"); err == nil && strings.TrimSpace(string(out)) == "running" {
			var missing []string
			for _, port := range []string{"443/tcp", "443/udp", "4053/udp"} {
				if q, err := fm.ExecCmd("firewall-cmd", "--query-port="+port); err != nil || strings.TrimSpace(string(q)) != "yes" {
					missing = append(missing, port)
				}
			}
			open := len(missing) == 0
			var planned []setupCommand
			if !open {
				addArgs := []string{"--permanent"}
				for _, port := range missing {
					addArgs = append(addArgs, "--add-port="+port)
				}
				planned = []setupCommand{
					{name: "firewall-cmd", args: addArgs},
					{name: "firewall-cmd", args: []string{"--reload"}},
				}
			}
			rules := []string{
				"firewall-cmd --permanent --add-port=443/tcp --add-port=443/udp --add-port=4053/udp",
				"firewall-cmd --reload",
			}
			if len(planned) > 0 {
				rules = make([]string, 0, len(planned))
				for _, c := range planned {
					rules = append(rules, c.String())
				}
			}
			msg := "firewalld is active"
			if open {
				msg = "firewalld is active; ports 443/tcp, 443/udp, 4053/udp are open"
			}
			return FirewallStatus{
				Kind:            FirewallFirewalld,
				Active:          true,
				PortsOpen:       open,
				PlannedCommands: planned,
				SuggestedRules:  rules,
				Message:         msg,
			}
		}
	}

	// 2. ufw
	if _, err := fm.LookPath("ufw"); err == nil {
		out, err := fm.ExecCmd("ufw", "status")
		if err != nil {
			// `ufw status` needs root. An unprivileged caller cannot tell an
			// active ufw from an absent one, so report it as present and
			// unverified instead of falling through to "no firewall".
			planned := []setupCommand{
				{name: "ufw", args: []string{"allow", "443/tcp"}},
				{name: "ufw", args: []string{"allow", "443/udp"}},
				{name: "ufw", args: []string{"allow", "4053/udp"}},
			}
			return FirewallStatus{
				Kind:            FirewallUFW,
				Active:          true,
				PlannedCommands: planned,
				SuggestedRules: []string{
					"ufw allow 443/tcp",
					"ufw allow 443/udp",
					"ufw allow 4053/udp",
				},
				Message: "ufw is installed but its status is unreadable (needs root); treating it as active",
			}
		}
		str := string(out)
		if strings.Contains(str, "Status: active") {
			planned, _ := extenderUFWPlan(str)
			open := len(planned) == 0
			rules := []string{
				"ufw allow 443/tcp",
				"ufw allow 443/udp",
				"ufw allow 4053/udp",
			}
			if len(planned) > 0 {
				rules = make([]string, 0, len(planned))
				for _, c := range planned {
					rules = append(rules, c.String())
				}
			}
			msg := "ufw is active"
			if open {
				msg = "ufw is active; ports 443/tcp, 443/udp, 4053/udp are allowed"
			}
			return FirewallStatus{
				Kind:            FirewallUFW,
				Active:          true,
				PortsOpen:       open,
				PlannedCommands: planned,
				SuggestedRules:  rules,
				Message:         msg,
			}
		}
	}

	// 3. nftables
	if _, err := fm.LookPath("nft"); err == nil {
		if fm.isSystemdServiceActive("nftables") {
			rules := []string{
				"nft add rule inet filter input tcp dport 443 accept",
				"nft add rule inet filter input udp dport { 443, 4053 } accept",
			}
			return FirewallStatus{
				Kind:           FirewallNftables,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "nftables service is active",
			}
		}
	}

	// 4. iptables
	if _, err := fm.LookPath("iptables"); err == nil {
		if fm.isSystemdServiceActive("iptables") {
			rules := []string{
				"iptables -I INPUT -p tcp --dport 443 -j ACCEPT",
				"iptables -I INPUT -p udp --dport 443 -j ACCEPT",
				"iptables -I INPUT -p udp --dport 4053 -j ACCEPT",
			}
			return FirewallStatus{
				Kind:           FirewallIptables,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "iptables service is active",
			}
		}
	}

	// Unknown / no active firewall service detected
	return FirewallStatus{
		Kind:      FirewallUnknown,
		Active:    false,
		PortsOpen: false,
		SuggestedRules: []string{
			"iptables: iptables -I INPUT -p tcp --dport 443 -j ACCEPT && iptables -I INPUT -p udp --dport 443 -j ACCEPT && iptables -I INPUT -p udp --dport 4053 -j ACCEPT",
			"nftables: nft add rule inet filter input tcp dport 443 accept && nft add rule inet filter input udp dport { 443, 4053 } accept",
		},
		Message: "no active firewall service detected (firewalld, ufw, nftables, iptables); verify host firewall manually",
	}
}

// extenderFirewalldPlan checks permanent and runtime rules for the default zone.
func extenderFirewalldPlan(run func(string, ...string) ([]byte, error)) ([]setupCommand, error) {
	out, err := run("firewall-cmd", "--get-default-zone")
	zone := strings.TrimSpace(string(out))
	if err != nil || zone == "" || strings.IndexFunc(zone, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return nil, fmt.Errorf("cannot determine firewalld default zone: output %q, error %v", zone, err)
	}
	zoneArg := "--zone=" + zone
	var plan []setupCommand
	for _, port := range []string{"443/tcp", "443/udp", "4053/udp"} {
		for _, permanent := range []bool{false, true} {
			args := []string{zoneArg, "--query-port=" + port}
			if permanent {
				args = append([]string{"--permanent"}, args...)
			}
			out, err := run("firewall-cmd", args...)
			answer := strings.TrimSpace(string(out))
			if answer == "yes" && err == nil {
				continue
			}
			if answer != "no" {
				return nil, fmt.Errorf("cannot query firewalld port %s: output %q, error %v", port, answer, err)
			}
			addArgs := []string{zoneArg, "--add-port=" + port}
			if permanent {
				addArgs = append([]string{"--permanent"}, addArgs...)
			}
			plan = append(plan, setupCommand{name: "firewall-cmd", args: addArgs})
		}
	}
	return plan, nil
}

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
		if !ufwAllows(status, port) {
			plan = append(plan, setupCommand{name: "ufw", args: []string{"allow", port}})
		}
	}
	return plan, nil
}

func ufwAllows(status, port string) bool {
	allowed := false
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == port && (f[1] == "DENY" || f[1] == "REJECT") {
			return false
		}
		if len(f) == 3 && f[0] == port && f[1] == "ALLOW" && f[2] == "Anywhere" {
			allowed = true
		}
		if len(f) == 4 && f[0] == port && f[1] == "ALLOW" && f[2] == "IN" && f[3] == "Anywhere" {
			allowed = true
		}
	}
	return allowed
}

func (fm *FirewallManager) isSystemdServiceActive(service string) bool {
	if _, err := fm.LookPath("systemctl"); err != nil {
		return false
	}
	out, err := fm.ExecCmd("systemctl", "is-active", service)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func (fm *FirewallManager) detectFreeBSD() FirewallStatus {
	// 1. pf
	if _, err := fm.LookPath("pfctl"); err == nil {
		out, err := fm.ExecCmd("pfctl", "-s", "info")
		if err == nil && strings.Contains(string(out), "Status: Enabled") {
			rules := []string{
				"pass in proto tcp to any port 443",
				"pass in proto udp to any port { 443 4053 }",
			}
			return FirewallStatus{
				Kind:           FirewallPF,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "pf is active; ensure /etc/pf.conf allows inbound tcp 443, udp 443, udp 4053",
			}
		}
	}

	// 2. ipfw
	if _, err := fm.LookPath("ipfw"); err == nil {
		out, err := fm.ExecCmd("service", "ipfw", "status")
		if err == nil && strings.Contains(strings.ToLower(string(out)), "is running") {
			rules := []string{
				"ipfw add allow tcp from any to any 443 in",
				"ipfw add allow udp from any to any 443,4053 in",
			}
			return FirewallStatus{
				Kind:           FirewallIPFW,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "ipfw is active; ensure rules allow inbound tcp 443, udp 443, udp 4053",
			}
		}
	}

	return FirewallStatus{
		Kind:      FirewallUnknown,
		Active:    false,
		PortsOpen: false,
		SuggestedRules: []string{
			"pf: pass in proto tcp to any port 443; pass in proto udp to any port { 443 4053 }",
			"ipfw: ipfw add allow tcp from any to any 443 in; ipfw add allow udp from any to any 443,4053 in",
		},
		Message: "no active firewall service detected (pf, ipfw); verify host firewall manually",
	}
}

func (fm *FirewallManager) detectDarwin() FirewallStatus {
	return FirewallStatus{
		Kind:      FirewallNone,
		Active:    true,
		PortsOpen: false,
		SuggestedRules: []string{
			"macOS Application Firewall (ALF) may prompt to allow inbound connections; confirm in System Settings > Network > Firewall",
		},
		Message: "macOS application firewall prompts interactively per binary; cannot be automated silently",
	}
}

func (fm *FirewallManager) detectWindows() FirewallStatus {
	ruleName := "URnetwork Extender"
	planned := planWindowsRules(fm.ExecCmd)
	ruleExists := len(planned) == 0

	rules := []string{
		fmt.Sprintf(`netsh advfirewall firewall add rule name="%s" dir=in action=allow protocol=TCP localport=443`, ruleName),
		fmt.Sprintf(`netsh advfirewall firewall add rule name="%s UDP" dir=in action=allow protocol=UDP localport=443,4053`, ruleName),
	}

	msg := "Windows Firewall: rule does not exist"
	if ruleExists {
		msg = fmt.Sprintf("Windows Firewall: rule %q already exists", ruleName)
	}

	return FirewallStatus{
		Kind:            FirewallWindowsAdv,
		Active:          true,
		PortsOpen:       ruleExists,
		PlannedCommands: planned,
		SuggestedRules:  rules,
		Message:         msg,
	}
}

// windowsRuleExists reports whether the named inbound rule is present and
// usable. A rule that exists but is disabled or set to block is present and
// not usable: it must be repaired, not counted as configured.
func windowsRuleExists(run func(string, ...string) ([]byte, error), name string) bool {
	present, usable := windowsRuleState(run, name)
	return present && usable
}

// windowsRuleState parses `netsh advfirewall firewall show rule`. Only the
// fields it can read are judged: on a localized Windows the labels differ, and
// a field that cannot be found is not held against the rule.
func windowsRuleState(run func(string, ...string) ([]byte, error), name string) (present, usable bool) {
	out, err := run("netsh", "advfirewall", "firewall", "show", "rule", "name="+name)
	if err != nil {
		return false, false
	}
	usable = true
	inRule := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, ":")
		if i < 0 {
			if line == name {
				present, inRule = true, true
			}
			continue
		}
		key, val := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		if val == name && strings.EqualFold(key, "Rule Name") {
			present, inRule = true, true
			continue
		}
		if !inRule {
			if val == name {
				present, inRule = true, true
			}
			continue
		}
		switch {
		case strings.EqualFold(key, "Enabled") && strings.EqualFold(val, "No"):
			usable = false
		case strings.EqualFold(key, "Action") && strings.EqualFold(val, "Block"):
			usable = false
		case strings.EqualFold(key, "Direction") && strings.EqualFold(val, "Out"):
			usable = false
		}
	}
	return present, present && usable
}

func planWindowsRules(run func(string, ...string) ([]byte, error)) []setupCommand {
	var plan []setupCommand
	for _, rule := range []struct{ name, protocol, ports string }{
		{"URnetwork Extender", "TCP", "443"},
		{"URnetwork Extender UDP", "UDP", "443,4053"},
	} {
		present, usable := windowsRuleState(run, rule.name)
		if present && usable {
			continue
		}
		if present {
			plan = append(plan, setupCommand{
				name: "netsh",
				args: []string{"advfirewall", "firewall", "delete", "rule", "name=" + rule.name},
			})
		}
		plan = append(plan, setupCommand{
			name: "netsh",
			args: []string{"advfirewall", "firewall", "add", "rule", "name=" + rule.name, "dir=in", "action=allow", "protocol=" + rule.protocol, "localport=" + rule.ports},
		})
	}
	return plan
}

func applyWindowsRules(run func(string, ...string) ([]byte, error)) (bool, error) {
	applied := false
	for _, rule := range []struct{ name, protocol, ports string }{
		{"URnetwork Extender", "TCP", "443"},
		{"URnetwork Extender UDP", "UDP", "443,4053"},
	} {
		present, usable := windowsRuleState(run, rule.name)
		if present && usable {
			continue
		}
		if present {
			if out, err := run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+rule.name); err != nil {
				return applied, fmt.Errorf("netsh delete unusable %s rule failed: %s: %w", rule.protocol, strings.TrimSpace(string(out)), err)
			}
		}
		out, err := run("netsh", "advfirewall", "firewall", "add", "rule", "name="+rule.name, "dir=in", "action=allow", "protocol="+rule.protocol, "localport="+rule.ports)
		if err != nil {
			return applied, fmt.Errorf("netsh add %s rule failed: %s: %w", rule.protocol, strings.TrimSpace(string(out)), err)
		}
		applied = true
	}
	return applied, nil
}

// Plan returns the exact firewall commands that Apply would execute.
func (fm *FirewallManager) Plan(status FirewallStatus) []setupCommand {
	if status.PortsOpen {
		return nil
	}
	switch status.Kind {
	case FirewallFirewalld:
		if len(status.PlannedCommands) > 0 {
			return append([]setupCommand(nil), status.PlannedCommands...)
		}
		return []setupCommand{
			{name: "firewall-cmd", args: []string{"--permanent", "--add-port=443/tcp", "--add-port=443/udp", "--add-port=4053/udp"}},
			{name: "firewall-cmd", args: []string{"--reload"}},
		}
	case FirewallUFW:
		if len(status.PlannedCommands) > 0 {
			return append([]setupCommand(nil), status.PlannedCommands...)
		}
		return []setupCommand{
			{name: "ufw", args: []string{"allow", "443/tcp"}},
			{name: "ufw", args: []string{"allow", "443/udp"}},
			{name: "ufw", args: []string{"allow", "4053/udp"}},
		}
	case FirewallWindowsAdv:
		if len(status.PlannedCommands) > 0 {
			return append([]setupCommand(nil), status.PlannedCommands...)
		}
		return planWindowsRules(fm.ExecCmd)
	default:
		return nil
	}
}

// Apply attempts to apply the firewall configuration if possible with root/admin privileges.
func (fm *FirewallManager) Apply(status FirewallStatus) (applied bool, err error) {
	switch status.Kind {
	case FirewallFirewalld:
		if status.PortsOpen {
			return false, nil
		}
		cmds := fm.Plan(status)
		for _, c := range cmds {
			if _, err := fm.ExecCmd(c.name, c.args...); err != nil {
				if len(c.args) > 0 && c.args[0] == "--reload" {
					return false, fmt.Errorf("firewall-cmd reload failed: %w", err)
				}
				return false, fmt.Errorf("firewall-cmd add-port failed: %w", err)
			}
		}
		return true, nil

	case FirewallUFW:
		if status.PortsOpen {
			return false, nil
		}
		for _, c := range fm.Plan(status) {
			portProto := ""
			if len(c.args) >= 2 {
				portProto = c.args[1]
			}
			if _, err := fm.ExecCmd(c.name, c.args...); err != nil {
				return false, fmt.Errorf("ufw allow %s failed: %w", portProto, err)
			}
		}
		return true, nil

	case FirewallWindowsAdv:
		if status.PortsOpen {
			return false, nil
		}
		return applyWindowsRules(fm.ExecCmd)

	default:
		// For nftables, iptables, pf, ipfw, or unknown: do not silently succeed.
		// Return false with no error so the caller prints the exact suggested rules.
		return false, nil
	}
}

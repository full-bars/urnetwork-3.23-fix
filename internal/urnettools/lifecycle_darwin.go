//go:build darwin

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// macOS lifecycle uses launchd LaunchAgents. The provider runs as a
// user-level process, so agents live in ~/Library/LaunchAgents and are
// bootstrapped into the user domain. gui/<uid> is the normal domain (works
// when the user has a GUI login session); user/<uid> is the headless
// fallback (SSH-only / server Macs). Both avoid stored credentials.

// launchAgentsDir is the per-user LaunchAgents directory.
func launchAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents")
}

// setAutoStart enables or disables login auto-start by installing (or
// removing) the provider LaunchAgent.
func setAutoStart(p Provider, on bool) error {
	if on {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no binary path", providerLabel(p))
		}
		dir := launchAgentsDir()
		if dir == "" {
			return fmt.Errorf("cannot resolve home for LaunchAgents dir")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		plist := filepath.Join(dir, "com.urnetwork.provider.plist")
		content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.urnetwork.provider</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>provide</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>WorkingDirectory</key><string>%s</string>
</dict>
</plist>
`, p.Binary, filepath.Dir(p.Binary))
		if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
			return err
		}
		return launchctlBootstrap(plist)
	}
	plist := filepath.Join(launchAgentsDir(), "com.urnetwork.provider.plist")
	return launchctlBootout(plist)
}

// setAutoUpdateSchedule manages the auto-update LaunchAgent. label is the
// platform-neutral identifier; the concrete agent name is fixed. Weekly is
// the default posture: launchd StartCalendarInterval fires at the given
// weekday/minute; daily and monthly use their own calendars.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	const agentName = "com.urnetwork.update"
	plist := filepath.Join(launchAgentsDir(), agentName+".plist")
	switch interval {
	case "off":
		return launchctlBootout(plist)
	case "daily":
		return writeUpdateAgent(plist, "EveryDay")
	case "weekly":
		// Sunday 00:00 (weekday 0 = Sunday in launchd).
		return writeUpdateAgent(plist, "EveryWeek")
	case "monthly":
		return writeUpdateAgent(plist, "EveryMonth")
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// writeUpdateAgent writes the auto-update LaunchAgent plist and bootstraps
// it. The agent runs `urnet-tools update -f` on the configured calendar.
func writeUpdateAgent(plist, calendarName string) error {
	dir := launchAgentsDir()
	if dir == "" {
		return fmt.Errorf("cannot resolve home for LaunchAgents dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	exe := toolExePath()
	var calendar string
	switch calendarName {
	case "EveryDay":
		calendar = "<key>Hour</key><integer>0</integer><key>Minute</key><integer>0</integer>"
	case "EveryWeek":
		calendar = "<key>Weekday</key><integer>0</integer><key>Hour</key><integer>0</integer><key>Minute</key><integer>0</integer>"
	case "EveryMonth":
		calendar = "<key>Day</key><integer>1</integer><key>Hour</key><integer>0</integer><key>Minute</key><integer>0</integer>"
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.urnetwork.update</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>update</string>
        <string>-f</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        %s
    </dict>
</dict>
</plist>
`, exe, calendar)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	return launchctlBootstrap(plist)
}

// toolExePath returns the running tool's absolute path for the agent.
func toolExePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Clean(exe)
	}
	return "urnet-tools"
}

// launchctlBootstrap loads an agent into the user domain, preferring
// gui/<uid> (login session) and falling back to user/<uid> (headless).
func launchctlBootstrap(plist string) error {
	uid := os.Getuid()
	// Try gui domain first (normal login), then user domain (headless).
	if err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), plist).Run(); err == nil {
		return nil
	}
	if err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("user/%d", uid), plist).Run(); err == nil {
		return nil
	}
	// Last-resort legacy load for older macOS / partial sessions.
	if err := exec.Command("launchctl", "load", "-w", plist).Run(); err == nil {
		return nil
	}
	// The plist is on disk; a later login/session will pick it up. Report
	// the failure but do not remove the file (the agent is registered).
	return fmt.Errorf("launchctl bootstrap failed for %s", plist)
}

// launchctlBootout removes an agent, tolerating "not loaded" (a clean
// no-op, like deleteTaskIfExists on Windows).
func launchctlBootout(plist string) error {
	uid := os.Getuid()
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d", uid), plist).Run()
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("user/%d", uid), plist).Run()
	_ = exec.Command("launchctl", "unload", "-w", plist).Run()
	_ = os.Remove(plist)
	return nil
}

// cleanupLifecycle removes the LaunchAgents on uninstall.
func cleanupLifecycle(p Provider) {
	dir := launchAgentsDir()
	if dir == "" {
		return
	}
	_ = launchctlBootout(filepath.Join(dir, "com.urnetwork.provider.plist"))
	_ = launchctlBootout(filepath.Join(dir, "com.urnetwork.update.plist"))
}

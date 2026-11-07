package main

import (
	"os"
	"strings"
)

// extenderControlKey is the control-socket setting that turns the extender
// role on or off. The role is ON by default; `urnet-tools extender off` turns
// it off and keeps it off across restarts, and `urnet-tools extender on` turns
// it back on, with no unit file or drop-in involved.
const extenderControlKey = "extender"

// extenderSetting says whether the operator wants the role, and in words where
// that answer came from, so the startup line and `extender status` can name it.
//
// Precedence, highest first:
//  1. the control-socket setting (`urnet-tools extender on|off`)
//  2. URNETWORK_EXTENDER=on|off in the environment, kept for existing installs
//  3. the default, which is on
//
// An unrecognised environment value is ignored rather than guessed at: the
// default stands and the caller can report the value it skipped.
func extenderSetting(environ func(string) string) (on bool, source string) {
	if value, ok := globalControlState.get(extenderControlKey); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "off", "0", "false", "no":
			return false, "control socket (urnet-tools extender off)"
		case "on", "1", "true", "yes":
			return true, "control socket (urnet-tools extender on)"
		}
	}
	switch strings.ToLower(strings.TrimSpace(environ(extenderEnv))) {
	case "off", "0", "false", "no":
		return false, extenderEnv + " environment variable"
	case "on", "1", "true":
		return true, extenderEnv + " environment variable"
	}
	return true, "default"
}

// extenderUnrecognisedEnv returns the value of URNETWORK_EXTENDER when it is
// set to something that is neither on nor off, so the start can say it was
// ignored. Empty when the variable is unset or understood.
func extenderUnrecognisedEnv(environ func(string) string) string {
	raw := strings.TrimSpace(environ(extenderEnv))
	switch strings.ToLower(raw) {
	case "", "on", "1", "true", "off", "0", "false", "no":
		return ""
	}
	return raw
}

// extenderEnvDefaultName is what clearing the control key returns to: the
// environment's answer when it has one, else the default. Used by the live
// apply of `clear`, which must not leave the role in the state the cleared
// setting had.
func extenderEnvDefaultName() string {
	if on, _ := extenderSettingWithoutControl(os.Getenv); !on {
		return "off"
	}
	return "on"
}

func extenderSettingWithoutControl(environ func(string) string) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(environ(extenderEnv))) {
	case "off", "0", "false", "no":
		return false, extenderEnv + " environment variable"
	}
	return true, "default"
}

// applyExtenderLive makes a changed setting take effect in this process: the
// manager starts or stops the role to match.
func applyExtenderLive(value string) error {
	why := "the extender setting changed to " + strings.ToLower(strings.TrimSpace(value))
	globalExtenderManager.Reconcile(why)
	return nil
}

package urnettools

import "fmt"

// OpenBSD keeps rc.d scripts in /etc/rc.d and drives them with rcctl(8). The
// helpers here are pure so the argv and paths can be unit-tested on any
// platform. The functions that run them live in lifecycle_openbsd.go.

// openbsdRcScriptDir is where the OpenBSD installer writes the service script.
// It must match rc_script in scripts/Provider_Install_OpenBSD.sh.
const openbsdRcScriptDir = "/etc/rc.d"

// openbsdRcScriptPath is the on-disk path of the rc.d script for a service.
func openbsdRcScriptPath(service string) string {
	return openbsdRcScriptDir + "/" + service
}

// openbsdRcctlArgv builds the command that runs `rcctl <args...>`, escalating
// when the caller is not root. doas is the OpenBSD default and is preferred over
// sudo. A caller that is neither root nor able to escalate gets an error that
// names the missing tool instead of an rcctl permission failure.
func openbsdRcctlArgv(euid int, hasDoas, hasSudo bool, args ...string) (name string, argv []string, err error) {
	switch {
	case euid == 0:
		return "rcctl", args, nil
	case hasDoas:
		return "doas", append([]string{"rcctl"}, args...), nil
	case hasSudo:
		return "sudo", append([]string{"rcctl"}, args...), nil
	}
	return "", nil, fmt.Errorf("`rcctl %s` needs root and neither doas nor sudo is installed - run it as root", joinArgs(args))
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

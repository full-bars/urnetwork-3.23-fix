package urnettools

import (
	"fmt"
	"strings"
)

// dockerUserPrefix marks a Provider record as a docker container (see
// dockerProvider in docker.go). A provider whose User field carries this
// prefix is managed by urnet-docker, never by the systemd/process tool.
const dockerUserPrefix = "docker:"

// isDockerProvider reports whether a resolved Provider is a docker
// container rather than a systemd/binary deployment. The two deployment
// namespaces must never cross (LA1 incident, 2026-08-24: `urnet-tools stop
// ps` silently dropped the unrecognized positional "ps", fell through to
// no-target default selection, and stopped the unrelated host systemd
// provider).
func isDockerProvider(p Provider) bool {
	return strings.HasPrefix(p.User, dockerUserPrefix)
}

// errLifecycleLeftoverPositional is returned when a lifecycle command
// (start/stop/restart) is given a bare positional that did not resolve to
// any target flag. These commands take NO positional arguments: an extra
// word almost always means the operator used the wrong tool (urnet-tools
// vs urnet-docker) or mistyped a --unit value. Refuse loudly instead of
// silently acting on the default provider.
func errLifecycleLeftoverPositional(cmd string, extra []string, providers []Provider) error {
	msg := fmt.Sprintf(
		"%s takes no positional arguments — got %q.\n"+
			"To target a systemd provider use a flag, e.g.: urnet-tools %s --unit <unit>\n",
		cmd, strings.Join(extra, " "), cmd)
	for _, p := range providers {
		if isDockerProvider(p) && len(extra) == 1 && p.Unit == extra[0] {
			msg = fmt.Sprintf(
				"urnet-tools %s takes no positional arguments — got %q, which matches DOCKER container %q.\n"+
					"That provider runs in docker; manage it with: urnet-docker %s %s\n",
				cmd, extra[0], p.Unit, cmd, extra[0])
			break
		}
	}
	return fmt.Errorf("%sRefusing to act on a default provider because the command line had extra arguments — name the target explicitly.", msg)
}

// guardLifecycleArgs enforces the lifecycle-command contract for
// start/stop/restart:
//   - leftover bare positionals are a hard error (never a silent default);
//   - a resolved provider that is a docker container is refused (wrong
//     tool — urnet-docker owns containers), unless the caller opted into
//     docker awareness.
//
// It returns the parsed Target on success.
func guardLifecycleArgs(cmd string, args []string) (Target, error) {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return t, err
	}
	if len(rest) > 0 {
		return t, errLifecycleLeftoverPositional(cmd, rest, discoverDockerFn())
	}
	return t, nil
}

// guardSystemdProvider refuses a resolved provider that lives in the docker
// namespace: unitCommand would run systemctl against the container NAME as
// if it were a systemd unit (cross-namespace contamination, LA1 D6).
//
// Reachability note (Sonnet HIGH, meso-miner PR #10/#12 review): this guard
// is LIVE on the lifecycle paths (start/stop/restart) because
// lifecycleCandidates widens the candidate pool with docker containers when
// an explicit target flag was given. It remains defense-in-depth for every
// other caller of unitCommand whose discovery does not include containers.
func guardSystemdProvider(p Provider) error {
	if isDockerProvider(p) {
		return fmt.Errorf(
			"%s is a docker container, not a systemd provider — refusing to touch it with urnet-tools (use urnet-docker)",
			providerLabel(p))
	}
	return nil
}

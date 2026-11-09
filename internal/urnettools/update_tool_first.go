package urnettools

import (
	"strconv"
	"strings"
)

// urnetUpdateReexecEnv is set in the environment of a re-executed tool process
// when the running tool installs a newer release tool first before continuing
// with the provider update. It acts as both a loop guard (preventing further
// re-exec) and a signal that the operator's update confirmation was already
// obtained in the parent process.
const urnetUpdateReexecEnv = "URNET_TOOLS_UPDATE_REEXEC"

var (
	reExecToolFn     = reExecTool
	isToolNewerFn    = isToolNewer
	selfUpdateToolFn = selfUpdateTool
)

// isToolNewer compares the candidate release tag against the currently running
// tool version. Returns true only when the candidate version is deterministically
// newer than the running tool version. Local dev builds ("dev") are treated as
// unreleased head-of-tree and decline self-update by default.
func isToolNewer(candidateTag, runningVersion string) bool {
	if candidateTag == "" {
		return false
	}
	if runningVersion == "dev" {
		return false
	}
	if runningVersion == "" {
		return true
	}
	return compareVersionTags(candidateTag, runningVersion) > 0
}

// isToolNewerThanRunning checks whether candidateTag is newer than the running
// tool and ensures the installed binary does not already match toolDigest.
func isToolNewerThanRunning(candidateTag, toolDigest string) bool {
	if candidateTag == "" || toolDigest == "" {
		return false
	}
	exe := resolvedExecutablePath()
	if cur, ferr := fileSHA256(exe); ferr == nil && strings.EqualFold(cur, toolDigest) {
		return false
	}
	return isToolNewerFn(candidateTag, ToolVersion)
}

// compareVersionTags compares two version strings (e.g. "v3.23.0-fix.35.6" vs "v3.23.0-fix.35.5").
// Returns:
//
//	+1 if v1 > v2
//	-1 if v1 < v2
//	 0 if v1 == v2
func compareVersionTags(v1, v2 string) int {
	v1 = strings.TrimPrefix(strings.TrimSpace(v1), "v")
	v1 = strings.TrimPrefix(v1, "V")
	v2 = strings.TrimPrefix(strings.TrimSpace(v2), "v")
	v2 = strings.TrimPrefix(v2, "V")

	if v1 == v2 {
		return 0
	}

	parts1 := splitVersionParts(v1)
	parts2 := splitVersionParts(v2)

	n := len(parts1)
	if len(parts2) < n {
		n = len(parts2)
	}

	for i := 0; i < n; i++ {
		p1, p2 := parts1[i], parts2[i]
		num1, err1 := strconv.Atoi(p1)
		num2, err2 := strconv.Atoi(p2)

		if err1 == nil && err2 == nil {
			if num1 > num2 {
				return 1
			}
			if num1 < num2 {
				return -1
			}
		} else if err1 == nil && err2 != nil {
			return 1
		} else if err1 != nil && err2 == nil {
			return -1
		} else {
			if p1 > p2 {
				return 1
			}
			if p1 < p2 {
				return -1
			}
		}
	}

	if len(parts1) > len(parts2) {
		return 1
	}
	if len(parts1) < len(parts2) {
		return -1
	}
	return 0
}

func splitVersionParts(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
}

// buildReexecArgs constructs the argv for the re-executed tool process, preserving
// user arguments while making tag and target selection explicit so the child does
// not repeat interactive prompts.
func buildReexecArgs(origArgs []string, chosen []Provider, all bool, tag string) []string {
	var base []string
	if containsArg(origArgs, "update") {
		base = append([]string{}, origArgs...)
	} else {
		base = []string{resolvedExecutablePath(), "update"}
		if len(origArgs) > 0 {
			base = append(base, origArgs...)
		}
	}

	hasTag := false
	hasTarget := false
	for _, arg := range base {
		if arg == "--tag" || strings.HasPrefix(arg, "--tag=") {
			hasTag = true
		}
		if arg == "--all" || arg == "-all" ||
			arg == "--unit" || strings.HasPrefix(arg, "--unit=") ||
			arg == "--user" || strings.HasPrefix(arg, "--user=") ||
			arg == "--network" || strings.HasPrefix(arg, "--network=") ||
			arg == "--network-id" || strings.HasPrefix(arg, "--network-id=") ||
			arg == "--state-dir" || strings.HasPrefix(arg, "--state-dir=") ||
			arg == "--include" || strings.HasPrefix(arg, "--include=") ||
			arg == "--exclude" || strings.HasPrefix(arg, "--exclude=") {
			hasTarget = true
		}
	}

	if !hasTag && tag != "" {
		base = append(base, "--tag", tag)
	}

	if !hasTarget {
		if all {
			base = append(base, "--all")
		} else if len(chosen) == 1 {
			if chosen[0].Unit != "" {
				base = append(base, "--unit", chosen[0].Unit)
			} else if chosen[0].StateDir != "" {
				base = append(base, "--state-dir", chosen[0].StateDir)
			}
		} else if len(chosen) > 1 {
			labels := make([]string, 0, len(chosen))
			for _, p := range chosen {
				labels = append(labels, providerLabel(p))
			}
			base = append(base, "--include", strings.Join(labels, ","))
		}
	}
	return base
}

func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

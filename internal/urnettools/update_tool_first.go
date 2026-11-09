package urnettools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// urnetUpdateReexecEnv is set in the environment of a re-executed tool process
// when the running tool installs a newer release tool first before continuing
// with the provider update. It acts as both a loop guard (preventing further
// re-exec) and a signal that the operator's update confirmation was already
// obtained in the parent process.
const urnetUpdateReexecEnv = "URNET_TOOLS_UPDATE_REEXEC"
const urnetUpdatePinnedEnv = "URNET_TOOLS_UPDATE_PINNED"

type reexecIdentity struct {
	Unit, StateDir, User string
}

func reexecIdentities(providers []Provider) []reexecIdentity {
	ids := make([]reexecIdentity, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, reexecIdentity{p.Unit, p.StateDir, p.User})
	}
	return ids
}

func sameReexecProviders(a, b []Provider) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[reexecIdentity]int{}
	for _, id := range reexecIdentities(a) {
		counts[id]++
	}
	for _, id := range reexecIdentities(b) {
		counts[id]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func reexecSingleTarget(providers []Provider, p Provider) (Target, error) {
	for _, target := range []Target{{Unit: p.Unit}, {StateDir: p.StateDir}} {
		if target.Unit == "" && target.StateDir == "" {
			continue
		}
		var matches []Provider
		for _, candidate := range providers {
			if target.matchProvider(candidate) {
				matches = append(matches, candidate)
			}
		}
		if sameReexecProviders(matches, []Provider{p}) {
			return target, nil
		}
	}
	return Target{}, fmt.Errorf("provider selection is ambiguous or changed")
}

// validateReexecSelection checks the actual parser's matching rules against the
// full inventory, including collisions with providers outside the selection.
func validateReexecSelection(providers, chosen []Provider) error {
	if len(chosen) == 0 {
		return fmt.Errorf("empty provider selection")
	}
	for _, p := range chosen {
		if p.Unit == "" && p.StateDir == "" {
			return fmt.Errorf("provider has neither unit nor state directory")
		}
	}
	var resolved []Provider
	var err error
	if len(chosen) == 1 {
		_, err = reexecSingleTarget(providers, chosen[0])
		if err != nil {
			return err
		}
		resolved = chosen
	} else {
		labels := make([]string, 0, len(chosen))
		for _, p := range chosen {
			label := providerLabel(p)
			if label == "" || strings.Contains(label, ",") || strings.TrimSpace(label) != label {
				return fmt.Errorf("provider label cannot be represented in an include list")
			}
			labels = append(labels, label)
		}
		resolved, err = selectByLabels(providers, labels)
		if err != nil {
			return err
		}
	}
	if !sameReexecProviders(resolved, chosen) {
		return fmt.Errorf("provider selection is ambiguous or changed")
	}
	return nil
}

func encodeReexecSelection(chosen []Provider) string {
	data, _ := json.Marshal(reexecIdentities(chosen))
	return string(data)
}

func reexecEnvironment(env []string, chosen []Provider) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if strings.HasPrefix(entry, urnetUpdateReexecEnv+"=") || strings.HasPrefix(entry, urnetUpdatePinnedEnv+"=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, urnetUpdateReexecEnv+"=1", urnetUpdatePinnedEnv+"="+encodeReexecSelection(chosen))
}

func checkReexecSelection(providers, chosen []Provider, encoded string) error {
	var expected []reexecIdentity
	if err := json.Unmarshal([]byte(encoded), &expected); err != nil || len(expected) == 0 {
		return fmt.Errorf("re-exec has no valid pinned provider selection; nothing updated")
	}
	var pinned []Provider
	for _, id := range expected {
		pinned = append(pinned, Provider{Unit: id.Unit, StateDir: id.StateDir, User: id.User})
	}
	if !sameReexecProviders(pinned, chosen) {
		return fmt.Errorf("re-exec provider selection changed; nothing updated")
	}
	if err := validateReexecSelection(providers, chosen); err != nil {
		return fmt.Errorf("re-exec provider selection changed: %v; nothing updated", err)
	}
	return nil
}

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
	filtered := make([]string, 0, len(base))
	selectors := map[string]bool{"--unit": true, "--state-dir": true, "--user": true, "--network": true, "--network-id": true, "--pid": true, "--include": true, "--exclude": true}
	for i := 0; i < len(base); i++ {
		arg := base[i]
		if arg == "--tag" || strings.HasPrefix(arg, "--tag=") {
			hasTag = true
		}
		if arg == "--all" || arg == "-all" {
			continue
		}
		key, _, hasValue := strings.Cut(arg, "=")
		if selectors[key] {
			if !hasValue && i+1 < len(base) {
				i++
			}
			continue
		}
		filtered = append(filtered, arg)
	}
	if !hasTag && tag != "" {
		filtered = append(filtered, "--tag", tag)
	}
	if len(chosen) == 1 {
		if chosen[0].Unit != "" {
			filtered = append(filtered, "--unit", chosen[0].Unit)
		} else {
			filtered = append(filtered, "--state-dir", chosen[0].StateDir)
		}
	} else if len(chosen) > 1 {
		labels := make([]string, 0, len(chosen))
		for _, p := range chosen {
			labels = append(labels, providerLabel(p))
		}
		filtered = append(filtered, "--include", strings.Join(labels, ","))
	}
	return filtered
}

func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

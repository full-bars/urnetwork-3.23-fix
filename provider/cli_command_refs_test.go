// cli_command_refs_test.go — docs and comments that tell someone to run
// `provider <cmd>` must name a command the CLI has. A removed or renamed
// command left in the docs sends the reader to something that does not exist.
//
// Ported from sn's miner/cli_command_refs_test.go (TestDocumentedCliCommandsExist).
// The sn original also checks the snclaim binary; there is none on this line, so
// this checks the provider's own command set: the first word after `provider`
// on every usage line, plus the nested groups whose next word is always a
// subcommand (`provider fleet <sub>`, `provider wallet <sub>` and
// `provider wallet hotkey <sub>`).
//
// The nested checks cover the families the v33 Bittensor port adds, so a docs
// reference such as `provider fleet manifest` is verified against the fleet
// subcommands and not just against the top-level word `fleet`.
package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestDocumentedCliCommandsExist(t *testing.T) {
	sets := documentedCommandSets(mainUsage())
	for group, set := range sets {
		if len(set) == 0 {
			t.Fatalf("no %s commands found in the usage", group)
		}
	}
	// `provider help` is a real alias handled before docopt is consulted (see
	// main), so it is deliberately absent from the usage text. Everything else
	// the docs name has to be a usage command.
	sets["provider"]["help"] = true
	// `provider consent for client <id>` is a line `provider wallet status`
	// prints, quoted in the Bittensor Quick Start. It is output, not a command.
	sets["provider"]["consent"] = true

	root := ".."
	skip := map[string]bool{
		".git":                          true,
		"third_party":                   true,
		"node_modules":                  true,
		filepath.Join("docs", "parked"): true,
	}

	var missing []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if skip[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, ref := range missingCommandRefs(string(b), sets) {
			missing = append(missing, rel+": "+ref)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("documented commands missing from the CLI usage:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestDocumentedCliCommandCheckerFlagsUnknown exercises the checker on
// synthetic content so a regression that quietly makes it blind (a broken
// nested reference, a lost pattern) fails loudly instead of passing suites.
func TestDocumentedCliCommandCheckerFlagsUnknown(t *testing.T) {
	// The references are assembled from pieces: the walk above also reads this
	// file, and a literal reference to an unknown fleet subcommand here would
	// itself be reported as a documented command the CLI does not have.
	ref := func(words ...string) string { return "`provider " + strings.Join(words, " ") + "`" }
	content := strings.Join([]string{
		"Real: " + ref("fleet", "manifest") + ", " + ref("fleet", "status") + ", " + ref("fleet", "revoke") + ".",
		"Real: " + ref("wallet", "hotkey", "challenge") + ", " + ref("wallet", "hotkey", "set") + ", " + ref("wallet", "status") + ".",
		"Real: " + ref("claim-daemon") + ", " + ref("claim-daemon-v2") + ", " + ref("operators") + ".",
		"Not real: " + ref("fleet", "evacuate") + ".",
		"Not real: " + ref("fleet-cmd") + ".",
		"Not real: " + ref("wallet", "hotkey", "rotate") + ".",
		"Not real: " + ref("wallet", "rotate") + ".",
	}, "\n")
	want := []string{
		"provider fleet evacuate",
		"provider fleet-cmd",
		"provider wallet hotkey rotate",
		"provider wallet rotate",
	}
	got := missingCommandRefs(content, documentedCommandSets(mainUsage()))
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checker mismatch\n got: %q\nwant: %q", got, want)
	}
}

// documentedCommandSets extracts the command sets the checker uses from the
// usage: the first word after `provider` on every usage line, plus the nested
// groups whose next word is always a subcommand.
func documentedCommandSets(usage string) map[string]map[string]bool {
	return map[string]map[string]bool{
		"provider":               usageCommands(usage, "provider"),
		"provider fleet":         usageCommands(usage, "provider fleet"),
		"provider wallet":        usageCommands(usage, "provider wallet"),
		"provider wallet hotkey": usageCommands(usage, "provider wallet hotkey"),
	}
}

// missingCommandRefs returns every backticked `provider ...` reference in
// content that does not name a command the usage declares. Each nested group
// is checked at its own depth, so `provider fleet status` is verified against
// the fleet subcommands, not only against the word `fleet`.
func missingCommandRefs(content string, sets map[string]map[string]bool) []string {
	refs := []struct {
		group string
		re    *regexp.Regexp
	}{
		{"provider", regexp.MustCompile("`provider ([a-z][a-z0-9_-]*)")},
		{"provider fleet", regexp.MustCompile("`provider fleet ([a-z][a-z0-9_-]*)")},
		{"provider wallet", regexp.MustCompile("`provider wallet ([a-z][a-z0-9_-]*)")},
		{"provider wallet hotkey", regexp.MustCompile("`provider wallet hotkey ([a-z][a-z0-9_-]*)")},
	}
	var missing []string
	for _, ref := range refs {
		for _, match := range ref.re.FindAllStringSubmatch(content, -1) {
			if !sets[ref.group][match[1]] {
				missing = append(missing, ref.group+" "+match[1])
			}
		}
	}
	return missing
}

// usageCommands returns the first word after the binary name on each docopt
// usage line.
func usageCommands(usage string, binary string) map[string]bool {
	line := regexp.MustCompile(`(?m)^\s*` + binary + ` ([a-z][a-z0-9_-]*)`)
	out := map[string]bool{}
	for _, match := range line.FindAllStringSubmatch(usage, -1) {
		out[match[1]] = true
	}
	return out
}

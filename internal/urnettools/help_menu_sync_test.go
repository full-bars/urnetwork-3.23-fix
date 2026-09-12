package urnettools

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// helpMenuMentions reports whether the hand-written root help menu lists a
// command name as its own entry, rather than merely containing the word.
func helpMenuMentions(name string) bool {
	// Entries are indented and start with the command name, sometimes
	// followed by arguments: "  metrics <on|off>   Toggle ...".
	re := regexp.MustCompile(`(?m)^\s{2,}` + regexp.QuoteMeta(name) + `(\s|$)`)
	return re.MatchString(rootHelpMenu)
}

// rootHelpMenu is hand maintained, and nothing has ever checked it against
// the commands actually registered on the root. It has drifted twice:
// hotswap was registered in PR #533 and never listed, and the whole
// config/history/metrics/profile/dashboard surface added in PR #568 was
// registered and never listed either. A command missing here is invisible:
// an operator reading --help has no way to learn it exists.
//
// Commands deliberately kept out of the menu belong in the exemption list
// below, with a reason, so the omission is a decision rather than a
// mistake nobody noticed.
func TestRootHelpMenuListsEveryRegisteredCommand(t *testing.T) {
	hiddenByDesign := map[string]string{
		"help":        "cobra builtin, the menu is the help",
		"completion":  "cobra builtin shell completion",
		"urnet-tools": "the root command itself",
		"idle-update": "internal, driven by the update timer",
		"uninstall":   "destructive, deliberately undiscoverable",
		"reinstall":   "recovery path, documented separately",
		"auto-start":  "installer plumbing",
		"auto-update": "installer plumbing",
	}

	var missing []string
	for _, c := range buildRootCmd().Commands() {
		name := c.Name()
		if _, ok := hiddenByDesign[name]; ok {
			continue
		}
		if c.Hidden {
			continue
		}
		if !helpMenuMentions(name) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Errorf("registered but absent from rootHelpMenu: %s\n"+
			"Add an entry to rootHelpMenu, or add the command to hiddenByDesign with a reason.",
			strings.Join(missing, ", "))
	}
}

// The converse drift: an entry describing a command that no longer exists
// sends an operator to a command that will not run.
func TestRootHelpMenuHasNoEntriesForMissingCommands(t *testing.T) {
	registered := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		registered[c.Name()] = true
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	root := buildRootCmd()
	// Cobra adds its builtin help command lazily, on execute. Without this
	// the walk misses it and the menu's own "help" entry reads as stale.
	root.InitDefaultHelpCmd()
	walk(root)

	entry := regexp.MustCompile(`(?m)^\s{2,}([a-z][a-z0-9-]*)`)
	var stale []string
	for _, m := range entry.FindAllStringSubmatch(rootHelpMenu, -1) {
		name := m[1]
		if !registered[name] {
			stale = append(stale, name)
		}
	}

	if len(stale) > 0 {
		t.Errorf("rootHelpMenu lists commands that are not registered: %s", strings.Join(stale, ", "))
	}
}

package urnettools

// Display-only flag metadata for the help pages.
//
// Every urnet-tools command sets DisableFlagParsing and parses its own flags
// by hand (see newCobraCmd). That keeps -f/--force, the targeting flags and
// every command-specific flag out of pflag's hands, but it also means cobra
// has nothing to render under "Flags:" on any page. This file adds a way to
// declare a command's flags as metadata: the declarations are attached to the
// cobra command so its help page renders a real "Flags:" section, and cobra
// never parses them; the handlers remain the only consumers.
//
// The declarations must mirror what the handler actually accepts. The test in
// cobra_help_audit_test.go cross-checks the declarations against the usage
// and example text, and parse-level tests cover the commands whose parsers
// have seams.

import (
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"
)

// helpFlag declares one hand-parsed flag for help rendering.
type helpFlag struct {
	// Name is the flag without dashes, e.g. "tag" or "dry-run".
	Name string `json:"name"`
	// Shorthand is the one-letter alias without a dash, e.g. "j"; empty
	// when the flag has none.
	Shorthand string `json:"shorthand,omitempty"`
	// Value is the value placeholder for a valued flag ("TAG", "SECONDS");
	// empty for a boolean flag.
	Value string `json:"value,omitempty"`
	// Usage is the one-line description shown next to the flag. No trailing
	// period (cobra's own style).
	Usage string `json:"usage"`
	// Default is the default as shown to the reader; empty when none. For
	// booleans use "true"/"false"; for valued flags the literal default.
	Default string `json:"default,omitempty"`
}

// helpFlagsAnnotationKey carries the declared flags on the cobra command so
// tests can walk the tree and cross-check declarations without a side table.
const helpFlagsAnnotationKey = "urnettools/declared-help-flags"

// withFlags attaches display-only flag metadata to a command. The command
// keeps DisableFlagParsing; cobra only uses these flags to render the help
// page's "Flags:" section.
func withFlags(cmd *cobra.Command, flags ...helpFlag) *cobra.Command {
	if len(flags) == 0 {
		return cmd
	}
	for _, f := range flags {
		usage := f.Usage
		if f.Value != "" && !strings.Contains(usage, "`") {
			// pflag renders a backquoted word as the value placeholder and
			// keeps it in the usage text; append it when the author did not
			// place one.
			usage = strings.TrimSuffix(strings.TrimRight(usage, " "), ".") + " `" + f.Value + "`"
		}
		if f.Value == "" {
			if f.Shorthand != "" {
				cmd.Flags().BoolP(f.name(), f.Shorthand, f.Default == "true", usage)
			} else {
				cmd.Flags().Bool(f.name(), f.Default == "true", usage)
			}
			continue
		}
		def := ""
		if f.Default != "" && f.Default != "none" {
			def = f.Default
		}
		if f.Shorthand != "" {
			cmd.Flags().StringP(f.name(), f.Shorthand, def, usage)
		} else {
			cmd.Flags().String(f.name(), def, usage)
		}
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	// Merge with anything a previous wrapper declared, so chaining
	// withTargetFlags/withSafetyFlags/withFlags accumulates instead of
	// clobbering.
	existing := []helpFlag{}
	if raw, ok := cmd.Annotations[helpFlagsAnnotationKey]; ok {
		_ = json.Unmarshal([]byte(raw), &existing)
	}
	encoded, err := json.Marshal(append(existing, flags...))
	if err == nil {
		cmd.Annotations[helpFlagsAnnotationKey] = string(encoded)
	}
	return cmd
}

// targetHelpFlags are the provider-targeting flags the shared target parser
// accepts for every targeting command (whoever parses, cobra never does).
var targetHelpFlags = []helpFlag{
	{Name: "unit", Value: "UNIT", Usage: "systemd `UNIT`, e.g. urnetwork-native.service"},
	{Name: "user", Value: "USER", Usage: "OS `USER`, e.g. urnet"},
	{Name: "network", Value: "NAME", Usage: "JWT network name `NAME` (account identity)"},
	{Name: "network-id", Value: "ID", Usage: "JWT network id `ID` (true unique identity)"},
	{Name: "state-dir", Value: "DIR", Usage: "provider state `DIR`"},
}

// safetyHelpFlags are the confirmation-safety flags the shared global parser
// accepts (and the root page documents).
var safetyHelpFlags = []helpFlag{
	{Name: "force", Shorthand: "f", Usage: "skip confirmation prompts ONLY (never picks providers)"},
	{Name: "dry-run", Shorthand: "n", Usage: "print the plan, change nothing"},
}

// withTargetFlags declares the targeting flags on a command that targets a
// provider.
func withTargetFlags(cmd *cobra.Command) *cobra.Command {
	return withFlags(cmd, targetHelpFlags...)
}

// withSafetyFlags declares -f/--force and -n/--dry-run on a command that
// honors them.
func withSafetyFlags(cmd *cobra.Command) *cobra.Command {
	return withFlags(cmd, safetyHelpFlags...)
}

// withTargetAndSafetyFlags declares both sets.
func withTargetAndSafetyFlags(cmd *cobra.Command) *cobra.Command {
	return withFlags(cmd, append(append([]helpFlag{}, targetHelpFlags...), safetyHelpFlags...)...)
}

// name returns the flag's name without dashes.
func (f helpFlag) name() string {
	return strings.TrimLeft(f.Name, "-")
}

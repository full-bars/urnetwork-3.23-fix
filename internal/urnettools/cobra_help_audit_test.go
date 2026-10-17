package urnettools

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestCobraHelpAudit walks the whole cobra tree and fails on the help gaps
// that make a command invisible or unusable: an empty Short (the one-line
// index), an empty Long (the command's own page), a leaf command with no
// Example (real invocations), a flag with no Usage text, and duplicate
// command names among siblings. Cobra's own builtins (help, completion) are
// exempt: their pages are the framework's.
func TestCobraHelpAudit(t *testing.T) {
	builtinExempt := map[string]string{
		"help":       "cobra builtin",
		"completion": "cobra builtin",
	}

	var gaps []string
	var walk func(path string, command *cobra.Command)
	walk = func(path string, command *cobra.Command) {
		name := command.Name()
		if path != "" {
			name = path + " " + command.Name()
		}
		if _, exempt := builtinExempt[command.Name()]; !exempt && !command.Hidden {
			if strings.TrimSpace(command.Short) == "" {
				gaps = append(gaps, name+": empty Short")
			}
			if strings.TrimSpace(command.Long) == "" {
				gaps = append(gaps, name+": empty Long")
			}
			subcommands := command.Commands()
			isLeaf := len(subcommands) == 0
			// the root itself carries the curated menu, not an Example
			if isLeaf && path != "" && strings.TrimSpace(command.Example) == "" {
				gaps = append(gaps, name+": missing Example")
			}
			checkFlags := func(flags *pflag.FlagSet) {
				if flags == nil {
					return
				}
				flags.VisitAll(func(flag *pflag.Flag) {
					if flag.Hidden {
						return
					}
					if strings.TrimSpace(flag.Usage) == "" {
						gaps = append(gaps, name+" --"+flag.Name+": flag without Usage")
					}
				})
			}
			checkFlags(command.LocalFlags())

			// Flag text must be declared: every --flag named in the page's
			// Long or Example needs a display-only declaration (or a
			// documented exemption), otherwise the page promises a flag the
			// Flags section never renders.
			if _, exempt := flagTextExemptions[command.Name()]; !exempt {
				declared := declaredFlagNames(command)
				for _, tok := range flagTokensInText(command.Long + "\n" + command.Example) {
					if globalFlagText[tok] || declared[tok] {
						continue
					}
					gaps = append(gaps, name+": text names --"+tok+" but the page declares no such flag")
				}
			}

			seen := map[string]bool{}
			for _, sub := range subcommands {
				if seen[sub.Name()] {
					gaps = append(gaps, name+": duplicate command name "+sub.Name())
				}
				seen[sub.Name()] = true
			}
		}
		for _, sub := range command.Commands() {
			walk(name, sub)
		}
	}
	walk("", buildRootCmd())

	if len(gaps) > 0 {
		sort.Strings(gaps)
		t.Errorf("cobra help audit: %d gaps\n  %s", len(gaps), strings.Join(gaps, "\n  "))
	}
}

// globalFlagText names the flags the shared helpers parse for every command.
// They are documented on the root page; a per-command page needs no
// declaration for them (a command-specific declaration is still fine).
var globalFlagText = map[string]bool{
	"force": true, "yes": true, "dry-run": true, "help": true,
	"unit": true, "user": true, "network": true, "network-id": true, "state-dir": true,
}

// flagTextExemptions documents commands whose Long/Example text names flags
// that are not declared on their own page, with the reason. Keep this list
// short and justified; the proxy subcommand pages will remove the proxy entry.
var flagTextExemptions = map[string]string{
	"proxy":  "the proxy subcommand pages (and their flags) land in a separate change; the parent page documents them in prose until then",
	"direct": "the text names the provider binary's own launch flags (--proxy_file/--proxy_url), not flags this command accepts",
}

// declaredFlagNames returns the flag names declared with withFlags for a
// command, read back from the annotation the declaration writes.
func declaredFlagNames(command *cobra.Command) map[string]bool {
	out := map[string]bool{}
	if command.Annotations == nil {
		return out
	}
	var flags []helpFlag
	if err := json.Unmarshal([]byte(command.Annotations[helpFlagsAnnotationKey]), &flags); err == nil {
		for _, f := range flags {
			out[strings.TrimLeft(f.Name, "-")] = true
		}
	}
	return out
}

// flagTokensInText extracts the --flag tokens named in help prose.
func flagTokensInText(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range regexp.MustCompile(`--([a-z][a-z0-9_-]*)`).FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// TestHelpFlagDeclarationsMatchParsers cross-checks the withFlags
// declarations against the source of the functions that parse them: every
// declared flag must appear in its parser's source, and every --flag literal
// in that parser must be declared (shared globals aside). The check reads
// source text; no handler is ever executed against the host.
func TestHelpFlagDeclarationsMatchParsers(t *testing.T) {
	type check struct {
		path  string // cobra path, e.g. "update"
		file  string // source file in this package
		funcs []string
		// forward marks a command whose flags are forwarded to the provider
		// binary by design; the provider owns the parser, so only the
		// declaration side is documented, not source-verified here.
		forward bool
	}
	checks := []check{
		{"update", "update.go", []string{"cmdUpdate"}, false},
		{"self-update", "update.go", []string{"cmdSelfUpdate"}, false},
		{"idle-update", "idle_update.go", []string{"parseIdleArgs"}, false},
		{"top", "top_cmd.go", []string{"parseTopFlags"}, false},
		{"config", "config.go", []string{"runConfig"}, false},
		{"sn-status", "sn_status.go", []string{"cmdSnStatus"}, false},
		{"status", "cli.go", []string{"cmdStatus"}, false},
		{"providers", "cli.go", []string{"cmdProviders"}, false},
		{"history", "cobra.go", []string{"parseHistoryArgs"}, false},
		{"baseline", "baseline.go", []string{"parseBaselineArgs"}, false},
		{"session", "session_cmds.go", []string{"cmdSession"}, false},
		{"choose-network", "restore_delegate.go", []string{"cmdChooseNetwork"}, false},
		{"wallet challenge", "wallet.go", []string{"cmdWalletChallenge"}, false},
		{"wallet set", "wallet.go", []string{"cmdWalletSet"}, false},
		{"wallet sign", "wallet.go", []string{"cmdWalletSign"}, false},
		{"auth", "", nil, true},
	}

	// The shared parsers own the global flag sets; verify the declarations
	// against them once, here.
	globalNames := map[string]bool{
		"unit": true, "user": true, "network": true, "network-id": true,
		"state-dir": true, "force": true, "dry-run": true,
	}
	sharedTarget := funcSource(t, "cli.go", "parseTargetFlagsInner")
	for _, f := range targetHelpFlags {
		if !strings.Contains(sharedTarget, `"`+"--"+strings.TrimLeft(f.Name, "-")+`"`) {
			t.Errorf("shared target parser lacks --%s", f.Name)
		}
	}
	sharedGlobal := funcSource(t, "cli.go", "parseGlobalFlags")
	for _, f := range safetyHelpFlags {
		if !strings.Contains(sharedGlobal, `"`+strings.TrimLeft(f.Name, "-")+`"`) &&
			!strings.Contains(sharedGlobal, `--`+strings.TrimLeft(f.Name, "-")) {
			t.Errorf("shared global parser lacks --%s", f.Name)
		}
	}

	for _, c := range checks {
		command := findCommandByPath(t, c.path)
		if command == nil {
			t.Errorf("%s: command not found", c.path)
			continue
		}
		declared := declaredFlagNames(command)
		if len(declared) == 0 && !c.forward {
			t.Errorf("%s: no declared flags", c.path)
			continue
		}
		if c.forward {
			continue
		}
		body := funcSource(t, c.file, c.funcs...)
		for name := range declared {
			if globalNames[name] {
				continue
			}
			if strings.Contains(body, `"`+name+`"`) || strings.Contains(body, `--`+name) {
				continue
			}
			// A declared flag may also be a documented passthrough: it must
			// then be named in the command's own Long or Example text.
			if strings.Contains(command.Long+command.Example, "--"+name) {
				continue
			}
			t.Errorf("%s: declared --%s is not in the parser source (%s %v)", c.path, name, c.file, c.funcs)
		}
		for name := range flagLiteralsInSource(t, c.file, c.funcs...) {
			if declared[name] || globalFlagText[name] {
				continue
			}
			t.Errorf("%s: the parser accepts --%s but the page does not declare it", c.path, name)
		}
	}
}

// findCommandByPath resolves a "a b c" path from the built root.
func findCommandByPath(t *testing.T, path string) *cobra.Command {
	t.Helper()
	parts := strings.Fields(path)
	current := buildRootCmd()
	for _, part := range parts {
		var next *cobra.Command
		for _, sub := range current.Commands() {
			if sub.Name() == part {
				next = sub
				break
			}
		}
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

// funcSource returns the source text of the named functions (whole bodies).
func funcSource(t *testing.T, file string, names ...string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	text := string(raw)
	var out strings.Builder
	for _, name := range names {
		idx := strings.Index(text, "func "+name+"(")
		if idx < 0 {
			re := regexp.MustCompile(`func \([^)]*\) ` + regexp.QuoteMeta(name) + `\(`)
			if loc := re.FindStringIndex(text); loc != nil {
				idx = loc[0]
			}
		}
		if idx < 0 {
			t.Fatalf("%s: function %s not found", file, name)
		}
		rest := text[idx:]
		if next := regexp.MustCompile(`(?m)^func `).FindStringIndex(rest[1:]); next != nil {
			rest = rest[:1+next[0]]
		}
		out.WriteString(rest)
	}
	return out.String()
}

// flagLiteralsInSource extracts the --flag literals a parser body names.
func flagLiteralsInSource(t *testing.T, file string, names ...string) map[string]bool {
	t.Helper()
	body := funcSource(t, file, names...)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"--([a-z][a-z0-9_-]*)"`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

package urnettools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The help pages list each command's flags from the cobra registration, while
// the commands read their arguments with hand-written parsers. These tests read
// the parsers' source and fail when the two lists differ, so a flag cannot be
// added to a parser without appearing in --help, or listed in --help without
// being accepted.

type parserSource struct {
	file string
	fn   string
}

var (
	longFlagLiteral  = regexp.MustCompile(`^(--[a-z][a-z0-9-]*)(=.*)?$`)
	shortFlagLiteral = regexp.MustCompile(`^-[a-zA-Z]$`)
)

// parserFlags returns the flags named by string literals in the given parser
// functions: "--name" and "--name=..." as "--name", and "-x" as "-x". Error
// messages are not flags, so only whole literals of those shapes count.
func parserFlags(t *testing.T, sources ...parserSource) map[string]bool {
	t.Helper()
	flags := map[string]bool{}
	for _, src := range sources {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, src.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src.file, err)
		}
		found := false
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != src.fn || fd.Body == nil {
				continue
			}
			found = true
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if m := longFlagLiteral.FindStringSubmatch(value); m != nil {
					flags[m[1]] = true
				} else if shortFlagLiteral.MatchString(value) {
					flags[value] = true
				}
				return true
			})
		}
		if !found {
			t.Fatalf("function %s not found in %s", src.fn, src.file)
		}
	}
	return flags
}

// registeredFlags returns the cobra flags of a set as "--name" and "-x".
func registeredFlags(set *pflag.FlagSet) map[string]bool {
	flags := map[string]bool{}
	set.VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		flags["--"+f.Name] = true
		if f.Shorthand != "" {
			flags["-"+f.Shorthand] = true
		}
	})
	return flags
}

func diffFlags(parser, registered, ignoreParser map[string]bool) (missing, extra []string) {
	for f := range parser {
		if !registered[f] && !ignoreParser[f] {
			missing = append(missing, f)
		}
	}
	for f := range registered {
		if !parser[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func TestHelpFlagsMatchTheParsers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command func() *cobra.Command
		sources []parserSource
		// accepted by the parser but deliberately not listed: aliases of a
		// listed flag and hidden developer flags
		unlisted []string
	}{
		{"status", newStatusCmd, []parserSource{{"cli.go", "cmdStatus"}}, []string{"--full"}},
		{"top", newTopCmd, []parserSource{
			{"top_cmd.go", "cmdTop"}, {"top_cmd.go", "parseTopFlags"},
			{"top_cmd.go", "hasNoMouseFlag"}, {"top_demo.go", "stripDemoFlag"},
		}, []string{"--demo"}},
		{"providers", newProvidersCmd, []parserSource{{"cli.go", "cmdProviders"}}, nil},
		{"start", newStartCmd, []parserSource{{"legacy_cmds.go", "cmdStart"}}, nil},
		{"stop", newStopCmd, []parserSource{{"legacy_cmds.go", "cmdStop"}}, nil},
		{"restart", newRestartCmd, []parserSource{{"legacy_cmds.go", "cmdRestart"}}, nil},
		{"update", newUpdateCmd, []parserSource{{"update.go", "cmdUpdate"}}, nil},
		{"logs", newLogsCmd, []parserSource{{"legacy_cmds.go", "cmdLogs"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ignore := map[string]bool{}
			for _, f := range tc.unlisted {
				ignore[f] = true
			}
			missing, extra := diffFlags(parserFlags(t, tc.sources...), registeredFlags(tc.command().Flags()), ignore)
			if len(missing) > 0 {
				t.Errorf("the parser accepts %v but --help does not list them", missing)
			}
			if len(extra) > 0 {
				t.Errorf("--help lists %v but the parser does not accept them", extra)
			}
		})
	}
}

// The target and safety flags are shared by every command.
func TestHelpGlobalFlagsMatchTheParsers(t *testing.T) {
	root := buildRootCmd()
	// -y and --yes are listed inside the -f description, -h and --help are
	// cobra's own.
	ignore := map[string]bool{"-y": true, "--yes": true, "-h": true, "--help": true}
	missing, extra := diffFlags(
		parserFlags(t, parserSource{"cli.go", "parseGlobalFlags"}, parserSource{"cli.go", "parseTargetFlagsInner"}),
		registeredFlags(root.PersistentFlags()), ignore)
	if len(missing) > 0 {
		t.Errorf("the parsers accept %v but --help does not list them", missing)
	}
	if len(extra) > 0 {
		t.Errorf("--help lists %v but the parsers do not accept them", extra)
	}
}

// Every listed flag needs a description, and no help line may run past 80
// columns, so a page stays readable in a plain terminal.
func TestHelpPagesAreReadable(t *testing.T) {
	root := buildRootCmd()
	for _, c := range root.Commands() {
		if c.Hidden {
			continue
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Usage == "" {
				t.Errorf("%s: flag --%s has no description", c.Name(), f.Name)
			}
		})
	}
	for _, name := range []string{"status", "top", "providers", "start", "stop", "restart", "update", "logs"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		cmd.SetOut(&out)
		if err := cmd.Help(); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if n := len([]rune(line)); n > 80 {
				t.Errorf("%s help has a %d column line: %q", name, n, line)
			}
			// a flag such as --dry-run must not be split across two lines
			if strings.HasSuffix(line, "-") && !strings.HasSuffix(line, " -") {
				t.Errorf("%s help splits a word at a hyphen: %q", name, line)
			}
		}
	}
}

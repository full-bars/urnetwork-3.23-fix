package urnettools

// Both help paths must work for every command: `help <command>` prints a
// page, `<command> --help` prints a page, and -h is exactly the same output
// as --help. The two paths may legitimately render different sources (the
// cobra tree for `help`, the command's own printer for `--help`), so the
// check is that both produce output and that -h never diverges from --help.
//
// version is the documented exception: -h on it is intercepted at the Run
// level to print the tool version (see TestVersionHelpIsRootIntercepted), so
// it is skipped here. Cobra's builtins carry the framework's own pages.
//
// Output is captured at the file-descriptor level, because several commands
// print their help directly (fmt.Printf / os.Stderr) rather than through the
// cobra writer, and a buffer attached to the command would miss them.

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// helpCommandPaths walks a root and returns every non-hidden command path.
func helpCommandPaths(root *cobra.Command) [][]string {
	var paths [][]string
	var walk func(path []string, cmd *cobra.Command)
	walk = func(path []string, cmd *cobra.Command) {
		if len(path) > 0 {
			paths = append(paths, append([]string{}, path...))
		}
		for _, sub := range cmd.Commands() {
			walk(append(path, sub.Name()), sub)
		}
	}
	for _, sub := range root.Commands() {
		name := sub.Name()
		if name == "help" || name == "completion" || sub.Hidden || name == "version" {
			continue
		}
		walk([]string{name}, sub)
	}
	return paths
}

// captureProcessOutput runs fn with stdout and stderr pointed at a pipe and
// returns everything written.
func captureProcessOutput(t *testing.T, fn func()) string {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	fn()
	w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// helpPageOutput executes one argv against a freshly built root and returns
// the printed page. The root is built inside the capture window so its
// writers bind to the captured file descriptors.
func helpPageOutput(t *testing.T, build func() *cobra.Command, argv ...string) string {
	t.Helper()
	var err error
	out := captureProcessOutput(t, func() {
		root := build()
		root.SetArgs(argv)
		err = root.Execute()
	})
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return out
}

func checkHelpPathsAgree(t *testing.T, name string, build func() *cobra.Command) {
	t.Helper()
	for _, path := range helpCommandPaths(build()) {
		joined := strings.Join(path, " ")
		withHelp := append(append([]string{}, path...), "--help")
		withH := append(append([]string{}, path...), "-h")

		dashHelp := helpPageOutput(t, build, withHelp...)
		if strings.TrimSpace(dashHelp) == "" {
			t.Errorf("%s %s --help printed nothing", name, joined)
			continue
		}
		if hOut := helpPageOutput(t, build, withH...); hOut != dashHelp {
			t.Errorf("%s: `%s -h` and `%s --help` render different output", name, joined, joined)
		}
		if helpOut := helpPageOutput(t, build, append([]string{"help"}, path...)...); strings.TrimSpace(helpOut) == "" {
			t.Errorf("%s help %s printed nothing", name, joined)
		}
	}
}

func TestHelpCommandAndDashHelpAgree(t *testing.T) {
	checkHelpPathsAgree(t, "urnet-tools", buildRootCmd)
	checkHelpPathsAgree(t, "urnet-docker", buildDockerRootCmd)
}

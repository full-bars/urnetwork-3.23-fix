package urnettools

import (
	"os"
	"strings"
	"testing"
)

func TestAliasNames(t *testing.T) {
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	for _, name := range []string{"ut", "urnet-tools", "urnet-docker"} {
		os.Args = []string{name}
		if got := programName(); got != name {
			t.Errorf("programName() = %q, want %q", got, name)
		}
	}
	if got := toolAssetName("ut", "windows", "amd64"); got != "urnet-tools-windows-amd64" {
		t.Errorf("asset = %q", got)
	}
}

func TestAliasCompletionStdout(t *testing.T) {
	oldArgs, oldOut := os.Args, os.Stdout
	out, err := os.CreateTemp(t.TempDir(), "completion")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldOut; out.Close() })
	os.Args, os.Stdout = []string{"ut"}, out
	if err := Run([]string{"completion", "bash"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out.Name())
	if err != nil || !strings.Contains(string(data), "complete -o default -F __start_ut ut") {
		t.Fatalf("completion not written to stdout: %v", err)
	}
}

func TestAliasCobraName(t *testing.T) {
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{"ut"}
	root := buildRootCmd()
	if got := root.Name(); got != "ut" {
		t.Fatalf("root name = %q", got)
	}
	if got := root.HelpTemplate(); !strings.Contains(got, "Usage:\n  ut [command]") {
		t.Fatalf("help does not use alias")
	}
	cmd, _, err := root.Find([]string{"update"})
	if err != nil || !strings.Contains(cmd.Example, "ut update") || strings.Contains(cmd.Example, "urnet-tools update") {
		t.Fatalf("update examples do not use alias: %q, %v", cmd.Example, err)
	}
	var completion strings.Builder
	if err := root.GenBashCompletion(&completion); err != nil || !strings.Contains(completion.String(), "complete -o default -F __start_ut ut") {
		t.Fatalf("completion does not register ut: %v", err)
	}
}

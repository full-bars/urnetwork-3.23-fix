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
}

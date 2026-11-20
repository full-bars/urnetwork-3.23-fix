package urnettools

import (
	"os"
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

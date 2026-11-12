package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBSDScriptServesBinary(t *testing.T) {
	const bin = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
	for _, tc := range []struct {
		name, script string
		want         bool
	}{
		{"exact", "#!/bin/ksh\ndaemon=\"" + bin + " provide\"\ndaemon_user=\"u\"\n", true},
		{"indented", "  daemon=\"" + bin + " provide\"  \n", true},
		{"other binary", "daemon=\"/home/u/urnetwork provide\"\n", false},
		{"prefix only", "daemon=\"" + bin + "-beta provide\"\n", false},
		{"commented", "# daemon=\"" + bin + " provide\"\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openbsdScriptServesBinary(tc.script, bin); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpenBSDCheckScript(t *testing.T) {
	const bin = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork")
	if err := openbsdCheckScript(path, bin); err == nil {
		t.Fatal("missing script accepted")
	}
	if err := os.WriteFile(path, []byte("daemon=\""+bin+" provide\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := openbsdCheckScript(path, bin); err != nil {
		t.Fatalf("matching script rejected: %v", err)
	}
	if err := openbsdCheckScript(path, "/tmp/other"); err == nil {
		t.Fatal("script for another binary accepted")
	}
	if err := openbsdCheckScript(path, ""); err == nil {
		t.Fatal("empty binary accepted")
	}
}

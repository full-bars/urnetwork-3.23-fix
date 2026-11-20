package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerUTAlias(t *testing.T) {
	for _, platform := range []string{"Linux", "Mac", "FreeBSD", "OpenBSD"} {
		t.Run(platform, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "Provider_Install_"+platform+".sh"))
			if err != nil {
				t.Fatal(err)
			}
			script := string(data)
			start := strings.Index(script, "ensure_ut_alias() {")
			if start < 0 {
				t.Fatal("alias function missing")
			}
			end := strings.Index(script[start:], "\n}\n")
			if end < 0 {
				t.Fatal("alias function end missing")
			}
			fn := script[start : start+end+3]
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "bin", "urnet-tools"), []byte("tool"), 0755); err != nil {
				t.Fatal(err)
			}
			run := func(disable bool) {
				t.Helper()
				c := exec.Command("sh", "-c", "priv() { \"$@\"; }\n"+fn+"\nensure_ut_alias", "test")
				c.Env = append(os.Environ(), "install_path="+dir)
				if disable {
					c.Env = append(c.Env, "URNETWORK_NO_UT_ALIAS=1")
				} else {
					c.Env = append(c.Env, "URNETWORK_NO_UT_ALIAS=0")
				}
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			}
			alias := filepath.Join(dir, "bin", "ut")
			run(true)
			if _, err := os.Lstat(alias); !os.IsNotExist(err) {
				t.Fatal("opt-out created alias")
			}
			run(false)
			if got, err := os.Readlink(alias); err != nil || got != "urnet-tools" {
				t.Fatalf("link %q: %v", got, err)
			}
			run(false)
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(alias, []byte("foreign"), 0755); err != nil {
				t.Fatal(err)
			}
			run(false)
			if got, _ := os.ReadFile(alias); string(got) != "foreign" {
				t.Fatal("foreign file changed")
			}
		})
	}
}

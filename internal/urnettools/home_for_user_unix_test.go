//go:build linux || freebsd

package urnettools

import (
	osuser "os/user"
	"testing"
)

// Under `sudo` with HOME preserved, $HOME names the invoking user's home, not
// the account's. The passwd entry must win for the current user too, as it did
// when homeForUser was getent only.
func TestHomeForUserPrefersPasswdOverHomeEnv(t *testing.T) {
	name := currentUserName()
	u, err := osuser.Lookup(name)
	if err != nil || u.HomeDir == "" {
		t.Skipf("no passwd entry for the current user %q", name)
	}
	t.Setenv("HOME", t.TempDir())
	if got := homeForUser(name); got != u.HomeDir {
		t.Fatalf("homeForUser(%q) = %q, want the passwd home %q", name, got, u.HomeDir)
	}
}

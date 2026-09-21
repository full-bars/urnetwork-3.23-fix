package main

import "testing"

// TestVersionDefault: Version must default to "dev" when the binary is not
// built via release.yml's ldflags (-X main.Version=...). The self-update
// path and release tooling depend on this var existing and being a plain
// string, so a build without the ldflag override must still produce a
// sane, non-empty value rather than a zero value.
func TestVersionDefault(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if Version != "dev" {
		t.Errorf("Version = %q, want %q (unless overridden by -ldflags, which this test run did not do)", Version, "dev")
	}
}

// urtop is a link to the same binary; it must behave as `urnet-tools top`,
// keeping whatever arguments follow.
func TestArgsForInvocation(t *testing.T) {
	cases := []struct {
		argv0 string
		args  []string
		want  []string
	}{
		{"urnet-tools", []string{"status"}, []string{"status"}},
		{"/usr/local/bin/urnet-tools", nil, []string{}},
		{"urtop", nil, []string{"top"}},
		{"/usr/local/bin/urtop", []string{"--network", "x"}, []string{"top", "--network", "x"}},
		{`C:\tools\urtop.exe`, []string{"--interval", "2s"}, []string{"top", "--interval", "2s"}},
		{"URTOP.EXE", nil, []string{"top"}},
		{"./urtop", []string{"top"}, []string{"top", "top"}},
		{"urtopper", nil, []string{}},
		{"", []string{"logs"}, []string{"logs"}},
	}
	for _, c := range cases {
		got := argsForInvocation(c.argv0, c.args)
		if len(got) != len(c.want) {
			t.Errorf("%q %v: got %v want %v", c.argv0, c.args, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q %v: got %v want %v", c.argv0, c.args, got, c.want)
				break
			}
		}
	}
}

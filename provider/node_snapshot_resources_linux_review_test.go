//go:build linux

package main

import "testing"

func TestPidsCgroupPath(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"0::/user.slice/session.scope", "/user.slice/session.scope"},
		{"4:cpu,pids:/test", "/test"},
		{"5:cpu:/wrong", ""},
		{"bad", ""},
	} {
		if got := pidsCgroupPath(c.line); got != c.want {
			t.Errorf("%q: %q", c.line, got)
		}
	}
}

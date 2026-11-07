package main

import "testing"

func TestParsePortHolder(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			"caddy on 443",
			`LISTEN 0 4096 *:443 *:* users:(("caddy",pid=894,fd=7))` + "\n",
			`caddy (pid 894)`,
		},
		{
			"several listeners keep the first",
			`LISTEN 0 4096 0.0.0.0:443 0.0.0.0:* users:(("nginx",pid=12,fd=6),("nginx",pid=13,fd=6))` + "\n",
			`nginx (pid 12)`,
		},
		{"no users column, not our process", `LISTEN 0 4096 *:443 *:*` + "\n", ""},
		{"empty", "", ""},
		{"garbage", "ss: command failed\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePortHolder(tc.out); got != tc.want {
				t.Errorf("parsePortHolder = %q, want %q", got, tc.want)
			}
		})
	}
}

package urnettools

import (
	"errors"
	"testing"
)

func TestOpenBSDManagedPID(t *testing.T) {
	for _, tc := range []struct {
		name, runfile, output string
		lookupErr             error
		want                  int
	}{
		{"unique", "daemon_rtable=4\npexp=/usr/local/bin/urnetwork provide -x\n", "123\n", nil, 123},
		{"multiple", "daemon_rtable=4\npexp=provider\n", "123\n456\n", nil, -1},
		{"missing expression", "daemon_rtable=4\n", "123", nil, -1},
		{"missing table", "pexp=provider\n", "123", nil, -1},
		{"invalid table", "daemon_rtable=-1\npexp=provider\n", "123", nil, -1},
		{"duplicate expression", "daemon_rtable=0\npexp=a\npexp=b\n", "123", nil, -1},
		{"bad pid", "daemon_rtable=0\npexp=a\n", "no", nil, -1},
		{"lookup failure", "daemon_rtable=0\npexp=a\n", "123", errors.New("failed"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := openbsdManagedPID(tc.runfile, func(table, pattern string) ([]byte, error) {
				if tc.name == "unique" && (table != "4" || pattern != "/usr/local/bin/urnetwork provide -x") {
					t.Fatalf("lookup got %q %q", table, pattern)
				}
				return []byte(tc.output), tc.lookupErr
			})
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

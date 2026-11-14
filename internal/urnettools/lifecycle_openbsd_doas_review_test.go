package urnettools

import (
	"errors"
	"os"
	"testing"
)

func TestOpenBSDDoasConfigurationSelection(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		lookupErr, configErr error
		want                 string
	}{
		{"configured", nil, nil, "doas"},
		{"missing", nil, os.ErrNotExist, "sudo"},
		{"unreadable", nil, os.ErrPermission, "doas"},
		{"other stat error", nil, errors.New("unknown"), "doas"},
		{"not installed", errors.New("missing executable"), nil, "sudo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, _, err := openbsdRcctlArgv(1000, openbsdDoasUsable(tc.lookupErr, tc.configErr), true, "restart", "urnetwork")
			if err != nil || name != tc.want {
				t.Fatalf("name = %q, err = %v, want %q", name, err, tc.want)
			}
		})
	}
}

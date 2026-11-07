package urnettools

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderSetupConfirmation(t *testing.T) {
	plan := []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", "/path with spaces/provider"}}, {name: "ufw", args: []string{"allow", "443/udp"}}}
	for _, tc := range []struct {
		name                 string
		yes, dryRun, confirm bool
		confirmErr           error
		wantCalls            int
		wantErr              bool
	}{
		{name: "non-interactive", confirmErr: errors.New("not a terminal"), wantErr: true},
		{name: "declined", wantErr: true},
		{name: "confirmed", confirm: true, wantCalls: 2},
		{name: "explicit yes", yes: true, wantCalls: 2},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var ran []setupCommand
			err := executeExtenderSetupPlan(&out, plan, tc.yes, tc.dryRun, func() (bool, error) { return tc.confirm, tc.confirmErr }, func(c setupCommand) error {
				ran = append(ran, c)
				return nil
			})
			if (err != nil) != tc.wantErr || len(ran) != tc.wantCalls {
				t.Fatalf("err=%v commands=%v", err, ran)
			}
			want := "Commands to run:\n  setcap cap_net_bind_service=+ep '/path with spaces/provider'\n  ufw allow 443/udp\n"
			if out.String() != want {
				t.Fatalf("printed %q, want %q", out.String(), want)
			}
			if tc.wantCalls > 0 && !reflect.DeepEqual(ran, plan) {
				t.Fatalf("executed %v, want %v", ran, plan)
			}
			if tc.confirmErr != nil && !strings.Contains(err.Error(), "--yes") {
				t.Fatalf("unclear error: %v", err)
			}
		})
	}
}

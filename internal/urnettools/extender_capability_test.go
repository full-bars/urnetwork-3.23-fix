package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExtenderSetupCapabilityPlan(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(binary, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, output, status string
		runErr                     error
		want                       []setupCommand
		wantErr                    bool
	}{
		{name: "missing", path: binary, status: "CapBnd:\t0000000000000400\n", want: []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", binary}}}},
		{name: "present", path: binary, output: binary + " cap_net_bind_service=ep\n"},
		{name: "other capabilities", path: binary, output: binary + " cap_net_raw=ep\n", wantErr: true},
		{name: "blocked", path: binary, status: "CapBnd:\t0000000000000000\n", wantErr: true},
		{name: "unknown bounding set", path: binary, wantErr: true},
		{name: "getcap error", path: binary, runErr: errors.New("unavailable"), wantErr: true},
		{name: "relative", path: "provider", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			plan, err := extenderCapabilityPlan(tc.path, []byte(tc.status), func(name string, args ...string) ([]byte, error) {
				calls++
				if name != "getcap" || !reflect.DeepEqual(args, []string{tc.path}) {
					t.Fatalf("unexpected check: %s %v", name, args)
				}
				return []byte(tc.output), tc.runErr
			})
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(plan, tc.want) {
				t.Fatalf("plan=%v error=%v", plan, err)
			}
			if tc.name == "relative" && calls != 0 {
				t.Fatalf("invalid path reached runner")
			}
		})
	}
}

func TestExtenderSetupCapabilityRejectsSymlink(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(binary, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	link := binary + "-link"
	if err := os.Symlink(binary, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := extenderCapabilityPlan(link, nil, func(string, ...string) ([]byte, error) {
		t.Fatal("symlink reached runner")
		return nil, nil
	})
	if err == nil {
		t.Fatal("symlink was accepted")
	}
}

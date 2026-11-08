package urnettools

import "testing"

func TestLinuxBindCapabilityGroups(t *testing.T) {
	cases := []struct {
		output string
		want   bool
	}{
		{"/bin/provider cap_chown,cap_net_bind_service=ep", true},
		{"/bin/provider cap_chown=ep cap_net_bind_service=eip", true},
		{"/bin/provider cap_net_bind_service,cap_chown+ep", true},
		{"/bin/provider cap_net_bind_service=p", false},
		{"/bin/provider cap_net_bind_service=e", false},
		{"/bin/provider cap_net_bind_service_extra=ep", false},
		{"/bin/provider cap_chown=ep", false},
	}
	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{ExecCmd: func(string, ...string) ([]byte, error) { return []byte(tc.output), nil }}
			got, _, err := sp.checkLinux("/bin/provider")
			if err != nil || got != tc.want {
				t.Fatalf("got=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}

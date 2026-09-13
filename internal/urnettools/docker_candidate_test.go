package urnettools

import "testing"

// TestIsProviderImageMetadata: discovery must recognise the provider image by
// its label or entrypoint, not only by container or image name.
func TestIsProviderImageMetadata(t *testing.T) {
	cases := []struct {
		name, label, entrypoint string
		want                    bool
	}{
		{"label", "1", "null", true},
		{"entrypoint before the label existed", "", `["/app/entrypoint.sh"]`, true},
		{"entrypoint with extra args", "", `["/app/entrypoint.sh","--flag"]`, true},
		{"unrelated image", "", `["/docker-entrypoint.sh"]`, false},
		{"similar path", "", `["/app/entrypoint.sh.bak"]`, false},
		{"no metadata", "", "null", false},
	}
	for _, c := range cases {
		if got := isProviderImageMetadata(c.label, c.entrypoint); got != c.want {
			t.Errorf("%s: isProviderImageMetadata(%q, %q) = %v, want %v", c.name, c.label, c.entrypoint, got, c.want)
		}
	}
}

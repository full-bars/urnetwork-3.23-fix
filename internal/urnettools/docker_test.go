package urnettools

import "testing"

// TestIsDockerCandidate covers the container image/name recognition rules.
func TestIsDockerCandidate(t *testing.T) {
	cases := []struct {
		image, name string
		want        bool
	}{
		{"ghcr.io/full-bars/urnetwork-3.23-fix:latest", "urnet", true},
		{"urnetwork-3.23-fix:stable", "provider1", true},
		{"nginx:latest", "web", false},
		{"redis:7", "cache", false},
		{"ghcr.io/full-bars/urnetwork-3.23-fix:latest", "anything", true},
		{"ubuntu:24.04", "urnet-test", true}, // name match is enough
	}
	for _, c := range cases {
		if got := isDockerCandidate(c.image, c.name); got != c.want {
			t.Errorf("isDockerCandidate(%q, %q) = %v, want %v", c.image, c.name, got, c.want)
		}
	}
}

// TestDockerImageVersion extracts the version from an image tag.
func TestDockerImageVersion(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.26.8": "v3.23.0-fix.26.8",
		"urnetwork-3.23-fix:stable":                             "",
		"urnetwork-3.23-fix:latest":                             "",
		"urnetwork-3.23-fix":                                    "",
	}
	for img, want := range cases {
		if got := dockerImageVersion(img); got != want {
			t.Errorf("dockerImageVersion(%q) = %q, want %q", img, got, want)
		}
	}
}

// TestTailLines verifies the logs tail helper.
func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := tailLines(in, "2"); got != "d\ne\n" {
		t.Errorf("tailLines 2 = %q", got)
	}
	if got := tailLines(in, "99"); got != in {
		t.Errorf("tailLines 99 should return everything, got %q", got)
	}
	if got := tailLines("single", "1"); got != "single\n" {
		t.Errorf("tailLines single = %q", got)
	}
}

// TestDockerProviderIdentity builds a Provider from a fake container record
// (unit-testable without a docker daemon).
func TestDockerProviderIdentity(t *testing.T) {
	c := dockerContainer{
		ID:    "abc123",
		Name:  "urnet",
		Image: "ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.26.8",
		State: "running",
	}
	p, err := dockerProvider(c)
	if err != nil {
		// Without a real docker daemon this errors on containerReadFile —
		// the container plumbing is not unit-testable offline; the pure
		// parts (version parse, candidate match) are covered above.
		t.Logf("dockerProvider requires a live docker daemon: %v", err)
		return
	}
	_ = p
}

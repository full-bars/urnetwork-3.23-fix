package urnettools

import "testing"

// The Docker image names the provider binary by architecture and flavour,
// for example urnetwork_amd64_stable. Discovery matched only "urnetwork" or
// "urnetwork-<suffix>", so inside a container it found nothing at all:
//
//	$ urnet-tools providers --all
//	no providers found on this box
//
// while the provider was running as pid 36 from /app/urnetwork_amd64_stable.
// That is why the image ships a bash reimplementation of the CLI instead of
// the real one.
func TestIsProviderArgAcceptsDockerImageBinaryNames(t *testing.T) {
	for _, name := range []string{
		"urnetwork_amd64_stable",
		"urnetwork_arm64_stable",
		"urnetwork_amd64_nightly",
		"/app/urnetwork_amd64_stable",
	} {
		if !isProviderArg(name) {
			t.Errorf("isProviderArg(%q) = false, want true; this is the binary the Docker image runs", name)
		}
	}
}

// The underscore separator must not widen matching to the non-provider
// siblings the hyphen form already excludes. A sentinel or update helper
// named with an underscore is still not a provider.
func TestIsProviderArgStillRejectsSiblingsWithUnderscores(t *testing.T) {
	for _, name := range []string{
		"urnetwork_sentinel_update",
		"urnetwork_update",
		"urnetwork_hub",
	} {
		if isProviderArg(name) {
			t.Errorf("isProviderArg(%q) = true, want false; it is a non-provider sibling", name)
		}
	}
}

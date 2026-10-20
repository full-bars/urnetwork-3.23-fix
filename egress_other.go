//go:build !windows && !darwin

package connect

// On other platforms self-exclusion is handled at the OS layer. Android uses
// VpnService.protect / addDisallowedApplication. The forced-egress binding is a
// no-op so the egress control is inert in those builds.
//
// Port note (v2026 engine -> 3.23 generation): the darwin split is the pinned
// engine's; egress_darwin.go carries the macOS binding.
func applyEgressInterface(_ uintptr, _ uint32, _ uint32) error {
	return nil
}

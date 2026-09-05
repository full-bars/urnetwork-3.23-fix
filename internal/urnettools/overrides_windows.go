//go:build windows

package urnettools

// On Windows, flock is not available. The atomic temp-file+rename pattern
// in saveOverridesJSONLocked already provides consistency for writes.
// For reads, the in-process mutex (overridesJSONFile.mu) is sufficient
// since the provider is a single-process application.

// lockShared is a no-op on Windows — flock is unavailable.
func lockShared(fd uintptr) error { return nil }

// lockExclusive is a no-op on Windows — flock is unavailable.
func lockExclusive(fd uintptr) error { return nil }

// unlock is a no-op on Windows — flock is unavailable.
func unlock(fd uintptr) error { return nil }

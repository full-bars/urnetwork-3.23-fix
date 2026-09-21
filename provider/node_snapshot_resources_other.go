//go:build !linux

package main

// readProcResources is a no-op off Linux: RSS and descriptor counts are
// omitted from the snapshot rather than guessed.
func readProcResources(r *SnapshotResources) {}

//go:build !linux

package main

func initSHMLogger() {
	// No-op for non-Linux platforms
}

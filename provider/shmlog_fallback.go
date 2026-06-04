//go:build !linux

package main

const (
	shmLogPath    = "/dev/shm/urnetwork.log"
	shmLogMaxSize = 1024 * 1024 // 1MB
)

func initSHMLogger() {
	// No-op for non-Linux platforms
}

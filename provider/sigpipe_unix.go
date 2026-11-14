//go:build linux || freebsd || openbsd || darwin

package main

import (
	"os/signal"
	"syscall"
)

func protectLogPipes() {
	signal.Ignore(syscall.SIGPIPE)
}

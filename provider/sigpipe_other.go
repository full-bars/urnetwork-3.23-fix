//go:build !linux && !freebsd && !openbsd && !darwin

package main

func protectLogPipes() {}

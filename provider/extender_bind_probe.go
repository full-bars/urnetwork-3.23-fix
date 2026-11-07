package main

import (
	"fmt"
	"io"
	"net"
)

// handleExtenderBindProbe tests the provider process's ability to bind TCP 443.
func handleExtenderBindProbe(args []string, listen func(string, string) (net.Listener, error), out, errOut io.Writer) (handled, success bool) {
	if len(args) != 1 || args[0] != "--extender-bind-probe" {
		return false, false
	}
	ln, err := listen("tcp", ":443")
	if err != nil {
		fmt.Fprintln(errOut, "extender-bind-probe: failed:", err)
		return true, false
	}
	if err := ln.Close(); err != nil {
		fmt.Fprintln(errOut, "extender-bind-probe: failed:", err)
		return true, false
	}
	fmt.Fprintln(out, "extender-bind-probe: proven")
	return true, true
}

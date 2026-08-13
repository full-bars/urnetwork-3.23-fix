// Command urnet-docker is the docker-container variant of urnet-tools.
//
// It manages URnetwork providers deployed as docker containers: discovery
// reads each container's in-container JWT to identify the account, targeting
// works by container name / network name, and provider-internal operations
// are delegated to the container's own urnet-tools via docker exec. Keeping
// this separate from urnet-tools makes it unambiguous which deployment kind
// each tool controls (process/systemd vs container).
package main

import (
	"fmt"
	"os"

	"github.com/urnetwork/connect/internal/urnettools"
)

// Version is stamped at release build time (-X main.Version=...). Keep the
// var: release.yml's ldflags require it.
var Version = "dev"

func main() {
	if err := urnettools.RunDocker(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "urnet-docker: %v\n", err)
		os.Exit(1)
	}
}

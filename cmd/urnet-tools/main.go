// Command urnet-tools is a provider-aware manager for URnetwork providers.
//
// Unlike the legacy shell-based urnet-tools (which resolves its target from
// the caller's $HOME and has no awareness of other providers on the box),
// this implementation discovers every running provider across all users,
// identifies each by its JWT network name, and requires an explicit target
// whenever the box runs more than one provider. See
// docs/URN-TOOLS-GO-DESIGN.md for the full design.
package main

import (
	"fmt"
	"os"

	"github.com/urnetwork/connect/internal/urnettools"
)

// Version is stamped at release build time (-X main.Version=...). Keep the
// var: release.yml's ldflags require it, and the self-update path uses it
// to report the running tool's version.
var Version = "dev"

func main() {
	if err := urnettools.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "urnet-tools: %v\n", err)
		os.Exit(1)
	}
}

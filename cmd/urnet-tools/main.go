// Command urnet-tools is a provider-aware manager for URnetwork providers.
//
// Unlike the legacy shell-based urnet-tools (which resolves its target from
// the caller's $HOME and has no awareness of other providers on the box),
// this implementation discovers every running provider across all users,
// identifies each by its JWT network name, and requires an explicit target
// whenever the box runs more than one provider. See the urnet-tools Go
// design document for the full design.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/urnetwork/connect/internal/urnettools"
)

// Version is stamped at release build time (-X main.Version=...). Keep the
// var: release.yml's ldflags require it, and the self-update path uses it
// to report the running tool's version.
var Version = "dev"

func main() {
	urnettools.ToolVersion = Version
	if err := urnettools.Run(argsForInvocation(os.Args[0], os.Args[1:])); err != nil {
		fmt.Fprintf(os.Stderr, "urnet-tools: %v\n", err)
		os.Exit(1)
	}
}

// argsForInvocation makes the binary behave as `urnet-tools top` when it is
// started under the name urtop (a link the installer creates beside
// urnet-tools). It reads the name it was invoked as, not the resolved
// executable path: os.Executable follows the link back to urnet-tools.
func argsForInvocation(argv0 string, args []string) []string {
	// Split on both separators by hand: filepath.Base only knows the host's,
	// and a Windows path must be handled the same when tested elsewhere.
	name := argv0
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	if name == "urtop" {
		return append([]string{"top"}, args...)
	}
	return append([]string{}, args...)
}

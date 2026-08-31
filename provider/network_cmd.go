package main

// network_cmd.go — the `provider choose_network` command handler.
// Saving/resetting logic lives in network.go; this file owns the CLI
// glue (argument extraction, user-facing output). Ported from
// urfoundation/sn PR #1 (miner/network_cmd.go).

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
)

// networkPresets maps shorthand names to (api_url, connect_url) pairs so
// users don't have to memorize backend URLs. "beta" matches the beta
// testnet endpoints used fleet-wide (see provider/README.md, FORK_CHANGES.md).
var networkPresets = map[string][2]string{
	"main": {"https://api.bringyour.com", "wss://connect.bringyour.com"},
	"beta": {"https://api.beta-test.net", "wss://connect.beta-test.net"},
}

// chooseNetworkCmd implements `provider choose_network <api_url>
// <connect_url>` and `provider choose_network --reset`.
func chooseNetworkCmd(opts docopt.Opts) {
	if reset, _ := opts.Bool("--reset"); reset {
		if err := resetNetworkConfig(); err != nil {
			fmt.Printf("failed to reset network: %s\n", err)
			os.Exit(1)
		}
		fmt.Println("network reset to the main network")
		return
	}

	apiUrl, err := opts.String("<api_url>")
	if err != nil {
		fmt.Printf("missing <api_url>: %s\n", err)
		os.Exit(1)
	}

	// Preset path: `provider choose_network main|beta`. Leaves the
	// explicit-URL path untouched below.
	if preset, ok := networkPresets[apiUrl]; ok {
		// If the user also supplied a connect_url, reject — presets are
		// self-contained and silently discarding the second arg is confusing.
		if _, urlErr := opts.String("<connect_url>"); urlErr == nil {
			fmt.Printf("preset %q includes its own connect_url — pass only the preset name, or use two explicit URLs\n", apiUrl)
			os.Exit(1)
		}
		apiUrl, connectUrl := preset[0], preset[1]
		if err := writeNetworkConfig(apiUrl, connectUrl); err != nil {
			fmt.Printf("network not saved: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("network saved (preset): api_url=%s connect_url=%s\n", apiUrl, connectUrl)
		return
	}
	if !strings.HasPrefix(apiUrl, "http://") && !strings.HasPrefix(apiUrl, "https://") {
		fmt.Printf("unknown network preset %q; known presets: main, beta — or pass an explicit URL pair: provider choose_network <api_url> <connect_url>\n", apiUrl)
		os.Exit(1)
	}

	connectUrl, err := opts.String("<connect_url>")
	if err != nil {
		fmt.Printf("missing <connect_url>: %s\n", err)
		fmt.Println("tip: use a preset name (main|beta) instead of a URL pair, e.g. `provider choose_network main`")
		os.Exit(1)
	}

	if err := writeNetworkConfig(apiUrl, connectUrl); err != nil {
		fmt.Printf("network not saved: %s\n", err)
		os.Exit(1)
	}
	fmt.Printf("network saved: api_url=%s connect_url=%s\n", apiUrl, connectUrl)
}

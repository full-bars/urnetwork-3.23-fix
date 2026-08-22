package main

// network_cmd.go — the `provider choose_network` command handler.
// Saving/resetting logic lives in network.go; this file owns the CLI
// glue (argument extraction, user-facing output). Ported from
// urfoundation/sn PR #1 (miner/network_cmd.go).

import (
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

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
	connectUrl, err := opts.String("<connect_url>")
	if err != nil {
		fmt.Printf("missing <connect_url>: %s\n", err)
		os.Exit(1)
	}

	if err := writeNetworkConfig(apiUrl, connectUrl); err != nil {
		fmt.Printf("network not saved: %s\n", err)
		os.Exit(1)
	}
	fmt.Printf("network saved: api_url=%s connect_url=%s\n", apiUrl, connectUrl)
}

package main

// snclaim_cmd.go — `provider snclaim`, the on-chain settlement-vault tool.
//
// sn ships this as a separate `snclaim` binary over its miner/onchain library:
// submit a claim transaction (for a key kept on an air-gapped host), read a
// claim's status, retry a deferred v1 claim credit with `withdraw`, and the v2
// status, claim and check commands. The provider already links that library
// for its claim daemons, so the same code is reachable here as a subcommand
// instead of a second binary (no change to the release build matrix). The
// provider's normal path for claiming is `provider claim` and the claim
// daemons; this is the low-level tool beside them.

// isSnclaimInvocation reports whether args (os.Args) is `provider snclaim ...`.
func isSnclaimInvocation(args []string) bool {
	return len(args) >= 2 && args[1] == "snclaim"
}

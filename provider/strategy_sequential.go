package main

import (
	"os"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// strategySequentialDefaultHedge is the hedge used when the operator enables
// the sequential dial without naming a duration.
const strategySequentialDefaultHedge = 300 * time.Millisecond

// applyProviderStrategySequential gives each identity's dial strategy a
// sequential first-choice window: the first-choice dialer is tried alone and
// the rest of the unordered block only starts on its failure or after the
// hedge expires. Measured on a heavy node, about 96 percent of full TLS
// handshakes came from the resilient modes and the normal proxy path racing
// concurrently per attempt (progress #3524); this trades a bounded escalation
// delay for far fewer concurrent full handshakes against a failing backend.
// Default OFF while the fleet data is collected.
// URNETWORK_STRATEGY_SEQUENTIAL: off/unset/0 disables; on/true/1 uses the
// default hedge; any duration ("400ms") sets it.
func applyProviderStrategySequential(settings *connect.ClientStrategySettings) {
	value := strings.TrimSpace(os.Getenv("URNETWORK_STRATEGY_SEQUENTIAL"))
	switch {
	case value == "", strings.EqualFold(value, "off"), value == "0":
		settings.SequentialDialHedge = 0
	case strings.EqualFold(value, "on"), strings.EqualFold(value, "true"), value == "1":
		settings.SequentialDialHedge = strategySequentialDefaultHedge
	default:
		if d, err := time.ParseDuration(value); err == nil && 0 < d {
			settings.SequentialDialHedge = d
		} else {
			settings.SequentialDialHedge = 0
		}
	}
}

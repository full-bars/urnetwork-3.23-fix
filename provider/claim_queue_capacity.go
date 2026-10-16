// Queue byte admission bounds retained-state reads and atomic publication
// without pruning signed history or interpreting an oversized file as empty.
package main

import "errors"

// A per-member queue retains 1,024 representative 8 KiB epoch records with
// roughly equal extra headroom. This is independent of the swarm member count.
const maximumClaimQueueBytes = 16 * 1024 * 1024

// Capacity refusal preserves original bytes and never admits a replacement
// nonce or an automatically compacted history.
var errClaimQueueCapacity = errors.New("claim queue exceeds the 16 MiB retained byte limit")

// The test boundary follows the real descriptor stat. It can grow the real
// fixture file, but cannot substitute decoded data or the read verdict.
type claimQueueReadHooks struct {
	afterStat func() error
}

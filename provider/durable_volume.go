// Persistent daemon and one-shot fleet owners consume an immutable operational
// declaration. Each actual store chooses its explicit daemon or owner-local API.
package main

import (
	"context"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect/durablevolume"
)

// Parsing never enrolls storage, infers a mount, or chooses a synthetic Host.
func minerStorageContext(ctx context.Context, opts docopt.Opts) context.Context {
	path, hash := fleetOpt(opts, "--durable-volumes"), fleetOpt(opts, "--durable-volumes-sha256")
	if path != "" || hash != "" {
		return durablevolume.WithReference(ctx, durablevolume.Reference{Path: path, Sha256: hash})
	}
	return ctx
}

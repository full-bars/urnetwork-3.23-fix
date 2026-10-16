// The claim daemons (claim-daemon, claim-daemon-v2). Ported from the sn
// miner's claim_daemon.go and claim_v2_daemon.go; the family branch fills this
// file, sharing the durable plumbing commit. Grammar and dispatch are final.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

func claimDaemonCmd(opts docopt.Opts) {
	fmt.Println("claim-daemon: not implemented yet")
	os.Exit(1)
}

// claimDaemonV2Cmd implements `provider claim-daemon-v2 --config=<path>
// [--v1-config=<path>] [--threshold-rao=<rao>] [--claim-now]`. The durable
// volume declaration rides the context, as the sn miner's dispatch does.
func claimDaemonV2Cmd(opts docopt.Opts) {
	if err := runClaimDaemonV2Command(minerStorageContext(context.Background(), opts), opts); err != nil {
		fmt.Printf("claim-daemon-v2: %s\n", err)
		os.Exit(1)
	}
}

package connect

// Pins the urfoundation/sn subnet packages this fork will build on for
// wallet/claim/bind-head support. Blank imports only — no behavior change;
// nothing calls these yet. Remove this file once real callers import them
// directly.
import (
	_ "github.com/urfoundation/sn/merkle"
	_ "github.com/urfoundation/sn/miner/onchain"
	_ "github.com/urfoundation/sn/ss58"
	_ "github.com/urfoundation/sn/stabi"
)

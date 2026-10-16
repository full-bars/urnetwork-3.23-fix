// The provider-lane glue the ported claim-daemon files need that sn's miner
// package kept elsewhere: sn's miner/fleet.go provided fleetOpt, sn's
// miner/sn.go provided stSettlementVault, and sdk.Api provided the jwt refresh
// and the per-call pool-claim read. The v33 lane has no sdk; the sdk names are
// kept so the ported call sites stay close to action 4ca8316e.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/connect"
)

// fleetOpt mirrors sn miner/fleet.go: the shared option reader of the miner
// command groups. The fleet family may hoist it when its files land.
func fleetOpt(opts docopt.Opts, name string) string {
	v, _ := opts.String(name)
	return strings.TrimSpace(v)
}

// stSettlementVault mirrors sn miner/sn.go: the v1 settlement vault binding
// used by the claim daemon's finalized reads.
var stSettlementVault = stabi.NewSTSettlementVault()

// refreshJwtResultError is the refusal object of POST /auth/refresh.
type refreshJwtResultError struct {
	Message string `json:"message"`
}

// refreshJwtResult is the reply of POST /auth/refresh.
type refreshJwtResult struct {
	ByJwt string                 `json:"by_jwt,omitempty"`
	Error *refreshJwtResultError `json:"error,omitempty"`
}

// daemonApi is the claim daemon's API handle: the connect BringYourApi plus
// the two calls sdk.Api supplied, GET /auth/refresh and the per-call-context
// pool claim read. sdk built both on the same connect transport
// (connect.HttpGetWithStrategyRaw), so the wire behavior is unchanged.
type daemonApi struct {
	*connect.BringYourApi
	clientStrategy *connect.ClientStrategy
	apiURL         string
}

func newDaemonApi(ctx context.Context, clientStrategy *connect.ClientStrategy, apiURL string) *daemonApi {
	return &daemonApi{BringYourApi: connect.NewBringYourApi(ctx, clientStrategy, apiURL), clientStrategy: clientStrategy, apiURL: apiURL}
}

// RefreshJwtSyncWithContext is sdk.Api.RefreshJwtSyncWithContext on the
// connect lane: GET <api_url>/auth/refresh with the current bearer, bound to
// the caller's context, decoded strictly as exactly one complete success or
// refusal.
func (self *daemonApi) RefreshJwtSyncWithContext(ctx context.Context) (*refreshJwtResult, error) {
	if ctx == nil {
		return nil, errors.New("refresh context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := connect.HttpGetWithStrategyRaw(ctx, self.clientStrategy, self.apiURL+"/auth/refresh", self.ByJwt())
	if err != nil {
		return nil, err
	}
	var result *refreshJwtResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result == nil || result.Error == nil && result.ByJwt == "" || result.Error != nil && (result.ByJwt != "" || result.Error.Message == "") {
		return nil, errors.New("refresh must contain exactly one complete success or refusal")
	}
	return result, nil
}

// SnPoolClaimSyncWithContext is sdk.Api.SnPoolClaimSyncWithContext on the
// connect lane: GET <api_url>/sn/pool/claim?epoch=N with the caller's
// context. The url is built exactly as connect's SnPoolClaimSync builds it.
func (self *daemonApi) SnPoolClaimSyncWithContext(ctx context.Context, args *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
	if args == nil {
		return nil, errors.New("pool claim args are required")
	}
	requestUrl := fmt.Sprintf("%s/sn/pool/claim?epoch=%d", self.apiURL, args.Epoch)
	if args.LegacyColdkey != "" {
		requestUrl += "&legacy_coldkey=" + neturl.QueryEscape(args.LegacyColdkey)
	}
	raw, err := connect.HttpGetWithStrategyRaw(ctx, self.clientStrategy, requestUrl, self.ByJwt())
	if err != nil {
		return nil, err
	}
	var result connect.SnPoolClaimResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

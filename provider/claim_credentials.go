// The queue owns its API until shutdown. Credentials control new proof reads;
// reconciliation of retained signed transactions never depends on this owner.
package main

import (
	"bytes"
	"context"
	"errors"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/clientauth"
)

// Called serially by the queue. Refresh and claim reads share the original finite context. Full identity
// validation precedes publication; disk remains provider-owned.
type claimCredentialApi struct {
	api           *daemonApi
	selection     snCredentialSelection
	legacyColdkey string
	diskToken     string
	original      string
	onReady       func()
	ready         bool
}

// An unchanged disk token cannot overwrite a newer in-memory SDK refresh.
// A changed disk token must still name the original provider and principal.
func (self *claimCredentialApi) reload() error {
	selection := self.selection
	selection.original = self.original
	token, _, err := readSnCredentials(selection)
	if err != nil {
		return err
	}
	if self.original == "" && !selection.legacyNetwork {
		if err := clientauth.ValidateRefreshedClientJwt(token, token); err != nil {
			return err
		}
		self.original = token
	}
	if self.diskToken != token {
		self.api.SetByJwt(token)
		self.diskToken = token
	}
	if self.api.ByJwt() == "" {
		return errors.New("provider claim credential is not ready after authentication rejection")
	}
	if !self.ready {
		self.ready = true
		if self.onReady != nil {
			self.onReady()
		}
	}
	return nil
}

// The connect handle remains the sole GET, retry transport and refresh owner.
// Each queue read can notice an identity-preserving provider file renewal.
func (self *claimCredentialApi) SnPoolClaimSyncWithContext(ctx context.Context, args *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
	if err := self.reload(); err != nil {
		return nil, err
	}
	if !self.selection.legacyNetwork {
		refreshed, err := self.api.RefreshJwtSyncWithContext(ctx)
		if err != nil {
			return nil, err
		}
		if refreshed == nil || refreshed.Error != nil || refreshed.ByJwt == "" {
			return nil, errors.New("provider claim credential refresh is not ready")
		}
		if err := clientauth.ValidateRefreshedClientJwt(self.original, refreshed.ByJwt); err != nil {
			return nil, err
		}
		self.api.SetByJwt(refreshed.ByJwt)
	}
	selected := *args
	selected.LegacyColdkey = self.legacyColdkey
	result, err := self.api.SnPoolClaimSyncWithContext(ctx, &selected)
	if err == nil && result != nil && result.Error == nil && len(result.NoId) != 0 && self.legacyColdkey != "" {
		coldkey, decodeErr := ss58.DecodeWithPrefix(self.legacyColdkey, ss58.BittensorPrefix)
		if decodeErr != nil || !bytes.Equal(coldkey[:], result.Coldkey) {
			return nil, errors.New("legacy claim response changed the selected original coldkey")
		}
	}
	return result, err
}

// Tests observe only a completed durable checkpoint, never replace a save or
// supply a claim/reconciliation verdict. The queue invokes this synchronously.
type claimDaemonCheckpointHooks struct{ afterCheckpoint func(*ClaimQueue) }
type claimDaemonCheckpointHooksKey struct{}

package main

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// renewalMutex serializes /network/auth-client renewal calls process-wide so
// boxes with many proxies (50-60+) don't stampede the API with simultaneous
// requests and get them all rejected/rate-limited.
var renewalMutex sync.Mutex

// proxyJWTRenewBefore is how long before a client JWT's exp claim the watcher
// renews it. Tokens live 24h on the beta backend (and now mainnet's new
// format), so this fires ~12h into a token's life, leaving 12h of runway for
// retries. On backends still issuing long-lived tokens (~51 days), the
// threshold simply never fires until the token is 12h from its own expiry —
// the watcher is a no-op there, preserving the old behavior.
const proxyJWTRenewBefore = 12 * time.Hour

// proxyJWTWatcherConfig carries everything the per-proxy renewal watcher
// needs. Every field except Tick is populated by the provideWithProxy wiring.
type proxyJWTWatcherConfig struct {
	IdentityKey    string
	ClientID       connect.Id
	Description    string
	ApiURL         string
	ClientStrategy *connect.ClientStrategy
	OOB            *connect.ApiOutOfBandControl
	Transport      *connect.PlatformTransport
	RenewNow       <-chan struct{}
	// Tick overrides the internal 1h ticker in tests; nil means 1h.
	Tick       <-chan time.Time
	ProxyIndex int
}

// runProxyJWTWatcher renews the proxy's client JWT before it expires and
// immediately when the OOB path observes a 401. It blocks until ctx is done.
//
// Renewal conditions (OR):
//  1. The cached client JWT's exp is within proxyJWTRenewBefore (12h).
//  2. The OOB control has observed at least one 401 since the last reset —
//     the backend rejected the current bearer, so waiting for the next
//     scheduled tick would keep the proxy a black hole.
//
// On success the fresh JWT (SAME client_id — the server UPDATEs the existing
// network_client row) is hot-swapped into the OOB control and the platform
// transport, then persisted to the client-JWT store so a restart reuses it.
func runProxyJWTWatcher(ctx context.Context, cfg proxyJWTWatcherConfig) {
	tick := cfg.Tick
	if tick == nil {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		tick = ticker.C
	}

	renew := func() bool {
		accountJWT, err := readAccountJWT()
		if err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s: cannot read account JWT: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		renewalMutex.Lock()
		defer renewalMutex.Unlock()

		newJwt, err := renewClientJWT(ctx, cfg.ApiURL, accountJWT, cfg.ClientID, cfg.Description)
		if err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s renewal failed: %v (will retry)\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		cfg.OOB.SetByJwt(newJwt)
		if cfg.Transport != nil {
			cfg.Transport.SetAuth(&connect.ClientAuth{
				ByJwt:      newJwt,
				InstanceId: connect.NewId(),
				AppVersion: RequireVersion(),
			})
		}
		if err := globalClientJWTStore.Put(cfg.IdentityKey, clientJWTEntry{
			ByClientJWT: newJwt,
			ClientID:    cfg.ClientID.String(),
			NetworkID:   accountNetworkId(accountJWT),
			MintedAt:    time.Now(),
		}); err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s renewal OK but store write failed: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
		}
		cfg.OOB.ResetAudit401Count()
		if exp := parseJWTExpiryTime(newJwt); exp != nil {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s, exp %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID, formatDuration(time.Until(*exp)))
		} else {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID)
		}
		return true
	}

	renewIfNeeded := func(reason string) {
		need := cfg.OOB != nil && cfg.OOB.Audit401Count() > 0
		if !need {
			if entry, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
				if exp := parseJWTExpiryTime(entry.ByClientJWT); exp != nil && time.Until(*exp) < proxyJWTRenewBefore {
					need = true
				}
			}
		}
		if need {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewal trigger: %s\n", cfg.ProxyIndex, cfg.IdentityKey, reason)
			renew()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			renewIfNeeded("hourly check")
		case <-cfg.RenewNow:
			renewIfNeeded("renew-now signal")
		}
	}
}

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
//
// The mutex deliberately covers the whole renewal (network call + store write
// + OOB/transport hot-swap): releasing it around just the store write would
// let the HTTP calls overlap, which is the stampede the mutex exists to
// prevent. Renewals are naturally staggered (each proxy mints at a different
// time, and the 12h pre-expiry window absorbs scheduling jitter), so actual
// contention is rare. Worst case for a 60-proxy box renewing back-to-back at
// ~300ms per auth-client call is ~18s of serialized renewals — acceptable,
// and it happens only on the pathological "every token expires at once" case.
var renewalMutex sync.Mutex

// proxyJWTRenewBefore is how long before a client JWT's exp claim the watcher
// renews it. Tokens live 24h on the beta backend (and now mainnet's new
// format), so this fires ~12h into a token's life, leaving 12h of runway for
// retries. On backends still issuing long-lived tokens (~51 days), the
// threshold simply never fires until the token is 12h from its own expiry —
// the watcher is a no-op there, preserving the old behavior.
const proxyJWTRenewBefore = 12 * time.Hour

// proxyJWTRenewTimeout bounds a single /network/auth-client renewal call so a
// hung transport cannot block the watcher (and the process-wide mutex)
// indefinitely. Mirrors the account-JWT refresher's verification timeout.
const proxyJWTRenewTimeout = 30 * time.Second

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
	// RenewNow is signaled by the OOB's 401 callback (and available for
	// tests to drive the fast-path directly).
	RenewNow chan struct{}
	// Tick overrides the internal 1h ticker in tests; nil means 1h.
	Tick       <-chan time.Time
	ProxyIndex int
	// InstanceId is the proxy's transport instance id from startup; it is
	// reused on every renewal so the server sees a stable session identity
	// instead of a fresh one per token rotation.
	InstanceId connect.Id
}

// runProxyJWTWatcher renews the proxy's client JWT before it expires and
// immediately when the backend rejects the current token. It blocks until ctx
// is done.
//
// Renewal conditions (OR):
//  1. Immediately at startup if the cached client JWT is already within
//     proxyJWTRenewBefore of expiry (covers hot-restart with an expired or
//     near-expired token — no waiting for the first hourly tick).
//  2. The cached client JWT's exp is within proxyJWTRenewBefore (12h).
//  3. The OOB control observed a 401 since the last reset (renew-now signal
//     from SetOn401, or the counter polled on tick).
//  4. The platform transport accumulated auth failures (ProxyAuthFailureCount
//     >= threshold) — the transport auth path is separate from the OOB, and
//     a 401 there would otherwise go undetected until the hourly tick.
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

	// Wire the immediate 401 trigger: when the OOB control sees a 401 it
	// pings renewNow (non-blocking), so renewal fires at once instead of
	// waiting up to an hour for the next tick.
	if cfg.OOB != nil {
		cfg.OOB.SetOn401(func() {
			select {
			case cfg.RenewNow <- struct{}{}:
			default:
			}
		})
	}

	renew := func() bool {
		accountJWT, err := readAccountJWT()
		if err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s: cannot read account JWT: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		renewalMutex.Lock()
		defer renewalMutex.Unlock()

		renewCtx, cancel := context.WithTimeout(ctx, proxyJWTRenewTimeout)
		defer cancel()

		newJwt, err := renewClientJWT(renewCtx, cfg.ApiURL, accountJWT, cfg.ClientID, cfg.Description)
		if err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s renewal failed: %v (will retry)\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		cfg.OOB.SetByJwt(newJwt)
		if cfg.Transport != nil {
			cfg.Transport.SetAuth(&connect.ClientAuth{
				ByJwt:      newJwt,
				InstanceId: cfg.InstanceId,
				AppVersion: RequireVersion(),
			})
		}
		// Keep the previous NetworkID when the account JWT parse fails: a
		// mismatch would make the store treat the entry as mint-fresh on
		// restart, silently losing the identity we just renewed.
		networkID := accountNetworkId(accountJWT)
		if networkID == "" {
			if prev, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
				networkID = prev.NetworkID
			}
		}
		if err := globalClientJWTStore.Put(cfg.IdentityKey, clientJWTEntry{
			ByClientJWT: newJwt,
			ClientID:    cfg.ClientID.String(),
			NetworkID:   networkID,
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
		if !need && cfg.ProxyIndex >= 0 && connect.ProxyAuthFailureCount(cfg.ProxyIndex) >= revokedIdentityAuthFailureThreshold {
			// The transport auth path is separate from the OOB; repeated
			// transport auth failures mean the bearer is being rejected at
			// the data-plane level even if no OOB call has fired.
			need = true
		}
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

	// Startup check: a hot-restart that reused an already-expired client JWT
	// must not sit as a black hole until the first hourly tick.
	renewIfNeeded("startup check")

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			renewIfNeeded("hourly check")
		case <-cfg.RenewNow:
			renewIfNeeded("401 fast-path")
		}
	}
}

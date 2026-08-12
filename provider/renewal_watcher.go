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
	// RevocationDone, when set, is closed by a successful renewal so the
	// pre-existing revocation watcher (watchReusedIdentityForRevocation)
	// stops racing the renewal watcher on the same store entry: without it,
	// a renewed-but-not-yet-reconnected proxy could be evicted as
	// "never authenticated" and the entry deleted under the fresh token.
	RevocationDone chan struct{}
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

		// H-2: route through the shared adaptive rate limiter (same AIMD
		// throttle as the mint path). Without this, a 401 storm drives one
		// renewal per backend-401 — the fast-path fires as fast as the
		// backend rejects, and the mutex serializes but does not rate-limit.
		if err := globalAuthRateLimiter.Wait(renewCtx); err != nil {
			tlog("⚠️ [jwt-renew] proxy[%d] %s renewal skipped: rate limiter: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		newJwt, err := renewClientJWT(renewCtx, cfg.ApiURL, accountJWT, cfg.ClientID, cfg.Description, cfg.ClientStrategy)
		// Feed the outcome back so the limiter's AIMD adjusts (429s halve
		// the rate, sustained success creeps it back up).
		globalAuthRateLimiter.ReportResult(err)
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
			tlog("⚠️ [jwt-renew] proxy[%d] %s renewal OK in memory but store write failed: %v — keeping old token armed for retry\n",
				cfg.ProxyIndex, cfg.IdentityKey, err)
			// Do NOT ResetAudit401Count and do NOT stand down the revocation
			// watcher: the in-memory swap is live but the persistence failed, so
			// a restart would load the old (expiring) token. Keep the 401 counter
			// armed and currentJwt on the old token so the next trigger renews
			// again; the disk failure is logged loudly above.
			return false
		}
		cfg.OOB.ResetAudit401Count()
		// The identity is demonstrably alive (server just re-signed it), so
		// the revocation watcher must stand down — otherwise it can evict
		// this entry while the transport is still reconnecting.
		if cfg.RevocationDone != nil {
			select {
			case <-cfg.RevocationDone:
			default:
				close(cfg.RevocationDone)
			}
		}
		if exp := parseJWTExpiryTime(newJwt); exp != nil {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s, exp %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID, formatDuration(time.Until(*exp)))
		} else {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID)
		}
		return true
	}

	// H2: track the live token in the watcher, not just the store. A store
	// entry can vanish (mint-time Put failure, revocation-watcher eviction,
	// disk error), which would silently disable the exp-driven check. The
	// store remains the persistence sink; the live copy is the source of
	// truth for the expiry decision.
	currentJwt := ""
	if entry, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
		currentJwt = entry.ByClientJWT
	}

	// H-4: snapshot the transport auth-failure count at startup and refresh
	// it after every successful renewal. ProxyAuthFailureCount is a CUMULATIVE
	// lifetime counter — comparing it raw against the threshold would renew
	// every hour forever once a flaky spell crossed 5 at any point. Comparing
	// against a baseline means only NEW failures since the last renewal
	// trigger the transport-auth fast path.
	authFailureBaseline := int64(0)
	if cfg.ProxyIndex >= 0 {
		authFailureBaseline = connect.ProxyAuthFailureCount(cfg.ProxyIndex)
	}

	renewIfNeeded := func(reason string) {
		need := cfg.OOB != nil && cfg.OOB.Audit401Count() > 0
		if !need && cfg.ProxyIndex >= 0 && connect.ProxyAuthFailureCount(cfg.ProxyIndex)-authFailureBaseline >= revokedIdentityAuthFailureThreshold {
			// The transport auth path is separate from the OOB; repeated
			// transport auth failures mean the bearer is being rejected at
			// the data-plane level even if no OOB call has fired.
			need = true
		}
		if !need && currentJwt != "" {
			if exp := parseJWTExpiryTime(currentJwt); exp != nil && time.Until(*exp) < proxyJWTRenewBefore {
				need = true
			}
		}
		if need {
			tlog("🔁 [jwt-renew] proxy[%d] %s renewal trigger: %s\n", cfg.ProxyIndex, cfg.IdentityKey, reason)
			if renew() {
				if entry, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
					currentJwt = entry.ByClientJWT
				}
				if cfg.ProxyIndex >= 0 {
					authFailureBaseline = connect.ProxyAuthFailureCount(cfg.ProxyIndex)
				}
			}
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

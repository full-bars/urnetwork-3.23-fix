# H3 + Miner Migration Plan (Bittensor/SN future)

Status: SEEDED — facts verified 2026-09-15, branch carries the plan + workspaces scaffold.

## Why

Upstream rewired its data path around H3/QUIC and moved the provider daemon to `urfoundation/sn`. The fork stays on the v3.23 library + its own ops layer. This branch tracks the migration to v2026 = H3 transport + IPv6 dual-stack + the sn miner base, carrying every fork customization.

## Verified facts (2026-09-15)

| Fact | Evidence |
|---|---|
| Upstream connect is library-only (no provider/, no urnet-tools) | `git ls-tree upstream/main` — no provider/ or internal/ dirs |
| Provider daemon lives in urfoundation/sn `miner/` | upstream PROVIDERFIXES.md: `urprovider from sn/cli/miner` |
| sn/miner builds against v2026 connect via sibling replace | sn go.mod: `replace github.com/urnetwork/connect => ../connect` |
| sn/miner is Bittensor-flavored (claims/onchain/swarm) | miner/ file list (claim_daemon.go, onchain/, swarm.go) |
| Library drift: transfer.go 224KB→603KB, ip.go 113KB→336KB, ip_remote_multi_client.go 116KB→700KB | git ls-tree objectsize fork vs upstream/main |
| Upstream provider throughput fixes merged: receive-autotuning unlock (210→719 Mb/s), zombie-flow retirement (180→629+ Mb/s) | PROVIDERFIXES.md summary |
| Fork ops layer: provider/ + internal/urnettools + scripts, ~67 connect.* symbols from provider/main.go alone | git grep count |

## Plan phases

### Phase 0 — Workspace scaffold (this branch)
Local workspace of sibling checkouts so `go build ./...` in sn/miner works:
```
~/h3-workspace/
  connect/        # v2026 (upstream/main)
  sn/             # urfoundation/sn
  server/ warp/ proxy/ userwireguard/ operator-proxy/   # per sn go.mod replaces
```
`scripts/h3-workspace.sh` sets it up (clone + correct branches + `go build` smoke test of urprovider).

### Phase 1 — Baseline: urprovider builds and runs as a plain provider
- Build `urprovider` from sn/miner, run against a test account (not fleet).
- Verify: contract acquisition, transfer path (H3 + legacy fallback), metrics.
- Gate: p2p/relay data flows both directions; H3 path engages with a v2026 client; legacy clients still work.

### Phase 2 — Carry the fork's provider runtime (layer 1)
The daemon features that live in fork `provider/` today, rewritten onto the v2026 API surface:
1. Pool health heartbeat + leak detection (`provider/pool_health.go`) — v2026 has `message_pool_ownership_test.go`/violation handler; map counters onto the new pool.
2. Resource-pressure actuator (memory scaling) — v2026 memory_budget.go + sn provider_memory.go (host-derived targets) replace the fork's static GOMEMLIMIT.
3. Earnings store + priority, contract-sizing profile, R-5 retention cap — port the fork's transfer-side fixes that still apply (audited: 2 MUST PORTs already landed on 3.23-fix; re-verify against v2026 final).
4. Startup banner, proxy health grading, CFAA sync — mostly additive to the daemon.

### Phase 3 — Carry the ops tooling (layer 2)
- Control socket (`provider/control_socket.go`) — port onto the sn daemon's runtime loop.
- Hotswap (zero-downtime exec) — fork's hotswap.go; check sn/miner restart model first.
- Metrics listen + Prometheus names (`metrics_provider.go`, `metrics_listen.go`) — keep fork's metric names (dashboards depend on them).
- urnet-tools: discovery/logs/config/update flows — daemon-agnostic; update flow targets the new binary + state dir.

### Phase 4 — Fleet rollout (user drives, box by box)
- New series tag off the migrated base (e.g. `v3.23.0-h3.*` or v2026-fix.0.x — TBD with user).
- Per-box: swap binary, keep state, verify provider registration + metrics, rollback = old binary.
- Bittensor: subnet claiming can be enabled per node; fleet default = off until user decides.

### Phase 5 — Decommission 3.23
- Freeze the v3.23.0-fix line at the last good tag; security-only fixes. Fork stays as the migration reference.

## Risks (top 5)

1. v2026 connect API drift on ops-layer rewrite breadth (Phase 2 size unknown until the symbol map is complete (Phase 0.5: complete the 67-symbol map for provider/main.go + the rest of provider/)).
2. Hotswap semantics vs sn/miner restart model — may need a fork of sn.
3. Fleet 120-node rollout window + metric-name continuity for Grafana dashboards.
4. H3 benefits require v2026-capable peers — upgrade fleet in waves, not all-at-once.
5. sn/miner Bittensor coupling (claims/onchain) adds moving parts the fork doesn't need — keep behind flags.

## Sequencing: hub removal first (2026-09-15 addendum)

PR #634 (hub deprecation, −9,738 lines / 24 files in hub/ alone) merges BEFORE any migration work:
- hub/ is upstream-origin carried code — ZERO fork-specific commits touched it since the v3.23 divergence → nothing of the fork's is lost.
- Stripping first removes ~10K lines from the migration carry bucket, plus hub refs in CI/scripts/tests.
- Sequence: merge #634 → cut hub-less v32 on the 3.23 line → THEN start Phase 1 off that (hub-less) base. Recompute the 343-commit delta against the stripped tree before Phase 1 gates.
- Bonus: the fork already carries SN bridge types (SnEpochResult, SnPoolClaimArgs/Result) in internal/urnettools — the Bittensor seam is partially present in today's code.

## Phase 0.5 sizing facts (2026-09-15)

| Surface | connect.* symbols | Fork-built | Upstream-present |
|---|---|---|---|
| provider/ (whole dir) | 99 | 39 | 60 |
| provider/main.go alone | 67 | 26 | 41 |
| control_socket.go | 4 | 3 (metrics/persistent-err) | 1 (ParseByteCount, identical) |
| internal/urnettools (whole tree) | 8 | 2 | 6 |
| update.go | 0 | — | — |

Key drift checks: NewClient/ParseByteCount byte-identical; NewClientWithDefaults dropped its settings param (call-site rename); v2026 has NO Prometheus surface (metrics layer is 100% fork-built, carries wholesale).

## Phase 0.5 verdict (2026-09-15)

Signature-drift pass over the ops layer's connect surface:
- Identical in v2026 (byte-level): NewClient, NewClientWithTag, ClientOob, SetMemoryBudget, ResizeMessagePools, ParseByteCount.
- Rename/call-site only: NewClientWithDefaults (settings param removed — callers pass settings explicitly), SendWithTimeout/SendMultiHopWithTimeout destination param TransferPath → Id (drop the path, pass the id).
- Fork-built, carries wholesale: metrics/Prometheus surface, pool health, proxy health/quality, PQE counters, earnings, control+hotswap (vetted above).
- **Estimate: Phase 2 sizing = call-site migration, not API rewrites. The ops layer's library seam is ~60 symbols, and sampled drift is shallow (params, not contracts). Expect days, not weeks, for the seam itself; the daemon-lifecycle differences (hotswap vs sn restart, urnet-tools update flow, metrics names) are the real work items, and they are additive/portable, not blocked on v2026 internals.**

## Next action
Phase 0 semantics + complete the symbol map (provider/main.go + controls + urnet-tools update paths) → estimate Phase 2 sizing. That number decides weeks-vs-months.
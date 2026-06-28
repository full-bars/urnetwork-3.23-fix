# Porting Plan: `ac91c55` — DPI Refactor

**Date**: 2026-06-28
**Upstream commit**: `ac91c55c75c79a2e54c8ba762ffd8a7618acb2e8`
**Author**: Brien Colwell
**Message**: `ip_security: update dpi to better separate web standards from dmca risks`
**Upstream tag**: HEAD of `origin/main`, past `v2026.6.25-976407430`
**Risk**: Medium — touches core packet relay path; no fork-local changes in `ip_security.go` so clean merge expected

---

## What this commit does

Splits the monolithic `ip_security.go` (~66K lines, mostly a `map[[4]byte]bool` blocklist) into a layered architecture:

| Layer | File | What it does |
|-------|------|-------------|
| 1 — CFAA | `ip_security_cfaa.go` + `ip_security_cfaa_block.go` | Static endpoint reputation: blocked IP ranges (binary-search packed table) + port/protocol policy. Three-way verdict: drop / allow / pass-to-DPI. |
| 2 — DMCA | `ip_security_dmca.go` | Stateful deep-packet inspection: BitTorrent signature detection (BEP 3/5/15/29), entropy-based encrypted-flow heuristic, 16-shard LRU flow table. |
| Exemption | `ip_security_webstandard.go` | Stateless TLS/DTLS/QUIC/STUN byte-signature matching. Rescues legitimate encrypted flows from the DMCA detector. |

The `SecurityPolicy.Inspect()` interface gains a `payload []byte` parameter so DPI can inspect L7 content.

---

## Pre-port audit: fork already has

These supporting types already exist in the fork (likely from an earlier partial port), so no need to port them:

- `ParseIpPathWithPayload([]byte) (*IpPath, []byte, error)` — `ip.go:3133`
- `func (self *IpPath) ToIp6Path() Ip6Path` — `ip.go:3287`
- `type Ip6Path struct` — `ip.go:3364`
- `type UserLimited interface` — `ip.go:3388`
- `func applyLruUserLimit[R UserLimited](...)` — `ip.go:3416`
- `func isPublicUnicast(ip net.IP) bool` — `ip_security.go:229`
- `func DisableSecurityPolicy()` — `ip_security.go:211`

---

## Porting steps (execute in order)

### Step 1 — Add TLS constants to `net_tls.go`

Add after `DefaultTlsConfig()` (line 41), before file end:

```go
type TlsContentType = byte

const (
	TlsContentTypeChangeCipherSpec TlsContentType = 0x14
	TlsContentTypeAlert            TlsContentType = 0x15
	TlsContentTypeHandshake        TlsContentType = 0x16
	TlsContentTypeApplicationData  TlsContentType = 0x17
	TlsContentTypeHeartbeat        TlsContentType = 0x18
)
```

Needed by `ip_security_webstandard.go` matchers.

### Step 2 — Create new file: `ip_security_cfaa.go`

Copy from upstream commit `ac91c55` — 208 lines.

Exports:
- `CfaaSecurityPolicySettings` struct with `Enabled bool`
- `DefaultCfaaSecurityPolicySettings()` factory

Contains:
- `cfaaDetector` with `inspect(ip, port, protocol, version) cfaaVerdict`
- Three-way verdict: `cfaaPass`/`cfaaAllow`/`cfaaDrop`
- Port policy with clear allow/drop/pass branching
- IP blocklist binary search: `cfaaBlockedIp4()`, `cfaaBlockedIp6()`, `cfaaSearch6()`, `cfaaBe64()`

### Step 3 — Create new file: `ip_security_cfaa_block.go`

Copy from upstream commit `ac91c55` — ~8175 lines.

Generated data file. Do not edit manually. Contains:
- `cfaaBlockedPrefixCount = 64131` (int)
- `cfaaBlockedPrefixData` (string, 513,048 bytes packed as 8-byte lo/hi records)
- `cfaaBlockedPrefix6Count` (int)
- `cfaaBlockedPrefix6Data` (string, packed as 32-byte lo/hi records)

### Step 4 — Create new file: `ip_security_webstandard.go`

Copy from upstream commit `ac91c55` — 149 lines.

Exports:
- `WebStandardSettings` struct with `Enabled`, `Tls`, `Dtls`, `Quic`, `Stun` bools
- `DefaultWebStandardSettings()` factory

Contains:
- `webStandardDetector` with `match(ipPath, payload) bool`
- `isTlsClientHello()`, `isDtlsClientHello()`, `isQuicLongHeader()`, `isStun()` matchers

### Step 5 — Create new file: `ip_security_dmca.go`

Copy from upstream commit `ac91c55` — 491 lines.

Exports:
- `DmcaSecurityPolicySettings` struct with all tuning knobs (see table below)
- `DefaultDmcaSecurityPolicySettings()` factory

Settings and defaults:

| Field | Default | Purpose |
|---|---|---|
| `Enabled` | `true` | Master switch |
| `LogOnly` | `false` | Evaluate but never enforce |
| `DropBittorrentSignature` | `true` | Drop on positive BitTorrent sig |
| `ReportBittorrentIncident` | `true` | Report abuse vs silent drop |
| `DropUnsanctionedEncrypted` | `true` | Drop obfuscated/encrypted non-web flows |
| `InspectionPacketBudget` | `8` | Max packets inspected per flow |
| `EncryptedDecisionPackets` | `3` | Consecutive encrypted packets to trigger |
| `MaxInspectionPayload` | `512` | Leading payload bytes inspected |
| `MinEncryptedPayload` | `32` | Min payload for entropy check |
| `EncryptedPopcountBand` | `0.10` | Max distance from 0.5 popcount ratio |
| `EncryptedMaxPrintableFraction` | `0.50` | Max printable-ASCII fraction |
| `EncryptedMinNormalizedEntropy` | `0.85` | Min normalized Shannon entropy |
| `MaxFlows` | `65536` | Max tracked flows (LRU eviction) |

Contains:
- 4-state verdict: `dmcaInspecting` / `dmcaAllow` / `dmcaDropEncrypted` / `dmcaBittorrent`
- 16-shard LRU flow table (`dmcaFlowShards`), each with `sync.RWMutex`
- Lock-free terminal-verdict fast path via `atomic.LoadInt32`
- BitTorrent signatures: BEP 3 peer wire, BEP 3 HTTP tracker, BEP 5 DHT KRPC, BEP 15 UDP tracker, BEP 29 uTP

### Step 6 — Refactor `ip_security.go`

#### 6a — Change `SecurityPolicy` interface

```go
// Before:
Inspect(provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error)

// After:
Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
```

Update all 3 implementations:
- `egressSecurityPolicy.Inspect`
- `ingressSecurityPolicy.Inspect`
- `disableSecurityPolicy.Inspect`

#### 6b — Rewrite `egressSecurityPolicy`

Struct gains `cfaa *cfaaDetector` and `dmca *dmcaDetector` fields.

```go
type egressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
	dmca  *dmcaDetector
}
```

Add exported constructor:
```go
func NewEgressSecurityPolicy(
	cfaaSettings *CfaaSecurityPolicySettings,
	dmcaSettings *DmcaSecurityPolicySettings,
	webSettings *WebStandardSettings,
	stats *SecurityPolicyStatsCollector,
) SecurityPolicy {
	return &egressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
		dmca:  newDmcaDetector(dmcaSettings, newWebStandardDetector(webSettings)),
	}
}
```

Update `DefaultEgressSecurityPolicy()` and `DefaultEgressSecurityPolicyWithStats()` to use `NewEgressSecurityPolicy` with default settings.

Rewrite `inspect(provideMode, ipPath, payload)`:

```
1. if ProvideMode_Network → Allow
2. if !isPublicUnicast(destIp) → Incident
3. switch cfaa.inspect(destIp, destPort, protocol, version):
   - cfaaDrop → return Drop
   - cfaaAllow → return Allow
   - cfaaPass → continue
4. switch dmca.classify(ipPath, payload):
   - dmcaBittorrent → return dmca.result(v)
   - dmcaDropEncrypted → return dmca.result(v)
   - default → return Allow
```

#### 6c — Rewrite `ingressSecurityPolicy`

Struct gains `cfaa *cfaaDetector` field.

```go
type ingressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
}
```

Add exported constructor:
```go
func NewIngressSecurityPolicy(
	cfaaSettings *CfaaSecurityPolicySettings,
	stats *SecurityPolicyStatsCollector,
) SecurityPolicy {
	return &ingressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
	}
}
```

Rewrite `inspect(provideMode, ipPath)`:
```
1. if ProvideMode_Network → Allow
2. if cfaaDrop == cfaa.inspect(sourceIp, sourcePort, protocol, version) → return Drop
3. return Allow
```

#### 6d — Remove `blockIp4s` map

Delete the entire `var blockIp4s = map[[4]byte]bool{...}` (lines ~394 through ~66266).

#### 6e — Update imports

The new `ip_security.go` core is ~251 lines and much simpler than the old one. Most imports that appear in the new detector files (`encoding/binary`, `math`, `sync/atomic`) live in those files, not in `ip_security.go` itself — they're already included when you copy the files in Steps 2–5.

What `ip_security.go` actually needs after the refactor:
- `"fmt"`, `"net"`, `"strconv"`, `"sync"` — keep these
- `"golang.org/x/exp/maps"` — keep (used by stats collector)
- Remove everything else — the old file used `encoding/binary` for the hash map; that's gone

Do not manually add `encoding/binary`, `math`, `slices`, or `sync/atomic` to `ip_security.go`; those belong in the new files and will cause "imported and not used" compile errors if added here.

### Step 7 — Update `ip.go` callers

#### 7a — `RemoteUserNatProvider.ClientReceive` (line ~2786)

```go
// Before:
ipPath, err := ParseIpPath(ipPacketToProvider.IpPacket.PacketBytes)
if err == nil {
	r, err := self.securityPolicy.Inspect(provideMode, ipPath)

// After:
ipPath, payload, err := ParseIpPathWithPayload(ipPacketToProvider.IpPacket.PacketBytes)
if err == nil {
	r, err := self.securityPolicy.Inspect(provideMode, ipPath, payload)
```

#### 7b — `RemoteUserNatClient.SendPacket` (line ~2912)

```go
// Before:
ipPath, err := ParseIpPath(packet)
if err != nil { return false }
r, err := self.securityPolicy.Inspect(minRelationship, ipPath)

// After:
ipPath, payload, err := ParseIpPathWithPayload(packet)
if err != nil { return false }
r, err := self.securityPolicy.Inspect(minRelationship, ipPath, payload)
```

### Step 8 — Update `ip_remote_multi_client.go` callers

#### 8a — `SendPacket` (line ~996)

Already calls `ParseIpPathWithPayload` — just change the `Inspect` call:

```go
// Before:
r, err := self.securityPolicy.Inspect(minRelationship, ipPath)

// After:
r, err := self.securityPolicy.Inspect(minRelationship, ipPath, payload)
```

#### 8b — `clientReceivePacket` (line ~1367)

```go
// Before:
r, err := self.ingressSecurityPolicy.Inspect(provideMode, ipPath)

// After:
r, err := self.ingressSecurityPolicy.Inspect(provideMode, ipPath, nil)
```

(Ingress doesn't use payload; `nil` means "header-only check".)

### Step 9 — Build and verify

```bash
cd urnetwork-3.23-fix
go build ./...
```

The `SecurityPolicy.Inspect` interface change forces every call site to update — the compiler will flag any misses. There are exactly 4 call sites across 2 files.

---

## What NOT to port

- `docs/IP_SECURITY.md` — documentation only
- `security/main.go` — blocklist generator (fork uses `security/gen.sh` with different feed pipeline)
- Test files (`*_test.go`) — optional, no existing test infra for security policy
- Upstream `net_tls.go` TLS parsing additions — fork has a stripped-down `net_tls.go`; only the `TlsContentType` constants are needed

---

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| Interface signature change breaks callers | Build will catch all; only 4 call sites across 2 files |
| DMCA state machine adds per-flow memory | 16 shards × LRU limit (default 65536 flows); bounded by `MaxFlows` config |
| Blocklist data format change | 66K lines removed (`map[[4]byte]bool`), replaced by 8K lines packed string + binary search. Verify with a smoke test that known-blocked IPs are still blocked. |
| **IPv6 blocking is entirely new** | The old `blockIp4s` was IPv4-only. `cfaaBlockedPrefix6Data` (214 prefix ranges) is new capability — not a migration. First deploy will start blocking CFAA-flagged IPv6 destinations that previously passed through unchecked. This is intentional and correct, but worth noting as a behavior change. |
| Fork's extra imports (`runtime`, `sync/atomic`) in `ip.go` | No conflict — upstream `ip_security_dmca.go` uses `sync/atomic` too |
| No existing security policy tests | Primary verification: compilation + provider binary smoke test with relayed traffic |

---

## Verification

```bash
cd urnetwork-3.23-fix/provider && go build -o /dev/null ./...
```

The build is the primary gate — the interface signature change will surface any missed call sites at compile time.

**Do not run `go test ./...`** — the full suite takes 300s+ and will appear hung (TestWeightedShuffleWithEntropy alone can block indefinitely). If you want to spot-check, use targeted tests with a timeout:

```bash
go test -run TestSecurity -timeout 30s ./...
go test -run TestCfaa -timeout 30s ./...
```

After the build passes, deploy to a test provider node and verify packet relay works on ports 443, 80, 53, and that BitTorrent traffic is blocked.

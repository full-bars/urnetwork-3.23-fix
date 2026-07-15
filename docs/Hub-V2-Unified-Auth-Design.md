# Hub v2: Unified Password-Authenticated Auth (Design Spec)

> **Status: proposal, not approved for implementation.** This document is a design spec to review and refine before any code is written. It is not an executable plan — do not run this through `superpowers:executing-plans` or similar. When the design below is settled, a separate task-by-task implementation plan should be written from it.

## Motivation

The hub currently has **four separate credentials**, each solving a real problem, but accumulated piecemeal and easy to conflate (see [Hub-Setup.md](Hub-Setup.md#understanding-hub-credentials) for the current-state explainer):

1. `URNETWORK_HUB_TOKEN` — a static, fleet-wide shared secret authorizing provider writes (`/api/report`, `/api/heartbeat`, `/api/nodes/remove`) and, as of PR #281, CA-cert auto-bootstrap.
2. `URNETWORK_HUB_DASHBOARD_PASS` — an HTTP Basic Auth password for humans viewing the dashboard (PR #282).
3. The hub's CA password (`hub.password`) — derives the Ed25519 CA used for TLS trust between providers and the hub.
4. The onboard token — a short-lived, single-purpose credential for the CA-cert fetch endpoint during manual onboarding.

Two structural problems remain even after #281/#282:

- **Too many things for an operator to track**, with genuinely confusable names (two different things are both casually called "the token").
- **The provider auto-bootstrap flow (PR #281) has an inherent TOFU gap.** A Bearer token proves the *provider's* identity to the hub, but nothing proves the *hub's* identity to the provider on first contact — there's no way for a static shared secret to do that. PR #281 mitigates this with a verify-first fetch + loud fallback, but for hubs using the password-derived CA (no public trust chain), the fallback is still a real, if narrowed, exposure window.

## Goal

Replace the multi-credential model with **one root password** the operator sets once, from which every other secret (CA keypair, dashboard auth, provider join credential) is cryptographically derived — while using a **Password-Authenticated Key Exchange (PAKE)** for provider onboarding so the TOFU gap is closed structurally, not just narrowed.

## Non-goals

- **Not** forcing existing fleets to migrate. v2 is opt-in and coexists with the current model indefinitely — see [Coexistence](#coexistence-with-v1).
- **Not** changing the dashboard Basic Auth mechanism. #282's `requireBasicAuth` middleware is correct and stays exactly as-is; only where the password *value* comes from changes.
- **Not** implementing this spec's code in this pass. This document is the spec to review.

## Threat model

What v2 needs to resist, framed against the current gap:

- **Passive network eavesdropper** during provider onboarding must learn nothing usable — not the password, not a token, not anything that lets them later impersonate the provider or the hub.
- **Active on-path attacker** (can intercept and inject) during onboarding must not be able to complete a handshake as either party without knowing the password, and must not be able to convince the real provider it's talking to the real hub (or vice versa) without it.
- **Hub compromise** (attacker steals `hub.db` / on-disk state) should not hand over anything directly usable to impersonate a provider — i.e., stored verifier material should not be password-equivalent.
- **Online guessing** must be bounded by rate limiting; PAKE's security guarantee is conditional on the number of *interactive* guesses an attacker can make against the live hub, not offline computation.

Explicitly out of scope: a compromised provider host (its derived credential is only as safe as the host it's provisioned on — no design change fixes that), and DoS resistance of the join endpoint beyond basic rate limiting.

## Protocol choice: OPAQUE

Two real options exist for this: **SPAKE2+** and **OPAQUE**.

| | SPAKE2+ | OPAQUE |
|---|---|---|
| Maturity | Older, widely deployed (Chrome, Matter/Thread commissioning, Magic Wormhole) | Newer, IETF CFRG draft-track |
| Server compromise | Stores a password-derived verifier; some direct exposure if stolen | Stores an opaque envelope; asymmetric — a stolen envelope alone doesn't let an attacker authenticate as the client |
| Go library maturity | `spake2` package inside `psanford/wormhole-william` (battle-tested via Magic Wormhole) | `bytemare/opaque` (actively maintained by a known Go-crypto author) |

**Recommendation: OPAQUE.** The asymmetric server-compromise property matters here specifically because the hub is the thing with the broader attack surface (public dashboard, public report endpoint) — if it's ever popped, we don't want that to also directly hand over provider-impersonation material. This needs to be re-verified against current library maintenance status before implementation starts; do not commit to a specific library without checking its current state first.

## Key derivation: one password, many purposes

```
root password (operator sets once, "hub init")
    │
    ▼  Argon2id (already exactly what today's CA derivation does — unchanged)
  master key
    │
    ├─ HKDF(master key, "urnetwork-hub-ca-v1")        → Ed25519 CA keypair (unchanged from today)
    ├─ HKDF(master key, "urnetwork-hub-dashboard-v1")  → dashboard Basic Auth verifier
    └─ HKDF(master key, "urnetwork-hub-join-v1")       → OPAQUE server setup (envelope/verifier for provider join)
```

Distinct HKDF context labels ensure a leak of one derived secret (e.g. the dashboard verifier, sent — base64, not encrypted — on every Basic Auth request) reveals nothing about the others. `hub init --password X` stays exactly the same command; it just derives more things from `X` than it does today. **Any hub that has already run `hub init` becomes join-capable the moment the binary is upgraded — no new setup required**, which is what makes this compatible with existing deployments.

`URNETWORK_HUB_DASHBOARD_PASS` remains available as an independent override for operators who want a separate dashboard password rather than the derived one — this is additive, not a forced unification.

## Provider join flow (replaces manual `hub link` + onboard token, for opted-in nodes)

```
urnet-tools hub join <hub-address>
# prompts interactively for the password (stdin only — never argv, which
# `ps aux` would expose)
# runs the OPAQUE handshake against a new hub endpoint
# on success, over the now-mutually-authenticated channel, receives:
#   - the CA cert (or skips needing one — the session itself proves identity)
#   - a per-node credential scoped to this provider only
```

This single command replaces three today: minting an onboard token, running the `curl | sh` onboarding script, and separately configuring `URNETWORK_HUB_TOKEN`.

### Why per-node credentials, not one shared token

A side benefit of moving off a single static `URNETWORK_HUB_TOKEN`: each provider gets its own credential, issued at join time. This means a compromised or misbehaving node can be revoked individually (delete its row from a per-node credential table) without rotating the secret for the entire fleet — a real operational improvement over today's all-or-nothing shared token.

### New hub-side surface

- A new join endpoint (HTTP or a raw handshake — TBD in implementation) implementing the OPAQUE registration/login exchange.
- **Rate limiting is load-bearing, not optional** — the hub must throttle/lock out repeated failed join attempts per source IP, reusing the pattern already established by the existing provider-auth rate limiter in this codebase.
- A per-node credential table (likely in `hub.db`, alongside the existing node/proxy tables) mapping issued credentials to node IDs, supporting lookup and revocation.

### `requireAuth` generalization

Today: `requireAuth(token string, next http.HandlerFunc)` compares the request's Bearer token against one constant via `subtle.ConstantTimeCompare`. v2 needs it to accept **either**:
1. The legacy static `URNETWORK_HUB_TOKEN` (unchanged, for v1 nodes), or
2. A valid per-node credential looked up from the credential table (v2 nodes).

This is additive to the existing function, not a rewrite — the write endpoints (`/api/report`, `/api/heartbeat`, `/api/nodes/remove`) don't change shape, they just gain a second acceptable credential form.

## Coexistence with v1

This is a hard requirement per the direction settled on for this design, not an afterthought:

- **v1 is never deprecated by this design.** `URNETWORK_HUB_TOKEN`, `hub link`, the onboard-token flow, and `bootstrapHubCA`'s verify-first logic (just shipped in PR #281) all continue to work unmodified, indefinitely.
- **v2 is opt-in per node.** A fleet can run a mix of v1 and v2 nodes simultaneously; nothing forces a fleet-wide cutover.
- **No hub-side flag switches modes.** The hub simply supports both auth paths at once — there's no "v1 mode" vs "v2 mode" toggle to get wrong; each request is just checked against whichever credential forms are currently valid.
- **#281's verify-first fix remains the correct, load-bearing safety net for every node that stays on v1** — which, given opt-in adoption, may be most nodes for a long time. It was not wasted work; it's the permanent safety story for anyone who never migrates.
- **#282's dashboard Basic Auth is essentially unaffected** — v2 only changes where the password value can optionally come from (derived vs. independently set), not the enforcement mechanism.

## Open questions to resolve before an implementation plan is written

1. **Library**: confirm `bytemare/opaque` (or the alternative chosen) is actively maintained and suitable, with a from-scratch review of its API surface — don't assume based on this document.
2. **Wire protocol shape**: plain HTTP endpoint carrying OPAQUE's protocol messages as JSON/binary blobs, vs. a raw TCP/WS upgrade — needs a concrete decision informed by how the library expects to be driven.
3. **Per-node credential storage**: schema, revocation UX (`urnet-tools hub revoke <node-id>`?), and how a revoked node's next report attempt fails (and how visible that failure is in the dashboard/logs).
4. **Rate-limit tuning**: what failure threshold/lockout window for the join endpoint, and whether it should share infrastructure with the existing provider-auth rate limiter or be independent.
5. **`hub init` behavior**: does it need a flag to opt a hub into serving the join endpoint at all, or is it always available once any password is set (with the join endpoint itself gated by "has a password ever been set")?
6. **Testing strategy**: PAKE correctness needs particular care in test design — a subtly wrong implementation can look correct in the naive golden-path test while being broken against an active attacker. Needs explicit adversarial test cases (wrong password, replayed handshake, truncated messages), not just happy-path coverage.

## Suggested rollout phases

1. This design spec — reviewed and settled (current phase).
2. A task-by-task implementation plan derived from the settled design (separate document, via `superpowers:writing-plans`).
3. Prototype the join handshake in isolation — its own package, feature-flagged, zero changes to existing endpoints or `requireAuth`.
4. Extend `requireAuth` and the write endpoints to accept the second credential form, still with no user-visible behavior change until a node actually opts in.
5. Dogfood on a single fleet node.
6. Gradual, voluntary rollout across the rest of the fleet — v1 never forced to retire.

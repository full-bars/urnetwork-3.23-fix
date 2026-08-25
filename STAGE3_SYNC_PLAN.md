# Stage 3 — Repo Mirroring & meso-miner Sync Plan

PURPOSE: keep `urnetwork-3.23-fix` (Repo A) and `full-bars/meso-miner` (Repo B)
in sync during the transition, and define the one-way promotion path to
`urfoundation/meso-miner` (Repo C — the CURATED public copy). Repo C MUST be
gotten right the FIRST time: no room for mistakes, explicit in-the-moment user
approval required before anything touches it.

## Hard rules (from CLAUDE.md / user — still in force)
- Repo C (`urfoundation/meso-miner`) is NEVER touched without explicit
  in-the-moment user approval. This plan only prepares for it.
- Every GitHub write (push/PR/comment/branch) needs per-action approval with
  exact verbatim text shown first.
- `progress.md` is gitignored/untracked; write atomically.
- PRIVACY SCRUB is a hard gate before Repo C: no real usernames (full-bars, mesocyclone,
  user, etc.), no `/home/...` or `C:\Users\...` paths, no host IPs. Use generic
  placeholders (e.g. `example-net`, `provider-host`). Public commit messages,
  release notes, and wiki MUST be scrubbed.

## Current state (2026-08-25)
- Repo A: /home/full-bars/ur/urnetwork-3.23-fix — active development, PR #465 open.
- Repo B: full-bars/meso-miner — NOT cloned locally yet (verify path; may live
  under /home/full-bars/ur/full-bars/meso-miner or a separate checkout). Mirror edits
  applied there only after A is verified.
- Repo C: urfoundation/meso-miner — curated public copy. Frozen until explicit go.
- 3.23-fix is NOT to be frozen until its users migrate (user rule).

## Mechanism: dual-apply changes to A and B
1. For doc (and later code) changes, make the edit in Repo A first, verify
   (build + go test where applicable, or at minimum a docs grep/lint).
2. Clone/checkout Repo B into its own worktree. Apply the SAME edit there
   (path may differ — e.g. B may have already stripped the hub component, so
   verify the analogous location rather than blind copy).
3. Verify B independently. Only then do both land (PR per repo, or a sync
   commit on each). Keep a one-line checklist per change:
   [ ] A edited  [ ] A verified  [ ] B edited  [ ] B verified  [ ] privacy ok.

## What freezes when
- Repo A stays unfrozen (users still on 3.23-fix). Do NOT set a freeze date
  unilaterally. Freeze is deferred until migration completes (user decision).
- Repo B mirrors A continuously during transition — no independent drift.
- Repo C stays read-only to us until promotion.

## Promotion to Repo C (curated, one-way, irreversible-ish)
Sequence — each gate requires explicit user sign-off before proceeding:
1. Change lands + verified in Repo A.
2. Mirrored + verified in Repo B.
3. User approves a line-by-line diff of A-vs-C for the changed files.
4. PRIVACY SCRUB gate: grep the proposed C commit for
   `full-bars|mesocyclone|/home/|C:\\Users|192\.168|10\.|172\.16|100\.` etc.
   Any hit = block until scrubbed. Use generic placeholders.
5. Promotion to C as a CURATED, SQUASHED, public-safe commit:
   - No internal commit-message chatter, no "fix typo from review thread".
   - Public-safe summary; no personal/host context.
6. After push to C, re-verify C builds + tests. Report the C commit hash.

## Open questions to confirm with user before any C work
- Exact Repo B path / clone URL.
- Whether B should receive code changes too (not just docs) during transition.
- Promotion trigger: per-release batch, or per-change?

## Status
- 2026-08-25: Stage 2 docs typo (`ecoramlogs` -> `eco`/`ramlogs`) fixed in Repo A
  (docs/Installation.md). Repo B mirror edit PENDING (Repo B not cloned locally).
  Stage 3 plan written; no action taken on Repo C (hard rule: not without approval).

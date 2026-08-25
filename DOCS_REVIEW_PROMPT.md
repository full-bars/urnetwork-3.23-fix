# Code/Docs Review Request — Stage 2 docs cleanup + Stage 3 sync plan (branch docs/stage2-cleanup, repo urnetwork-3.23-fix)

You are reviewing a DOCS-ONLY change on a Go provider-management CLI repo. Two things changed:
1. docs/Installation.md line 97: `ecoramlogs`/`optimize` typo fixed to `eco`/`ramlogs` (the real
   commands — confirmed correct at lines 119-120 and in AI.md).
2. STAGE3_SYNC_PLAN.md: a new plan file for keeping urnetwork-3.23-fix (Repo A) and
   full-bars/meso-miner (Repo B) in sync, and the one-way promotion to urfoundation/meso-miner
   (Repo C, the curated public copy).

## What I need you to verify DEEPLY (read-only; use Bash/grep/diff freely)
The diff itself is trivial (1 typo + 1 new doc). The risk is in the CLAIMS, not the edits:

A. DEDUPE CLAIM — the change asserts "no duplication" among AI.md, PROJECT_STRUCTURE.md,
   FORK_CHANGES.md, README, and any wiki/internal-plan docs. VERIFY this yourself:
   - Do AI.md and PROJECT_STRUCTURE.md and FORK_CHANGES.md actually overlap in content, or are
     they distinct? Grep for duplicated sections/prose.
   - Is there a `legacy-urnetwork-tools-backup/` dir, a `superpowers/` dir, or a `wiki/` dir that
     holds stale/duplicate docs? (The handoff plan claimed these existed pre-consolidation — confirm
     they're gone or flag what's still there.)
   - Any other .md with duplicated install/hub/architecture content that should be deduped but wasn't?
   Report concrete file:line evidence, not assertions.

B. STAGE3_SYNC_PLAN.md CORRECTNESS/SAFETY — this is a PLAN that constrains future irreversible
   actions on the PUBLIC repo (Repo C). Scrutinize:
   - Does it correctly enforce: Repo C untouched without explicit in-the-moment approval? Privacy
     scrub gate (no real usernames like klets/mesocyclone, no /home/ or C:\Users\ paths, no host IPs)?
   - Is the dual-apply (A<->B) mechanism concrete enough to actually follow? Any missing step?
   - Does it respect "3.23-fix NOT frozen until users migrate"? Any contradiction?
   - Any factual error (wrong repo path assumption, wrong command, unsafe suggestion)?
   - Is it internally consistent (e.g. it says Repo B not cloned locally — does the rest of the plan
     still hold, or does it assume B is present)?

C. The `ecoramlogs` fix — confirm `eco`/`ramlogs` are the correct commands (grep AI.md and
   docs/Installation.md lines 119-120 for the canonical usage). Any other stale command name in the
   repo (grep for `optimize` as a command, `ecoramlogs`, or similar typos)?

## Output format
Severity-ranked findings: [HIGH|MEDIUM|LOW] file:line — description — suggested fix.
For the dedupe claim, state explicitly: VERIFIED-NO-DUPES or LIST-THE-DUPES (with evidence).
For STAGE3 plan: APPROVE-AS-IS or LIST-CONCERNS (with file:line in the plan).
End with: VERDICT: APPROVE or VERDICT: REQUEST_CHANGES.
Work INLINE — do not spawn background agents. If you cannot finish, prioritize findings over breadth.

The diff and the relevant docs are in this directory (snapshot at branch head). The full diff is
appended below; the docs themselves (AI.md, PROJECT_STRUCTURE.md, FORK_CHANGES.md, docs/Installation.md,
STAGE3_SYNC_PLAN.md, README.md) are readable files in this tree.

=== DIFF (5897e0d1 vs base 3db6a8b6) ===

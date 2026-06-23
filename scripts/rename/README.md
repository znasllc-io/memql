# SI → AI rename tooling (epic memql#1889)

A curated, identifier-boundary, allowlist/denylist renamer for the
"SI / AI" → **AI** sweep. Dry-run by default; emits a
unified diff. Re-runnable and idempotent.

## Why a tool (not `sed`)
"SI" is a substring of dozens of unrelated tokens: `SIGTERM`, `SESSION`,
`SIZE`, `SIMLI` (avatar vendor), `SIP*` (LiveKit telephony), `POSIX`,
`TRANSITION`, `CLASSIFICATION`, `GENESIS`, `SID`. A blind substitution
corrupts all of them. This tool only ever applies the **explicit rules** in
`rules.json`, each matched at identifier boundaries, and a denylist scan
refuses any match that would touch a protected span.

## Files
- `rules.json` — the curated allowlist (grouped) + the denylist.
- `si_to_ai.py` — the engine (rename + G1 scan).
- `test_si_to_ai.py` — self-test (boundary safety, denylist, idempotency).

## Usage
```bash
# Dry-run a group over a repo (diff to stdout, summary to stderr):
python3 scripts/rename/si_to_ai.py --root . --group go-internal

# Dry-run everything applicable:
python3 scripts/rename/si_to_ai.py --root ../copresent

# Apply:
python3 scripts/rename/si_to_ai.py --root . --group proto --apply

# G1 verification scan (residual SI-as-AI identifiers, exit 1 if any):
python3 scripts/rename/si_to_ai.py --root . --scan

# Self-test:
python3 scripts/rename/test_si_to_ai.py
```
`--root` is the repo to operate on (worktrees, `vendor/`, `node_modules/`,
`dist/`, `memql-rel*` sibling checkouts are always skipped).

## Rule groups (map to epic issues)
| group | issue | scope |
|---|---|---|
| `dsl-keyword` | 1.2 | `si(` → `ai(` in `.memql` |
| `go-internal` | 1.3 | Go identifiers (`InvokeSI`, `*SIProvider`, `SIExpression`, …) |
| `proto` | 1.4 | wire family (`SIChat*`, `SIForward*`, `SIStream*`, `SITranscribe*`) + snake fields |
| `dsl-identifiers` | 1.2/1.5 | DSL-derived names crossing Go/`.memql`/TS (`autoJoinSI`, `mutationJoinSpaceAsSI`, …) |
| `frontend` | 1.6 | SPA app identifiers (`isSITyping`, `hasSIParticipant`, …) |

## Casing convention (two, each internally consistent)
- **Wire/proto family → `Ai`** (title case): `SIChatMsg` → `AiChatMsg`. Matches
  protoc-generated Go/TS and the documented target in `CLAUDE.md`.
- **Pure Go/DSL/frontend identifiers → `AI`** (all-caps initialism):
  `SIExpression` → `AIExpression`, `isSITyping` → `isAITyping`. Matches Go
  initialism idiom and the epic spec (`AIExpression`).

## Hard denylist (never renamed)
`SIP*` (telephony; precise `SIP(?![a-z])…` so `SIProvider` still renames),
`TSInterface*`/`TSImport*`/`CSS*`/`CJS*` (TS/CSS AST), and English words:
`POSIX`, `VERSION`, `MISSING`, `INSIDE`, `OUTSIDE`, `ANALYSIS`, `DECISIONS`,
`SILENTLY`, `PERSISTS`, `EPSILON`, `UNSIGNALED`, `SID` — plus the empirically
found families `SESSION*`, `SIZE*`, `SIMILAR*`, `TRANSITION*`, `SIGNAL*`,
`SIMLI*`, `GENESIS*`, `CLASSIFICATION`, `ASSISTANT*`.

## Out of scope (deferred — NOT identifiers)
Two SI tokens are runtime/stored contracts, not code identifiers, so they are
denylisted here and tracked as follow-ups (renaming them needs a coordinated
reseed/migration, not a code sweep):
- **Env var names** `MEMQL_SI_*`, `VITE_SIMLI_*`, `MEMQL_SIMLI_API_KEY` — secret-store
  + genesis-envelope contract.
- **Stored data value** `participantType: "si"` — DB row data. Identifiers that
  *read* the value are renamed; the value string stays `"si"`.

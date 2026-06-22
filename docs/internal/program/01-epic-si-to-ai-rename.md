# Epic 1 — AI → AI rename

Rename "AI / AI" to "AI" across DSL, Go, wire/proto, and
frontend, in one coordinated sweep. **Runs first** (before decoupling).
**Session: S1. Gate produced: G1.**

**Repos:** `memql`, `memql-bff-copresent`, `memql-cockpit`, `copresent`.

## Centerpiece
The author-facing DSL construct is `si("promptName", args)` — the blocking
LLM-invocation function (`agent()` is the async/orchestrated sibling). Marquee
rename: **`si()` → `ai()`**, grammar node `SIExpression → AIExpression`.

## Hard denylist (must NOT be renamed)
`SIP*` (SIP telephony trunk code), `TSInterface*`/`TSImport*`/`CSS*`/`CJS*`
(TS/CSS AST node names in the frontend), and English words containing "AI":
`POSIX`, `VERSION`, `MISSING`, `INSIDE`, `OUTSIDE`, `ANALYSIS`, `DECISIONS`,
`SILENTLY`, `PERSISTS`, `EPSILON`, `UNSIGNALED`, `SID` (review individually).

---

## Issue 1.1 — Build the rename tooling: curated allowlist/denylist [foundation]
**Problem:** A blind `sed` corrupts `SIP*`, `TS*`, and English words.
**Approach:** Produce a scripted, word-boundary, case-preserving renamer driven
by an explicit **allowlist** of AI-identifiers (see buckets below) and the
**denylist** above. Dry-run mode emits a diff for review. Covers `.go`,
`.memql`, `.proto`, `.ts/.tsx`, `.md`.
**Acceptance:** Dry-run over all repos shows only intended identifiers; zero
denylist hits. Script is re-runnable and idempotent.
**Gates:** produces the tooling all other 1.x issues use.

## Issue 1.2 — Rename DSL keyword `si()` → `ai()` + grammar (core) [G:1.1]
**Approach:** Rename the construct keyword, `SIExpression → AIExpression`, the
parser/classifier registration (`component/memql` spec-kind table), and all
`si(...)` call sites in core `dsl/**/*.memql`.
**Acceptance:** Core DSL parses/executes with `ai(...)`; old `si(...)` rejected
(or aliased with a deprecation per decision). Conformance tests green.
**Files:** `component/memql/*` (spec kind registry), `dsl/**/*.memql`.

## Issue 1.3 — Rename internal Go identifiers (core) [G:1.1] [P with 1.2]
**Scope:** `InvokeSI`, `InvokeSIStructured`, `SIInvocation`, `*SIProvider`
(`ChatSIProvider`, `EmbeddingSIProvider`, `TTSSIProvider`,
`ToolCallingChatSIProvider`, `openSIProvider`), `generateSIResponse`,
`insertSIResponse`, `proxySI`, files `core/common/si.go`,
`component/memql/si_providers.go`.
**Acceptance:** Core compiles; tests green; no internal AI identifiers remain
(outside denylist).

## Issue 1.4 — Rename wire/proto names + regenerate [G:1.1] (sub-gate for 1.5/1.6)
**Scope:** `SIForward*`, `SIChat*`, `SIStream*`, `SITranscribe*` in
`component/grpc/memql.proto`, `component/node/node.proto`,
`component/polyphon/proto/polyphon.proto`; regenerate `*.pb.go`.
**Approach:** This is the breaking-contract step. Rename proto messages/RPCs,
regenerate Go, bump the wire version. **Must land before 1.5 and 1.6** so
dependents regenerate against the new contract.
**Acceptance:** Protos regenerate cleanly; all nodes compile against new
contract; a cross-node smoke test passes.

## Issue 1.5 — Propagate rename to CoPresent BFF pack [G:1.4]
**Scope:** `si()` → `ai()` in `memql-bff-copresent/dsl/copresent/*.memql`;
AI-named generated Go/args (`MutationJoinSpaceAsSIArgs`, `LogicAutoJoinSIArgs`,
`buildQueryHasSIResponseForReply`, etc.); regenerate against the new wire.
**Acceptance:** BFF builds + tests green against renamed core/wire.

## Issue 1.6 — Propagate rename to frontend [G:1.4] [P with 1.5]
**Scope:** `copresent` SPA: regenerate TS from the new protos; rename app
identifiers (`isSITyping`, etc.). **Do not touch `SIP*` or `TS*` AST names.**
**Acceptance:** Frontend type-checks + builds against the regenerated gRPC TS.

## Issue 1.7 — Docs, comments, descriptions + Epic verification [G:1.2,1.3,1.4,1.5,1.6]
**Scope:** Prose "AI"/"AI" in `@description(...)`, comments,
`*.md`. Then full verification.
**Acceptance (G1):** All four repos build + test green; a repo-wide scan shows
zero AI-as-synthetic-intelligence identifiers outside the denylist. Run the
final scan via a fresh verification sub-session.

---

## Parallelization within S1
`1.1` first → then `1.2 [P] 1.3` (core) and `1.4` (proto) → `1.4` gates
`1.5 [P] 1.6` → `1.7` closes the epic and **opens G1**.

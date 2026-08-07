# Grouping the unlabelled backlog into epics

Date: 2026-08-07

## Problem

Nine open issues in `znasllc-io/memql` carry no `epic:*` label. They are not
claimable as a coherent body of work: `build-issues` takes them one at a time
with no ordering, two of them are `suggestion`-labelled and so are not claimable
at all, and one (#3228) is an XL programme filed as a single task.

This design groups all nine into three epics, decomposes #3228, and records the
four decisions the issues themselves left open.

## The cut

Three epics, each with a stated principle.

### Epic A -- `seam-contract-fidelity`

> A seam whose behaviour contradicts its own contract.

Four of the nine are deferred findings from the `auth-surface-hardening` trace
(closed epic #3204), and each is code that reads as load-bearing and is not: a
carrier documenting claims it never carries (#3219), a field that silently
stopped crossing a hop (#3221), a sweep returning nil that is indistinguishable
from "there are no users" (#3217), and a registry resolving *a* specialist
rather than *the caller's* (#3216).

### Epic B -- `repo-truth-guards`

> A documented mechanism that no longer matches the tree, and that nothing
> verifies.

The sentence is lifted from #3215. Four issues share the shape: `VERSIONING.md`
against the actual tags (#3214), a CI filter bucket contradicting its own
comment (#3213), `go:generate` directives naming three paths that do not exist
(#3215), a script writing into a deleted directory for a deleted consumer
(#3225). Three of the four ask for a guard demonstrated red before the fix; the
guards, not the individual fixes, are the epic's deliverable.

### Epic C -- `module-boundaries`

> The dependency direction is enforced by the compiler, or it is a convention.

#3228 becomes the epic issue rather than a task under a new one: its body
already is an epic body -- the corrected measurements (183 packages, 40 `wire`
importers, ~29 `go.mod` files rather than 8), the `GOWORK=off` finding, and the
two struck criteria. Its acceptance criteria become the ordered sub-tasks.

**Why A and B are separate.** Both are contract-versus-reality, but A is runtime
engine seams verified by tests and B is repo/build/docs metadata verified by
guards. Different verification, different blast radius.

## Decisions

| # | Decision | Consequence |
|---|---|---|
| A | #3228 becomes its own decomposed epic | Eight ordered sub-tasks. It is a programme -- ~29 `go.mod` files, a cross-repo prerequisite, and a hard precondition -- not a task. |
| B | #3214 and #3213 live in Epic B, not Epic C | Both were found while researching the module split and both feed it, but by nature they are drift fixes with guard-shaped remedies. C8 carries a cross-epic dependency on B1; that link is sufficient. |
| C | The tag line resumes `vX.Y.Z` | #3214 option (a). Option (b) forecloses publishing `wire` as a Go submodule, which contradicts Epic C's intent. The Go module line is unfrozen from `v0.9.6`. |
| D | `memql-cockpit` coordinates via a per-module `replace` set | Extends the pattern cockpit already relies on (`replace github.com/znasllc-io/memql => ../memql`). A `go.work` on the cockpit side was rejected: workspace mode is exactly what masks the boundary violations Epic C exists to catch. |
| E | #3216 splits into two tasks | The delivery-seam fix is a standalone engine defect with value independent of specialist routing, and it is a hard prerequisite for the owner-keyed lookup. |
| F | The specialist registry key reuses the owner already stamped | Space owner on the voice hop, AccessContext user on chat. Consistent with #1503 and requires no new resolution. |

### Why decision C is forced

`VERSIONING.md` declares git tags the single source of truth in the form
`vX.Y.Z`. The tags actually cut are unprefixed (`0.14.0`), which Go cannot
resolve as a module version, so `proxy.golang.org` still answers `v0.9.6` from
2026-06-02. Publishing a submodule on top of a release line the proxy already
disagrees with produces an unrecoverable `ambiguous import` for consumers.

### Why decision D is forced

Cockpit pins `memql v0.0.0-20260623073124-7c7d1350e667` with a `replace`
covering only the root module path. That redirect does not catch a nested
module, 30 cockpit files import `wire` directly, and the pre-split pin yields
`ambiguous import` -- which has no consumer-side fix. The coordination must land
before or with the first `go.mod`.

### Why decision E is forced

`integrations/agent` never calls `common.ContextWithToolDefaults`, and
`applyToolDefaults` strips `@autoInjected` fields whether or not a server
default is present -- pinned today by
`TestApplyToolDefaults_AutoInjectedStripsEvenWhenNoDefaults`. So a server-stamped
owner on the agent path is both LLM-forgeable (without `@autoInjected`) and
dropped at dispatch (with it). The lookup cannot be scoped until the seam is
fixed.

## Tasks

### Epic A -- `seam-contract-fidelity`

| # | Issue | Task | Size | Depends |
|---|---|---|---|---|
| A1 | new | Fix the tool-defaults delivery seam on the agent path. Audit which tools are already affected -- it may be wider than `askSpecialist`. | M | -- |
| A2 | #3216 | Key the specialist registry per owner. Test requires a positive control: the owning user hits the expected row id, then the other user misses. A miss-only assertion is satisfied by an empty map. | M | A1 |
| A3 | #3221 | Add provenance-only `first_name`/`last_name` to `ForwardedAuthority`. The guard rails are the deliverable. Lift from closed branch `issue/3205-mesh-forwarded-auth` (`cd07e468`). | S | -- |
| A4 | #3219 | Adopt `ForwardedAuthority` on `WorkbenchForwardRequest`. No `map<string, string>` auth carrier left in `node.proto`. | S | A3 |
| A5 | #3217 | Add the `case *ExecuteResult` arm to `extractRowIds`; sweep for other instances of the shape. Verified on the `db-tests` lane. | S | -- |

A5 turns on a startup sweep that has been inert. In a cluster with existing
users that means per-user seed materialization for all of them on the next boot
-- intended, but real blast radius, which is why it wants a live database rather
than folding into a security commit.

### Epic B -- `repo-truth-guards`

| # | Issue | Task | Size |
|---|---|---|---|
| B1 | #3214 | Resume `vX.Y.Z` tags. Document how the `0.9.90`-`0.14.0` lineage relates to the v-line and state the Go-module consequence explicitly. | S |
| B2 | #3213 | Add `component/bus/gen/**` to the `proto` bucket, plus a guard asserting the bucket covers every `gen/` tree that exists. Guard red first. | S |
| B3 | #3215 | Delete both dead `go:generate` directives, plus a guard that every `//go:generate` in the tree references paths that resolve. Guard red first. | S |
| B4 | #3225 | Delete `scripts/migrate-seeds/`. The live `SeedMaterializer` path is untouched. | XS |

### Epic C -- `module-boundaries`

Strictly ordered. Every task keeps the tree building and green.

| # | Task | Gate |
|---|---|---|
| C1 | Land the per-module `replace` set in `memql-cockpit` and record the mechanism. No `go.mod` in memql before this merges. | Blocking |
| C2 | Demote `component/{auth,bus,events,safety}` to the base tier. Own commit, zero code changes, resolves the engine/platform mutual requirement. | -- |
| C3 | `wire` lands first and alone (L0: `component/{grpc,node,bus}/gen`) together with a required `GOWORK=off` per-module CI lane. Demonstrate a downward violation failing to compile, then revert. | -- |
| C4 | `base` module, including the seven `component/*` leaf carve-outs. | -- |
| C5 | `engine` module. `embed_inventory_test.go` is the gate. | -- |
| C6 | `platform` module. O4 decided explicitly in the PR body. | -- |
| C7 | `integrations`, the server modules, `app`. | -- |
| C8 | Independent version lines for `wire` and `engine` only; every other module lockstep. | Needs B1 |

Carried across every C task: package count stays 183; `go build ./...` and
`go test ./...` green across all modules; all eight tagged binaries build with
sizes recorded before and after (a change beyond ~10% is a finding to
investigate, not an automatic failure, and the figures in
`docs/public/build/build-tags.md` and `CLAUDE.md` are stale so must not be gated
on); the `changes` bucket paths are unchanged by the split itself.

C5 is where `go:embed` risk concentrates: a `go.mod` in a subdirectory of an
embedded tree drops files with `go build` exit 0 and no diagnostic. The
`embed_inventory_test.go` guard added in #3226 pins 11 packages / 270 files and
is what catches this. Keep it green rather than trusting the build.

**Preconditions verified green on `main` on 2026-08-07**:
`go test -run 'TestAreaGraph|TestEmbed' .` passes, covering #3164's DAG
assertion and #3226's embed inventory.

## Repository changes

House convention, confirmed against `epic:auth-surface-hardening` and
`epic:concept-schema-fidelity`: task issues carry exactly `task` +
`epic:<slug>`; epic issues carry `epic, feature, epic:<slug>`; epics track
children by number in the body, not through GitHub sub-issues.

**Labels created** (colour `#5319E7`, matching the family):
`epic:seam-contract-fidelity`, `epic:repo-truth-guards`,
`epic:module-boundaries`.

**Issues created** (11): two epic issues for A and B; task A1; tasks C1 through
C8.

**Issues relabelled** (9):

| Issue | Change |
|---|---|
| #3228 | Retitled `Epic: module-boundaries`; `task` replaced by `epic, feature, epic:module-boundaries`. Body kept, decisions appended. |
| #3221, #3219 | `+ epic:seam-contract-fidelity` |
| #3216, #3217 | `suggestion` -> `task`, `+ epic:seam-contract-fidelity` |
| #3213, #3214, #3215 | `+ task`, `+ epic:repo-truth-guards` |
| #3225 | `+ task`, `+ epic:repo-truth-guards` |

Moving #3216 and #3217 off `suggestion` is a triage approval: under the
`znas-skills` convention `suggestion` means an owner reviews before anyone
builds, and such issues are not claimable. Both are well-evidenced, and #3216's
central claim was verified against the tree while writing this design.
Descriptive labels (`bug`, `hygiene`, `documentation`, `versioning`,
`github-actions`) are kept alongside `task` rather than stripped.

**Not done here**: no GitHub sub-issue links, no milestone changes, no issue
closures, and nothing touched in `memql-cockpit` -- C1 is filed as work, not
performed.

## Success criteria

- Every open issue in the repository carries an `epic:*` label.
- Each epic states its principle and lists its tasks by number.
- The four decisions above are recorded on the issues that were blocked on
  them, so no task begins with an open question at its centre.
- No task in Epic C can start before its predecessor lands, and C1 blocks all
  of them.

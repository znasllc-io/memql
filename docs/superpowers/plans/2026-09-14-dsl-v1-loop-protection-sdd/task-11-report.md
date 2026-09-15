# Task 11 report: the architecture model renders the graph (#5384)

## Status: CHECKPOINT -- research complete, ZERO code changes made yet

Session ran out of credits during the investigation phase, before any Write/Edit
call. `git status --short` in the worktree is clean; there is nothing to commit.
This report exists so a continuation does not have to re-derive the findings
below from scratch.

## Checkpoint: done / partial / not started, per brief item

- **`component/architecture/model/model.go`** (`KindAutomation`, `EdgeTriggers`
  in the closed sets, with doc lines): NOT STARTED. Read the file; know exactly
  where to add (`Kind` const block at line ~30-38, `EdgeKind` const block at
  line ~47-88, following the existing doc-comment style on each constant).
- **`component/architecture/model/ids.go`** (`AutomationID`): NOT STARTED. Read
  the file; the pattern is a one-line `fmt.Sprintf` constructor like the other
  seven (`ClusterID`, `ServiceID`, etc.) at the bottom of the file.
- **`cmd/memql-arch/automations.go`** (new, `addAutomations`): NOT STARTED, but
  fully designed -- see "Design for addAutomations" below.
- **`cmd/memql-arch/main.go`** (`--automations` flag, call after `extract.Run`):
  NOT STARTED. Read the file in full (97 lines); the call site is after
  `extract.Run` returns `m` and before the `--reproducible` blanking / write,
  guarded by `if *withAutomations { ... }`.
- **`Makefile` `arch-model` target** (append `--automations`): NOT STARTED.
  Located at Makefile line ~494-496 per the brief; not yet re-read in this
  session but the edit is a one-token append to the existing `$(GO) run
  ./cmd/memql-arch ...` line.
- **CI path filter** (`.github/workflows/ci.yml`, the `gates:` block) and
  **gate-input rows** (`scripts/dev/gate_inputs_lane_scope_test.go:79,131`):
  NOT STARTED. Both files fully read and understood -- see "CI filter
  structure" below, which resolves a real ambiguity in the brief before any
  edit is made.
- **`component/architecture/CLAUDE.md`** (the ":235 pure Go" line, the ":63
  Planned ERD line): NOT STARTED. Read the file in full; exact current text
  identified below.
- **Tests** (`component/architecture/model_automations_test.go`,
  `cmd/memql-arch` unit test for `addAutomations`): NOT STARTED. TDD step
  (write tests first, expect FAIL) never reached.
- **`make arch-model` regeneration + size check + node/edge delta**: NOT
  STARTED.
- **Commit**: NOT STARTED. Nothing to commit -- worktree is clean.

## Design for addAutomations (worked out, not yet written)

Signature per brief: `func addAutomations(m *model.Model, root string) error`
in package `main` (`cmd/memql-arch`), called from `main.go` after
`extract.Run`. `root` is accepted for signature symmetry with the other
extractor passes but is NOT actually needed: the automation loader reads
`dsl.Tree()` (the embedded DSL tree baked into the `dsl` Go module at build
time via `//go:embed`), not a filesystem path passed at runtime, and
`cmd/memql-arch` is built fresh by `go run` from this exact checkout, so the
embedded tree always matches `root` in practice. Do not try to thread `root`
into the DSL loader.

Build pattern -- copy `component/memql/lint_parity.go`'s `LintUnifiedTree`
construction (lines ~70-110 in that file) but WITHOUT its overlay-mounting
half (`MountOverlayDomains`) and without its global-state restore `defer`
(unneeded: `cmd/memql-arch` is a one-shot process that exits right after, and
`addAutomations` runs once, not repeatedly):

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
)

func addAutomations(m *model.Model, root string) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		return fmt.Errorf("addAutomations: loading concepts: %w", err)
	}
	registry := concept.DefaultRegistry()

	eng, err := memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		return fmt.Errorf("addAutomations: constructing offline engine: %w", err)
	}
	eng.Logger = logger
	if err := eng.Init(registry); err != nil {
		return fmt.Errorf("addAutomations: initializing offline engine: %w", err)
	}

	loader := automations.NewLoader(automations.LoaderOptions{
		Logger:    logger,
		Registry:  registry,
		Functions: eng.Functions(),
	})
	g, err := loader.StaticGraph()
	if err != nil {
		return fmt.Errorf("addAutomations: building the static loop graph: %w", err)
	}

	clusterID, err := clusterNodeID(m) // scan m.Nodes for the one Kind==KindCluster node; error if != 1
	if err != nil {
		return fmt.Errorf("addAutomations: %w", err)
	}

	for _, a := range g.Automations {
		m.Nodes = append(m.Nodes, model.Node{
			ID:     model.AutomationID(a.Name),
			Kind:   model.KindAutomation,
			Name:   a.Name,
			Parent: clusterID,
			Source: automationSourceRef(a.Origin), // parse "unified:<path>:<name>" -> file (line omitted; not cheaply available)
			Attrs: pruneEmptyLocal(map[string]string{ // local helper; extract.pruneEmpty is unexported to another package
				"stratum":  strconv.Itoa(a.Stratum),
				"trigger":  a.Trigger,
				"schedule": a.Schedule,
				"filter":   a.Filter,
				"loop":     renderLoop(a.Loop),   // e.g. "maxDepth=3 until=row => row.count >= 3"
				"mode":     renderMode(a.Mode),   // e.g. "queued" or "parallel max=5"
				"writes":   strings.Join(a.Writes, ","),
				"origin":   a.Origin,
			}),
		})
	}
	for _, e := range g.Edges {
		m.Edges = append(m.Edges, model.Edge{
			From: model.AutomationID(e.From),
			To:   model.AutomationID(e.To),
			Kind: model.EdgeTriggers,
			Attrs: pruneEmptyLocal(map[string]string{
				"concept": e.Concept,
				"topic":   e.Topic,
				"decided": strconv.FormatBool(e.Decided),
				"via":     strings.Join(e.Via, ","),
			}),
		})
	}
	return nil
}
```

Open questions to settle while implementing (none blocking, just decisions):

1. **Cluster node lookup.** `main.go` calls `extract.Run` (which internally
   calls `ExtractCluster` as its LAST step, per
   `component/architecture/extract/cluster.go`) before `addAutomations` would
   run, so `m.Nodes` holds exactly one `Kind == model.KindCluster` node by
   then. Scan for it rather than recomputing the cluster name logic
   (`extract.Run`'s default-from-folder-name fallback is unexported and
   duplicating it risks drifting); error out if the count is not exactly 1
   (mirrors `TestCommittedModelClusterIsPinned`'s own expectation).
2. **Source ref parsing.** An automation's `Origin` is `unified:<path>:<name>`
   (confirmed against `automationFile` in `component/automations/loop_check.go`
   line ~156, which already does exactly this strip: cut `unified:` prefix,
   cut `:<name>` suffix, fall back to the raw origin string otherwise). Reuse
   that EXACT logic rather than re-deriving it independently (either by
   depending on `automations.automationFile` if it were exported -- it is not,
   it's unexported -- or by duplicating the same two `strings.Cut`-style
   operations locally in `cmd/memql-arch`). Line number: brief says "omit the
   line" when not cheaply available -- there is no line number carried
   anywhere on `GraphAutomation`/`Automation`, so `Source.Line` should stay 0
   (omitted, since `SourceRef.Line` has `json:"line,omitempty"`).
3. **`loop` / `mode` attr rendering.** No existing convention to copy (these
   annotations have no `String()` method and no prior text-rendering site
   found in `component/automations`). Free to invent a compact human-readable
   form; the brief's acceptance test only pins the `stratum` attr's presence
   on `automation:routeRequest`, not the exact string for `loop`/`mode`.
4. **`pruneEmpty`-equivalent.** `component/architecture/extract.pruneEmpty`
   (in `packages.go` line ~309) is unexported and in a different package
   (`extract`, not `main`), so `cmd/memql-arch` needs its own tiny copy (same
   logic: strip empty-string values, return nil map if the result is empty).

## Verified facts worth NOT re-deriving

- **`memql.NewOfflineEngine` does not exist yet** in this tree (grepped, zero
  hits). Build the offline engine locally as designed above, matching
  `component/memql/lint_parity.go`'s `LintUnifiedTree` construction
  (`New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))` +
  `eng.Init(registry)`), which is a proven DB-free, network-free pattern
  already exercised by `cmd/memqllint`.
- **Module boundaries are fine, no go.mod edits needed.** `cmd/memql-arch` has
  no `go.mod` of its own -- it is part of the ROOT module
  (`github.com/znasllc-io/memql`). The root `go.mod` already `require`s (with
  local `replace`s) `component/architecture`, `component/automations`,
  `component/memql`, `component/database` (which contains the
  `memory-nodes` subpackage), `core`, and `dsl`. `component/architecture` is
  its OWN separate module (confirmed via its `go.mod`), but that only matters
  for which `go test` invocation reaches its tests (see CI section below), not
  for whether `cmd/memql-arch` can import it (it already does, in the
  existing `main.go`).
- **`automation routeRequest` already exists** in `dsl/forge/automations.memql`
  (line 31), as an ordinary event-triggered automation (`@trigger(event=
  "node.created", concept="v1:forge:request", partition="*")`) -- NOT yet the
  Phase-B "before write" body shape from decisions.md D-M (that rewrite is
  Task 14+, gated on epic 3 merging). So `automation:routeRequest` is a valid,
  already-present node the new test can assert against today.
- **`archiveFileOnArtifactArchive -> indexFileOnCreate` is very likely a real
  edge**, confirmed by reading both automations in
  `dsl/library/automations.memql` (lines 197-267):
  - `archiveFileOnArtifactArchive` triggers on `node.updated` of
    `v1:library:artifact`, filter `row.kind=="file" && row.archived==true`,
    and its one step calls mutation `archiveLibraryFile(fileId:
    sourceConceptRef)` -- an UPDATE on `v1:library:file`. Per D-I, an update
    mutation produces BOTH `graph.node.created.<C>` and
    `graph.node.updated.<C>` (MemQL is append-only; an update is a new row
    version too).
  - `indexFileOnCreate` triggers on `node.created` of `v1:library:file`,
    filter `row.status=="stored"`. `archiveLibraryFile`'s update does not set
    `status`, so per the three-valued filter evaluation (D-I / `writeRow` in
    `component/automations/loop_graph.go`), `status` is UNKNOWN on the
    produced row -- and only a `false` decision removes an edge, so the edge
    survives (undecided).
  - This was NOT run/measured yet (no code exists to run), so treat it as
    "very likely, reasoned from source" rather than confirmed -- the brief
    itself says "read one from the measurement", so RUN the real static graph
    once `addAutomations` exists and confirm this pair (or pick whichever
    pair the actual output contains) before hardcoding it into the test.
- **`GraphAutomation` / `GraphEdge` field shapes** (component/automations/
  loop_graph.go lines ~58-75), needed verbatim for the node/edge builders:
  ```go
  type GraphAutomation struct {
      Name, Origin, Trigger, Schedule, Filter string
      Template                                bool
      Stratum                                 int
      Writes, Publishes, Opaque               []string
      Loop                                    *LoopConfig
      Mode                                    *ModeConfig
      Cycle                                   int // index into Cycles, -1 when none
  }
  type GraphEdge struct {
      From, To, Concept, Topic string
      Via                      []string
      Decided                  bool
      Reason                   string
  }
  ```
  `LoopConfig{MaxDepth int, Until string, UntilLambda *ast.LambdaExpr}` and
  `ModeConfig{Kind string, Max int}` are in `component/automations/types.go`
  lines 190-202.
- **`model.WriteJSON`'s sort order** is `(ID, Kind, Name, Parent, Doc,
  sourceKey, attrsKey)` for nodes and `(From, To, Kind, attrsKey)` for edges
  (`component/architecture/model/json.go` `nodeLess`/`edgeLess`). New nodes/
  edges appended in any order are fine as long as `WriteJSON` (called by
  `m.WriteFile` in `main.go`) is what ultimately serializes -- it re-sorts
  unconditionally, so `addAutomations` does not need to sort anything itself
  before returning. (The brief's "Sort as WriteJSON requires" bullet is
  therefore satisfied for free by the existing write path; no extra code
  needed there. Worth double-checking this reasoning when implementing --
  confirm `main.go` still routes through `m.WriteFile`/`WriteJSON` after
  `addAutomations` runs, which it does today since that call happens near the
  end of `main()`.)

## CI filter structure -- resolved an ambiguity in the brief before editing

Read `.github/workflows/ci.yml` in full around the relevant region and
`scripts/dev/gate_inputs_lane_scope_test.go` in full. Key finding:

- The top-level `gates:` paths-filter block spans lines 184-420 (next key
  `proving:` starts at 421). It ALREADY contains both `'dsl/**'` (line ~303)
  and `'**/*.memql'` (line ~184 area), which between them already match
  `dsl/**/automations.memql`, `dsl/**/mutations.memql` and
  `dsl/**/logic.memql` -- i.e., a DSL-only change to any of these files
  already sets the `gates` AND `dsl` outputs true today, which already
  triggers the `go-checks` job (`if:` includes `needs.changes.outputs.dsl`
  and `.gates`) and, inside it, `RUN_GATES` (since `go` stays false on a
  DSL-only change), which runs the `go test (gate inputs)` step that already
  names `./component/architecture/...` explicitly.
  - So the THREE new glob entries the brief asks for
    (`dsl/**/automations.memql` etc.) are **redundant with the existing
    broader globs for CI *triggering* purposes** -- they change nothing about
    whether the lane runs. Add them anyway, exactly as decisions.md D-K
    specifies verbatim ("The staleness gate's CI path filter and gate-input
    rows gain dsl/**/automations.memql, dsl/**/mutations.memql and
    dsl/**/logic.memql") -- it is a deliberate, if partially redundant,
    documentation/audit trail decision, not a bug to "optimize away". Do NOT
    remove or second-guess `'dsl/**'`/`'**/*.memql'` while doing this.
  - Where exactly to insert the three new lines: right by the existing
    `'component/architecture/embedded/**'` line (currently at line 287 in
    this exact revision; brief's cited line number matches) reads most
    naturally, since that is the neighboring entry documenting the same
    staleness gate's inputs.
- **The separate, real mechanism** the brief also asks to touch is
  `scripts/dev/gate_inputs_lane_scope_test.go`'s `gateInputs` table (NOT the
  YAML filter): this is a hand-written Go slice
  (`{path, pkg, gate}` rows, lines 68-144) asserting two DIFFERENT things per
  row: (a) `TestGatesBucketCoversEveryKnownGateInput` -- the named `path` must
  match some glob in the `gates` bucket; (b) `TestGateInputsStepCoversEveryGatePackage`
  -- the named `pkg` must be covered by the `go test (gate inputs)` step's
  package-pattern list. This table is a hand-maintained AUDIT of "which
  non-Go file does which Go test read", deliberately NOT mechanically
  derived (per the file's own doc comment) -- so it is NOT made redundant by
  `dsl/**` already existing in the `gates:` glob list; it is a separate
  assertion that a SPECIFIC file exists AND is covered, used as a trip-wire
  against the bucket being narrowed later. The existing row at line 79 is:
  `{"component/architecture/embedded/topology.model.json", "component/architecture", "TestArchitectureModelIsNotStale"}`
  and at line 131: `{"arch.yaml", "component/architecture", "TestArchitectureModelIsNotStale"}`.
  Adding three new rows for `dsl/**/automations.memql` etc. (each naming an
  ACTUAL existing file under `dsl/`, per the table's own requirement that
  every row's path `os.Stat`s successfully -- `TestGatesBucketCoversEveryKnownGateInput`
  fails on a row naming a nonexistent path) with `pkg: "component/architecture"`
  and `gate: "TestArchitectureModelIsNotStale"` is what actually implements
  "a DSL-only change that removes an edge runs the gate" as a CHECKED
  invariant (today nothing pins that these three specific file kinds matter
  to this specific gate; only the broad `dsl/**`/`**/*.memql` globs happen to
  cover them incidentally, with no test asserting that coverage is
  intentional or would survive a future narrowing).
  - Concretely need one representative EXISTING file per glob kind for the
    table rows (a row's path must exist on disk): e.g.
    `dsl/forge/automations.memql` (confirmed exists, has `routeRequest`),
    a real `mutations.memql` (e.g. `dsl/library/mutations.memql`, confirmed
    exists per an earlier grep), and a real `logic.memql` (need to confirm
    an exact existing path -- `dsl/forge/logic.memql` is referenced in the
    comment at the top of `dsl/forge/automations.memql`
    ("dsl/forge/logic.memql: requestRouteStatus / transitionEventKind") so
    it almost certainly exists; NOT independently `os.Stat`-verified yet in
    this session).
  - The three new rows are NOT literally about "`dsl/**/automations.memql`"
    as a row `path` value (a glob is not a file and would fail the table's
    own existence check) -- they should each name one concrete existing
    `.memql` file of that kind, mirroring how the existing `examples/
    referencepack/dsl/concepts.memql` row names one concrete file to stand in
    for a glob's worth of coverage. Re-read the brief's exact wording once
    more when implementing to confirm this reading is what's intended (it
    reads that way but was not double-checked against a second source).

## Files read this session (none edited)

- `.superpowers/sdd/2026-09-14-dsl-v1-loop-protection/{task-11-brief.md,global-constraints.md,decisions.md}`
- `/home/znas/memql-projects/wt-loops-s6-arch/component/architecture/CLAUDE.md`
- `/home/znas/memql-projects/wt-loops-s6-arch/component/architecture/model/{model.go,ids.go,json.go}`
- `/home/znas/memql-projects/wt-loops-s6-arch/component/architecture/extract/{extract.go,cluster.go,packages.go (partial)}`
- `/home/znas/memql-projects/wt-loops-s6-arch/component/architecture/model_current_test.go` (full, 738 lines)
- `/home/znas/memql-projects/wt-loops-s6-arch/cmd/memql-arch/main.go` (full, 97 lines; only file in that dir today)
- `/home/znas/memql-projects/wt-loops-s6-arch/component/automations/{loop_check.go,loop_graph.go,loader.go (partial),types.go (partial)}`
- `/home/znas/memql-projects/wt-loops-s6-arch/component/memql/{lint_parity.go,unified_loader.go (partial),engine.go (partial),engine_bootstrap.go (partial)}`
- `/home/znas/memql-projects/wt-loops-s6-arch/app/engine.go` (partial, the production `NewLoader` call site)
- `/home/znas/memql-projects/wt-loops-s6-arch/dsl/forge/automations.memql` (partial), `dsl/library/automations.memql` (partial)
- `/home/znas/memql-projects/wt-loops-s6-arch/go.work`, root `go.mod` (grep only), `component/architecture/go.mod`, `component/automations/go.mod` (header only), `component/memql/go.mod` (header only), `dsl/go.mod` (header only), `core/go.mod` (header only)
- `/home/znas/memql-projects/wt-loops-s6-arch/.github/workflows/ci.yml` (large portions: the `changes` job's full filter block structure, `go-checks` and `go-tests` job bodies)
- `/home/znas/memql-projects/wt-loops-s6-arch/scripts/dev/gate_inputs_lane_scope_test.go` (full, 514 lines)

## What remains (in order)

1. Write `component/architecture/model_automations_test.go` and the
   `cmd/memql-arch` unit test for `addAutomations` FIRST (TDD step 1 per the
   brief), expect FAIL (package/function do not exist yet).
2. `model.go`: add `KindAutomation`, `EdgeTriggers` with doc lines.
3. `model/ids.go`: add `AutomationID`.
4. `cmd/memql-arch/automations.go`: implement `addAutomations` per the design
   above, resolving the four open questions inline as they come up.
5. `cmd/memql-arch/main.go`: add `--automations` flag, call `addAutomations(m,
   absRoot)` after `extract.Run`, before the `--reproducible` blank + write.
6. `Makefile`: append `--automations` to the `arch-model` target's memql-arch
   invocation.
7. `.github/workflows/ci.yml`: add the three glob lines near
   `'component/architecture/embedded/**'` in the `gates:` block.
8. `scripts/dev/gate_inputs_lane_scope_test.go`: add three `gateInputs` rows
   (concrete existing file paths, one per `.memql` kind) near lines 79/131.
9. `component/architecture/CLAUDE.md`: update the ":235 pure Go" line to name
   the automation family as the one DSL-derived exception, and the ":63
   (Planned) ERD" line (need to re-read exact current text -- captured above
   as "Model is pure Go... DSL concepts... live in dsl/observability" at line
   235, and the ERD row at line 63: `| **ERD** | (Planned) parse the .memql
   concept tree |` -- decide whether ERD is now PARTIALLY done via automations
   or whether it's a distinct family; automations are NOT an ERD [entities +
   relationships in the data-modeling sense], they're a call/trigger graph, so
   the ERD line likely stays "(Planned)" and a NEW row or a note is what
   actually documents the automation family -- reread brief wording "the
   :63 (Planned) ERD line" once more when implementing, it may mean something
   more specific).
10. Run `go build github.com/znasllc-io/memql/...`.
11. Run `go test -count=1 github.com/znasllc-io/memql/component/architecture/...
    github.com/znasllc-io/memql/cmd/memql-arch/...` (expect the two new tests
    to fail until `make arch-model` regenerates the committed artifact -- the
    brief's step order is write tests -> implement -> `make arch-model` ->
    run tests).
12. Run `make arch-model` (minutes; CHA call graph). Check
    `stat -c %s component/architecture/embedded/topology.model.json` is
    under 70MB (currently need a baseline "before" measurement too -- not yet
    taken this session; get it via `git show HEAD:component/architecture/
    embedded/topology.model.json | wc -c` or `stat` on the pre-change file
    before regenerating, since the working tree copy will be overwritten).
13. Diff node-id sets old vs new (`jq`/Python over both JSON files, or a throwaway
    Go script) -- report automation-node additions, triggers-edge additions,
    and any OTHER (non-automation) id churn from streams 1-3's Go changes
    already merged into this worktree but not yet reflected in the committed
    model (expected per the brief: "the committed model on this branch may
    already be stale against streams 1-3's new Go code -- that is expected").
14. Re-run the two test packages, expect GREEN.
15. `go test -count=1 ./scripts/...` from the worktree root (gate-input lane
    rows).
16. `go test -count=1 .` after `git add` (root gates, since they walk `git
    ls-files`).
17. `gofmt -w` only touched files.
18. Stage by explicit path; commit code and (if verified under 70MB) the
    regenerated model -- per global-constraints.md, splitting into two
    commits (code, then model) is acceptable per the brief's "or split code
    and the regenerated model into two commits".

## Concerns / risks flagged for whoever resumes

- The `loop`/`mode` attr string format is unspecified by any existing
  convention; pick something and move on, it is not gate-tested by name.
- The `routeRequest` / `archiveFileOnArtifactArchive -> indexFileOnCreate`
  choices are reasoned from source reading, not from an actual graph run --
  verify against real `addAutomations` output before hardcoding into the new
  test (the brief explicitly says to read the example from the measurement,
  not to trust a guess).
- Need to confirm `dsl/forge/logic.memql` actually exists (implied but not
  `os.Stat`-verified) before using it as a `gateInputs` row target, and need
  to pick a genuinely appropriate existing `mutations.memql` file too.
- The `component/architecture/CLAUDE.md` ":63 ERD" edit instruction is
  ambiguous as read (see item 9 above) -- resolve by re-reading the brief
  line carefully in context, not by guessing.
- No code was compiled or tested this session -- all Go snippets above are
  designed from reading real signatures but UNVERIFIED by the compiler. Budget
  time for the usual back-and-forth on exact field names / import paths once
  writing starts.

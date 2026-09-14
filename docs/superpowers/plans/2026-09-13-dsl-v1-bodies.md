# DSL v1 body language Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. This plan is deleted in the epic's merge.
>
> **Resuming?** Read `2026-09-13-dsl-v1-bodies-checkpoint.md` beside this file first: where the branch stands, what the peers owe, and what comes next.

**Goal:** One body language for `logic` and `automation`: statements that execute in source order, with forward references refused; the terse header, `step` blocks, the `steps.<id>` spellings and the pinned loop variable retired; one execution model for both keywords, with journal parity; the whole tree migrated by `memqlmigrate --rewrite=bodies` in the same PR (epic memql#5370, tasks #5371-#5374).

**Architecture:** `component/language` gains a body AST (`ast/body.go`), a statement parser that reads `logic` and `automation` natively instead of through the struct-form text rewriter (`parser/v1_body.go`), a scope checker and a body compiler that lowers statements to the executor's step list in source order (`compiler/body_*.go`). The topological sort goes. `component/automations` runs one step sequence for both keywords: the Executor for automations, and the same sequence runner for logic, which replaces the LogicRunner's local short-circuits with epic 2's one in-process evaluator (`memql.EvalExpr` over a `RunScope`). `cmd/memqlmigrate` gains the `bodies` tree rewrite, which carries its own reader for the retired forms so it keeps working for bundle repositories after the engine stops parsing them.

**Tech Stack:** Go 1.26 multi-module workspace; PostgreSQL 16 + TimescaleDB for db-gated tests (throwaway on port 15434); TypeScript for the VS Code extension's regenerated grammar.

**Spec:** `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md` (on PR #5355), section 4 epic 3; decisions D2, D6, D12, D13, D14, D15, D23, D24. Read it before any task. The owner's answers to the four forks D12 left open are recorded under "Decisions" below and are as binding as the record.

## Global Constraints

- One PR, branch `epic/dsl-v1-bodies`, against `main`; closes #5370 #5371 #5372 #5373 #5374 (one `Closes #n` line each -- `Closes #a, #b` links only the first). This plan is deleted in the PR's last commit.
- Base: `epic/dsl-v1-expressions` (memql-10), itself on `epic/dsl-v1-foundations` (memql-b1). Rebase onto each as it moves; open the PR against `main` and enqueue only after both of theirs merge.
- Edition `2026`; language line `1.0` (`parser.Edition`, `parser.LanguageVersion`). The body rewrite registers as `{name: "bodies", edition: "2026", epic: "dsl-v1-bodies", tree: rewriteBodies}` in `cmd/memqlmigrate/rewrites.go` and lives in its own files.
- No backwards-compat shims (repo rule 3). A retired form is refused at parse or load naming `memqlmigrate --rewrite=bodies` and the replacement; the tree is migrated in the same PR. The one deliberate exception is the migrator's own reader of the retired forms, which must outlive the engine's.
- Every refusal names the construct, the position (line:col), the rule id in square brackets at the end, and what to write instead (D24): `... [body_forward_reference]`.
- Verify with the MODULE PATH, never `go test ./...` from the root: `go test github.com/znasllc-io/memql/component/language/...`, `make test`, and for db-gated trees `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable' go test -count=1 ./component/automations/... ./component/memql/... ./test/conformance/...`.
- Run `go test -count=1 .` (root gates: docs, vendor domains, positioning) and `go test -count=1 ./scripts/ci/...` after adding any file, AFTER `git add` (several gates walk `git ls-files`).
- Stage files by explicit path; never `git add -A` / `git add .`. No emojis. `gofmt -w` only files you touched. Never run prettier.
- `GrammarVersion` moves once, in Task 13 (the flip, Step 7), when the surface is final: the grammar-surface corpus does not cover the statement forms until then, so the additive parser of Task 2 leaves it standing. After epic 1 lands, the D25 parity gate (`cmd/memql-lsp/editorparity_test.go`) requires `editors/vscode/package.json` `memql.grammarVersion`, the extension version, a CHANGELOG section and `parser.EditorRelease` to move in the same commit.
- The arch model (`topology.model.json`) goes stale on new packages and files; regenerate it last (`make arch-model`), taking the base branch's side on conflict and regenerating.
- Commit format: `Issue #<N>: <description>` with the attribution trailer.
- Do not touch the running k3d cluster `memql`.
- Shared files with peers: the struct-body clause tables memql-b1 exports from `parser/rewriter.go` (edit the logic/automation rows only); `parser/parser.go` expression levels and `rewriter.go` filter/return/spec/trait rewriting belong to memql-10; `cmd/memqlmigrate/rewrites.go` gets one registry line; `test/conformance/corpus_test.go` is memql-b1's (extend it only by the agreed hook in Task 12).

---

## Decisions this plan makes (from the record and the owner's answers, made concrete)

### The surface

A `logic` is a name, an optional `args { }` block and statements. An `automation` is a trigger (or `@template`), optional `@filter`, an optional `args { }` block, optional `precondition` blocks and the same statements. There is no `body { }` wrapper and no `step` block (owner, 2026-09-13).

```memql
/// Route a submitted request by the submitter's role.
logic requestRouteStatus {
  args {
    submitterRole any
  }
  role := args.submitterRole ?? ""
  return role == "owner" ? "queued" : role == "admin" || role == "writer" ? "needs_approval" : "needs_validation"
}

@trigger(event="node.created", concept="v1:forge:request")
automation routeRequest {
  args {
    id              any
    submitterRole   any
    submitterUserId any
  }
  decide := logic requestRouteStatus(submitterRole: args.submitterRole)
  switch decide {
    case "queued" {
      mutation advanceRequest(requestId: args.id, status: "queued", approvedByUserId: args.submitterUserId)
    }
    default {
      mutation advanceRequest(requestId: args.id, status: decide)
    }
  }
  mutation recordRequestEvent(requestId: args.id, kind: "routed", fromStatus: "submitted", note: "routed by submitter role")
}
```

| Statement | Form | Notes |
|---|---|---|
| bind | `name := <call>` / `name := <expression>` | `name` is its value from here on |
| call | `<kind> <name>(<named args>)` | kind is one of `query mutation logic builtin automation action` |
| if | `if <cond> { } else if <cond> { } else { }` | `else` on the closing brace's line |
| for | `for <x> in <expr> [if <cond>] { }` | the author names `<x>` |
| switch | `switch <expr> { case <lit>[, <lit>] { } default { } }` | case labels are literals, unique |
| parallel | `parallel { branch <label> { } ... } [wait all\|any]` | `wait all` is the default and is not written |
| publish | `publish "<topic>" { <map> }` | automations only; the topic is a string literal |
| return | `return [<expr>]` | ends the body; in an automation, the run |

Trailing clauses close a statement, in this order when several are written (owner, 2026-09-13): `on surface("<name>")` (an `action` call only), `retry(<n>)` (a call statement only, `n >= 1`), `on error continue` (a call, `for` or `parallel` statement). `on error stop` is the default and is never written. One statement per line; an expression may continue onto the next line when the line ends inside an open delimiter or on a binary operator, or when the next line begins with one (no statement begins with an operator).

A construct call is a statement of its own -- the whole right-hand side of `:=`, the whole value of `return`, or a bare call statement. A construct call nested inside an expression is refused, because a side effect that is not a statement is not journaled, previewed or retried. Arguments are named; the bare-argument pun (`logic x(event)`) is retired.

### Names and scope (owner answers 1 and 4)

- A bare name is a statement name, a loop variable, a lambda parameter or a reserved root (`args actor event now config partition trace`). An argument is read `args.x` in both keywords; the G2 bare-args reading (memql#2364) is retired with the rest of the pun.
- A block that runs at most once -- an `if`/`else` branch, a `switch` case -- shares the enclosing scope. A name bound in a branch that did not run is absent. Each branch of ONE if/else chain or ONE switch may bind the same name; whichever runs binds it.
- A loop body and a parallel branch have their own scope: a name bound inside exists only inside. A loop variable or a loop-body name may not shadow a name or a root of an enclosing scope.
- A name is bound once per scope, apart from the sibling-branch rule above. Reading a name before the statement that binds it is refused, naming both lines. Reading a name bound in another branch of the same chain is refused (that branch cannot have run).
- `steps.<id>...` is refused. `x.result` is ordinary member access after the migration (a logic may return a map with a `result` key); the rewrite, not the parser, retires the step-result spelling.
- `return` may appear anywhere except inside a parallel branch. In a logic the last top-level statement must be a `return`.
- A logic may call `query`, `mutation`, `logic` and `builtin`; `publish`, `automation` and `action` are refused in a logic at load, naming the automation form (D14).

### What a statement is at run time

The body compiler emits the executor's step list in SOURCE ORDER; there is no topological sort. Every emitted step carries `binds` (the statement's name, when it has one) separately from `id` (unique within its list, stable across edits elsewhere):

| Statement | Step emitted |
|---|---|
| `x := query q(...)`, `mutation`, `logic`, `builtin` | `type: "function"`, `function: {name, kind, args}`, `binds: "x"` |
| `x := automation a(...)` | `type: "automation"`, `automation: {name, args}` |
| `x := action a(...) [on surface("s")]` | `type: "action"`, `action: {ref, args, surface}` |
| `x := <expression>` | `type: "expression"`, `expression: "<canonical v1 source>"` |
| `if` / `else if` / `else` | FLATTENED: each statement inside is emitted with `condition` = the conjunction of the enclosing branch conditions (`!(earlier) && this` for later branches), exactly as today's if-flattening |
| `switch s { case "a", "b" {} default {} }` | FLATTENED to conditions `s == "a" \|\| s == "b"` and `!(...)` for default -- typed equality (`1 == "1"` is false) |
| `for x in src if f { }` | `type: "forEach"`, `forEach: {source, filter, as: "x", do: [...]}` |
| `parallel { branch a { } }` | `type: "parallel"`, `parallel: {wait, branches: [{id: "a", type: "block", block: {steps: [...]}}]}` |
| `publish "t" { ... }` | `type: "event"`, `event: {topic: "t", payload: {...value leaves...}}` |
| `return v` | `type: "return"`, `return: {value: "<canonical v1 source>"}` |
| `return <call>` | the call's own step (as for `x := <call>`, without `binds`) carrying `returns: true`: the sequence ends after it with the call's value as the return value, so the call is journaled, previewed and retried like any call |

Expression leaves follow epic 2's encoding: fields that are always an expression (`condition`, `forEach.source`, `forEach.filter`, `expression`, `return.value`) carry canonical v1 source (`ast.FormatExpr`); a value inside an args or payload map is `{"$expr": "<source>"}`.

Step ids: a named statement's id is its name (`x`; `x#2` for a sibling-branch rebinding). An unnamed statement takes its callee's name (`createArtifact`, `createArtifact#2`), `for_<var>`, `parallel`, `publish` or `return`, numbered in source order. Named statements claim their ids first.

Values (the row projection is D12's and ours, agreed with memql-10): a `query` result is a list of `memql.ExprRow`; one row reads `row.id` / `row.concept` / `row.type` / `row.createdAt` / `row.createdBy` / `row.provenance` as intrinsics and every other name from the payload (`rows.first().email`). A `mutation` value is the written row. A `logic` value is its return value. A `builtin` value is its result, unwrapped. An `action` value is what its capability produced -- a capability-script envelope unwraps to its `result`, and the rest (`ok`, `changed`, `ref`, `capability`, `resultFingerprint`, `surface`) moves to the step's metadata, which is what retires `.result.result.result` climbs. An `automation` call's value is the sub-run's returned value. A row crossing into a call argument or a published payload marshals to today's node-map shape, so no consumer sees a changed wire value.

Runtime rules: a skipped statement binds nothing. `retry(n)` retries a failed call up to n more times whatever its `on error`; `on error continue` then records the failure, leaves the name absent and continues. `return` ends the sequence; inside a `for` it ends the enclosing body, not only the iteration. A `return` in an automation records its value on the run's `outcome.returned`.

### One execution model and journal parity (D14)

- Logic bodies compile at LOAD (the function loader), never at call. The engine dispatches every logic call -- one statement or many -- to the same sequence runner the Executor uses. `fn.LogicSteps`, the call-time compile in `LogicRunner.compileBodyToAutomation`, and every local short-circuit in `logic_runner.go`, `logic_logical.go` and `logic_arithmetic.go` are deleted; an expression statement evaluates through `memql.EvalExpr`.
- A logic invoked inside a run (a statement of an automation, or of a logic already inside a run) journals its statements as steps of THAT run, keyed `<caller key>/<statement id>`. Resume never resumes into a nested key; it re-runs the calling statement, as today.
- A logic invoked directly is journaled as its own run only when it writes: the runner watches for the first graph write through a context-carried write observer, opens a `logic:<name>` run at that moment, back-fills the statements already executed, and journals the rest. A read-only call leaves no run row. The journal's own writes run without the observer.

### The migration (`--rewrite=bodies`, D6)

- **Order.** For every body, compute T (today's order: Kahn over the references the legacy compiler could see -- dotted names whose first segment is a step id, `first(x)` / `last(x)`; never `steps.`-rooted, never an undotted bare name -- FIFO queue, co-released consumers in source order) and D (every reference, as the new scope checker sees it). If T respects D, emit T. If T reads a name before it is bound (today's engine read nothing there), emit the source order when it respects D, else the D-topological order nearest the source. Every statement whose position differs from the source, and every statement that today ran before a name it reads, gets one `// memqlmigrate:` comment naming the move and why. Where today's order among side-effecting statements was not fixed (co-released consumers; the legacy consumer list is built from Go map iteration), the later statement's comment says so.
- **Kinds.** Every call gets its kind from a declaration index over the tree being rewritten plus the engine's embedded tree; a name that resolves to no construct, or to two kinds, is reported and the file is left unchanged.
- **References.** `steps.x.result[.f]`, `x.result[.f]` (x a step) and the bare-argument pun become `x[.f]`; action references drop their three `.result` climbs; `.payload.<f>` on a row value (a loop variable over rows, `.first()` / `.last()` / an element of a rows value) becomes `.<f>`; `First() Last() Empty() Nodes()` become their lowercase methods; in an automation, a bare args field and `event.payload.<f>` become `args.<f>` (declaring `<f> any` when missing).
- **Statements.** `step n { <call> }` becomes `n := <call>`; `step n { if c { <call> } }` becomes `if c { n := <call> }`; `step n { forEach x in s where f { ... } }` / `for x := range s if f` become `for x in s if f { ... }` (the step name is dropped: a `for` binds nothing); `switch` keeps its shape; `parallel { wait: "all", failFast: ..., branches: [...] }` becomes `parallel { branch ... }`; every legacy `action` spelling becomes `action <name>(...) [on surface(...)]`; logic `x := if c { <call> }` becomes `if c { x := <call> }`, `x := retry(n) <call>` becomes `x := <call> retry(n)`; `publishEvent(topic: "t", payload: p)` becomes `publish "t" p`; object-literal shorthand entries (`{ args.event.payload.id }`) expand to `id: ...`.
- **Terse automations** expand to a one-statement automation `logic <l>(event: event)`. A logic that publishes, reached only from one automation, is INLINED into that automation (its `args.event.payload.<f>` reads become `args.<f>`, the fields are declared, its cross-domain `use` imports are copied, its doc comment moves in as a `//` block) and the logic is deleted. Any other publishing logic is reported for a hand edit.
- **Declarations and triggers.** `mutate <Concept> <name> {` becomes `mutation <Concept> <name> {` (D13) in every file; `partition=` is removed from `@trigger`; `@schedule(cron=X)` becomes `@trigger(schedule=X)` (D15).
- **Idempotent**, and refuses rather than guesses: a file it cannot rewrite exactly is returned unchanged with a message naming the construct and the reason.

### Behaviour changes the PR body must call out

- `dsl/workbench/automations.memql` `releaseWorkspaceOnRunTerminal`: today `teardown` ran before the per-workspace release loop (its `steps.terminal` reference was invisible to the sort). The migration keeps that order and says so in a comment; the order is now visible and reviewable.
- `examples/deploypack/dsl/logic.memql` `driveDeploymentInProgress` and `recordReconciledState`: today `observed`/`reconciled`/`obsProbe` consumers ran BEFORE the statements they read (undotted references were invisible to the sort), so both always answered empty. Source order fixes them.
- The seven logic bodies that published (`dsl/identity/logic.memql` x4, `dsl/safety/logic.memql` x2, `dsl/data/logic.memql` x1) move into their automations; the logic constructs are deleted.
- `switch` compares with typed equality; every switch in the tree compares strings, so no row changes.
- `x := mutation m(...)` now names the written row, and an action statement's value is its capability's result.

---

## Sequencing and hand-offs

| Task | Needs | Can start |
|---|---|---|
| 1 Body AST | epic 2 Task 1 (committed, `e1ecd6d6c`) | now |
| 2 Statement parser | epic 2 Task 2 (`parseV1Expression`) | on memql-10's SHA |
| 3 Scope checker, 4 Body compiler | Task 2 | after 2 |
| 5-8 The `bodies` rewrite | nothing but today's tree | now |
| 9 Runtime, 10 Logic model and journal, 11 Dry run and resume | epic 2 Task 7 (`EvalExpr`, `RunScope`, the compiler encoding) | on memql-10's SHA |
| 12 Corpus | Task 2 for load and refusal cells; Task 10 for logic `evaluate` cells; memql-b1's scaffold for `cells/logic`, `cells/automation`; Task 13 for the scenarios, which run the SHIPPED automations | in slices, as each arrives |
| 13 The flip | epic 2 Task 9 (tree on v1 expressions), Tasks 2-11 | last |
| 14 Gates that read bodies, 15 Docs, 16 Ship | Task 13 | last |

When a peer's SHA arrives: `git -C /home/znas/memql-projects/epic-dsl-v1-bodies rebase <sha>`, rerun `go test github.com/znasllc-io/memql/component/language/... github.com/znasllc-io/memql/cmd/memqlmigrate/...`, then continue.

## File Structure

| Path | Responsibility | Task |
|---|---|---|
| `component/language/ast/body.go` (new) | `Body`, the statement nodes, `ConstructCall`, `StatementMods`, `WalkBody` | 1 |
| `component/language/parser/v1_body.go` (new) | native `logic` / `automation` declarations, the statement parser, the retired-form refusals | 2 |
| `component/language/parser/v1_body_refusals.go` (new) | the refusal codes and messages, `BodyStatementForms()` | 2 |
| `component/language/compiler/body_scope.go` (new) | scope and name resolution; `CheckBody` | 3 |
| `component/language/compiler/body_compile.go` (new) | `CompileBody`: statements to steps, ids, flattening | 4 |
| `cmd/memqlmigrate/bodies.go` (new) | the tree rewrite entry, the registry line, per-file orchestration | 5 |
| `cmd/memqlmigrate/bodies_read.go` (new) | the reader of the retired body forms, comment-preserving | 5 |
| `cmd/memqlmigrate/bodies_refs.go` (new) | the token-level reference rewrites | 6 |
| `cmd/memqlmigrate/bodies_order.go` (new) | the legacy order T, the reference graph D, the order policy, the move comments | 7 |
| `cmd/memqlmigrate/bodies_index.go` (new) | the declaration index over the tree plus the embedded tree | 5 |
| `cmd/memqlmigrate/bodies_inline.go` (new) | terse expansion and publishing-logic inlining; `mutate`, `partition`, `@schedule` | 8 |
| `component/automations/run_scope.go` (epic 2's) | statement names via `Binds`, child scopes, the row projection | 9 |
| `component/automations/sequence.go` (new) | `runSequence`: the one step loop (conditions, binds, retry, on error, return) | 9 |
| `component/automations/executor.go`, `steps/*.go`, `types.go` | new step types, `Binds`, action value, `block` (9); `switch` deleted (13) | 9, 13 |
| `component/automations/logic_runner.go` | a v1 body runs on `runSequence` (10); the short-circuits are deleted with the legacy tree (13) | 10, 13 |
| `component/automations/logic_logical.go`, `logic_arithmetic.go` | DELETED | 13 |
| `component/memql/function_loader.go`, `engine.go`, `function_types.go` | compile logic bodies at load; one dispatch | 10 |
| `core/common/write_observer.go` (new), `component/memql/executor_mutation.go` | the write observer seam | 10 |
| `component/automations/steps/sandbox_registry.go`, `resume.go` | dry run and resume over the new step types | 11 |
| `test/conformance/2026/statements/**`, `test/conformance/statements_gate_test.go` (new) | statement cells and their completeness gate | 12 |
| `test/conformance/2026/scenarios/**`, `test/conformance/scenarios_db_test.go` (new) | the four scenario suites, dry run and resume cases | 12 |
| `dsl/**`, `examples/**/dsl/**`, `deploy/fleet/dsl/**`, `test/**/*.memql`, Go fixtures | the migrated tree | 13 |
| `component/emailrules/generate.go`, `dsl/authoring/**` | generators and authoring material write v1 | 13 |
| `component/language/dslspec/*`, `component/memql/sense/*`, `cmd/memql-lsp/internal/grammar/*`, `editors/vscode/*` | the statement vocabulary; `mutation` keyword | 13 |
| legacy: `parser/rewriter.go` logic/automation/terse stages, `parser.go` `parseGoStyleAutomationBody` and its helpers, `ast` legacy statement nodes, `compiler/automation_generator.go` `topoSortSteps` and reference extraction, `automations/args_resolution.go` G2 | DELETED | 13 |
| `test/dslconformance/*`, `component/memql/dslgate/*`, `component/memql/callgraph/*`, `sense/runnable.go`, `keyword_slices.go`, `sdk/gen/gen.go` | gates and slicers that read bodies | 14 |
| `docs/public/language/memql.md`, `authoring-rules.md`, root `CLAUDE.md`, `component/language/CLAUDE.md` | the body language documented | 15 |

---

## Task 1: The body AST (#5371)

**Files:**
- Create: `component/language/ast/body.go`, `component/language/ast/body_test.go`

**Interfaces:**
- Consumes: epic 2's `ast.ExpressionNode`, `ast.Span`, `ast.NamedArg`, `ast.MapExpr`, `ast.WalkV1`, `ast.FormatExpr`.
- Produces:

```go
// Body is the statements of a logic or an automation, in source order.
type Body struct {
	Statements []BodyStatement
	Span       Span
}

// BodyStatement is one statement. The concrete types are below; nothing
// outside this file implements it.
type BodyStatement interface {
	Node
	bodyStatement()
	StatementSpan() Span
}

// ConstructCall is `<kind> <name>(<named args>) [on surface("s")]`.
type ConstructCall struct {
	Kind    string     // query | mutation | logic | builtin | automation | action
	Name    string
	Args    []NamedArg
	Surface string     // action only; "" when not written
	Span    Span
}

// StatementMods are the trailing clauses. Retry 0 means none; OnError "" means stop.
type StatementMods struct {
	Retry   int
	OnError string // "" | "continue"
}

type AssignStatement struct {
	Name     string
	NameSpan Span
	Call     *ConstructCall // exactly one of Call and Value is set
	Value    ExpressionNode
	Mods     StatementMods
	Span     Span
}
type CallStatement struct {
	Call *ConstructCall
	Mods StatementMods
	Span Span
}
type IfBranch struct {
	Cond ExpressionNode // nil for the final else
	Body []BodyStatement
	Span Span
}
type IfStatement struct {
	Branches []IfBranch // Branches[0] is the `if`; a nil Cond is the `else`
	Span     Span
}
type ForStatement struct {
	Var     string
	VarSpan Span
	Source  ExpressionNode
	Filter  ExpressionNode // nil when no `if`
	Body    []BodyStatement
	Mods    StatementMods  // OnError only
	Span    Span
}
type CaseArm struct {
	Labels  []ExpressionNode // literals; empty for default
	Default bool
	Body    []BodyStatement
	Span    Span
}
type SwitchStatement struct {
	Subject ExpressionNode
	Cases   []CaseArm
	Span    Span
}
type ParallelBranch struct {
	Label string
	Body  []BodyStatement
	Span  Span
}
type ParallelStatement struct {
	Branches []ParallelBranch
	Wait     string // "all" | "any"
	Mods     StatementMods // OnError only
	Span     Span
}
type PublishStatement struct {
	Topic   string
	Payload *MapExpr
	Span    Span
}
type ReturnStatement struct {
	Call  *ConstructCall // at most one of Call and Value; neither for a bare return
	Value ExpressionNode
	Mods  StatementMods  // Retry only, with a Call
	Span  Span
}

// WalkBody visits every statement depth-first in source order; returning
// false from visit skips that statement's children.
func WalkBody(stmts []BodyStatement, visit func(BodyStatement) bool)

// StatementExpressions returns every expression a statement itself holds
// (conditions, sources, values, call arguments), not those of nested statements.
func StatementExpressions(s BodyStatement) []ExpressionNode
```

- [ ] **Step 1: Write the failing tests** (`body_test.go`): `TestWalkBodyVisitsInSourceOrder` builds `x := a; if c { y := query q(); for i in y { mutation m(v: i) } } else { return x }` by hand and asserts the visit order `assign x, if, assign y, for i, call m, return`; `TestWalkBodySkipsChildrenOnFalse`; `TestStatementExpressionsCoversEveryField` (an if branch's Cond, a for's Source and Filter, a call's argument values, a publish payload, a return value, a switch subject and labels) -- each statement type appears once, so adding a type without extending the helper fails the test.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/language/ast/ -run 'TestWalkBody|TestStatementExpressions'` -- expect a compile failure.
- [ ] **Step 3: Implement** `body.go`: the types above, `node()` / `bodyStatement()` / `StatementSpan()` on each, `WalkBody`, `StatementExpressions`. The package doc paragraph names the record and says the legacy `AssignStmt`/`IfStmt`/`ForRangeStmt`/`SwitchStmt`/`ReturnStmt` go in Task 13.
- [ ] **Step 4: Run** the tests -- PASS; run `go test github.com/znasllc-io/memql/component/language/...`.
- [ ] **Step 5: Commit** `Issue #5371: the body AST: statements, construct calls and trailing clauses`.

## Task 2: The statement parser and the retired forms refused (#5371)

**Files:**
- Create: `component/language/parser/v1_body.go`, `component/language/parser/v1_body_refusals.go`, `component/language/parser/v1_body_test.go`, `component/language/parser/v1_body_refusals_test.go`
- Modify: `component/language/parser/parser.go` (`parseDefinition` dispatches `logic` and `automation` to the native parser, per construct, see Step 5; `processAutomationAttributes` refuses `partition=` and `@schedule` on a native automation), `component/language/ast/ast.go` (`AutomationDef.Body *Body`; `Steps` stays until Task 13), `component/language/parser/rewriter.go` (the exported clause-table rows for a native logic and automation are `args` and `args precondition`; the three legacy stages stay until Task 13)

**Interfaces:**
- Consumes: Task 1; epic 2's `(p *Parser) parseV1Expression() (ast.ExpressionNode, error)` (mid-stream; stops at a token that cannot continue an expression) and `ast.FormatExpr`.
- Produces:

```go
// parseV1LogicOrAutomation parses `logic NAME { args {...} statements }` and
// `automation NAME { args {...} precondition ... statements }` natively. The
// result is a *FunctionDef whose Body is an *AutomationDef with Body set and
// Steps empty; attributes and doc comments attach as today.
func (p *Parser) parseV1LogicOrAutomation(kind string, attrs []*Attribute) (*FunctionDef, error)

// parseV1Statements reads statements until the `}` closing the enclosing
// block (not consumed). kind is "logic" or "automation"; it decides nothing
// here -- the construct rules are load checks (Task 3) -- but is carried for
// messages.
func (p *Parser) parseV1Statements(kind, construct string) ([]ast.BodyStatement, error)

// BodyRefusal is a parse-time refusal with its stable code. It wraps the
// positioned *ParseError (as epic 2's RetiredFormError does), so every consumer
// that reads a parse error's position reads this one's; the native parser also
// prefixes every error with the construct (`logic l: ...`, D24).
type BodyRefusal struct {
	Code  string
	Parse *ParseError
}
func (e *BodyRefusal) Error() string // Parse.Error() + " [<Code>]"

// BodyStatementForms lists every statement form and trailing clause the
// parser accepts, in the words the corpus directories use: assign, call, if,
// else, for, switch, parallel, publish, return, retry, onError, onSurface.
func BodyStatementForms() []string
```

Statement termination is by line: two statements on one line refuse; `parseV1Expression` decides where an expression ends, and a bare `return` is a `return` whose next token is on a later line or is `}`.

The refusals this task pins (the message prefixes are the contract):

| Code | Trigger | Message |
|---|---|---|
| `body_step_retired` | `step <n> {` | `` `step <n> { ... }` is retired in edition 2026: write the step's call as a statement, `<n> := <call>` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_block_retired` | `body {` in a logic | `` `body { }` is retired in edition 2026: a logic's statements follow its args block directly (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_terse_retired` | `automation N @trigger(...) => logic L` | `` the terse `automation N @trigger(...) => logic L` form is retired in edition 2026: write @trigger(...) above `automation N { logic L(event: event) }` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_steps_reference_retired` | an expression rooted at `steps` | `` `steps.<id>...` is retired in edition 2026: a statement's name is its value, so write `<id>` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_foreach_retired` | `forEach x in s [where f]` | `` `forEach` is retired in edition 2026: write `for x in <source> if <cond> { }` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_for_range_retired` | `for x := range s` | `` `for x := range <source>` is retired in edition 2026: write `for x in <source>` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_conditional_assign_retired` | `x := if c { ... }` | `` `x := if <cond> { <call> }` is retired in edition 2026: write `if <cond> { x := <call> }` -- a name bound in an if branch is readable after it (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_publish_event_retired` | `publishEvent(` | `` `publishEvent(...)` is retired in edition 2026: write `publish "<topic>" { ... }` in an automation (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_call_kind_missing` | `name(...)` or `name { ... }` at statement position | `` `<name>(...)` names no construct kind: write `query`, `mutation`, `logic`, `builtin`, `automation` or `action` before it (memqlmigrate --rewrite=bodies adds it) `` |
| `body_positional_argument` | `kind name(x)` with an unnamed argument | `` `<kind> <name>(<x>)` passes <x> without a name: write `<x>: <value>`; the bare-argument pun is retired (memqlmigrate --rewrite=bodies rewrites it) `` |
| `body_call_in_expression` | a construct `CallExpr` nested in an expression | `` a construct call is a statement of its own: bind it, `<n> := <kind> <name>(...)`, and read <n> here `` |
| `body_one_statement_per_line` | a second statement starts on the same line | `` one statement per line: `<text>` starts a second statement on line <n> `` |
| `body_else_placement` | `else` on a new line | `` `else` follows the closing brace on the same line: `} else {` `` |
| `body_retry_placement` | `retry(n)` on a non-call | `` `retry(n)` applies to a construct call statement `` |
| `body_on_error_placement` | `on error` on if/switch/assign-expression/publish/return | `` `on error continue` applies to a call, a `for` or a `parallel` statement `` |
| `body_surface_placement` | `on surface(...)` not on an `action` call | `` `on surface(...)` applies to an `action` call `` |
| `body_clause_order` | trailing clauses out of the canonical order, or one written twice; `wait` after `on error` | `` trailing clauses are written once each, in the order `on surface(...)`, `retry(n)`, `on error continue` `` |
| `body_default_clause` | `on error stop` or `wait all` written (D24: one form per operation) | `` `on error stop` is the default: delete it `` |
| `body_case_label` | a non-literal or repeated case label | `` a case label is a literal written once: `<label>` `` |
| `body_wait_value` | `wait` other than all/any | `` `wait` takes `all` or `any` `` |
| `body_empty` | an automation with no statement | `` an automation has at least one statement `` |
| `trigger_partition_retired` | `partition=` in `@trigger` | `` `partition=` on @trigger is retired in edition 2026: delete it (memqlmigrate --rewrite=bodies rewrites it) `` |
| `trigger_schedule_synonym_retired` | `@schedule(cron=X)` | `` `@schedule(cron=...)` is retired in edition 2026: write `@trigger(schedule=...)` (memqlmigrate --rewrite=bodies rewrites it) `` |
| `mutate_keyword_retired` | `mutate <Concept> <name> {` | `` the declaration keyword `mutate` is retired in edition 2026: write `mutation <Concept> <name> {` (memqlmigrate --rewrite=bodies rewrites it) `` |

`mutate_keyword_retired` and the `mutation` declaration keyword land in Task 13 with the tree migration, because every `.memql` file with a mutation changes in that commit; this task only defines the refusal.

- [ ] **Step 1: Write the failing parse tests** (`v1_body_test.go`), each parsing a whole file through `ParseFile` and asserting the `*ast.Body` shape (statement types, names, `ast.FormatExpr` of each expression, spans): a logic with `args` and three statements; an automation with `@trigger`, `args`, one `precondition` block and statements; every row of the statement table; `else if` chains; a `for` with and without `if`; `switch` with a two-label case and `default`; `parallel` with two branches and `wait any`; every trailing-clause combination in canonical order (any other order is a `body_clause_order` refusal); `publish "t" { a: 1, b: args.x }`; bare `return` followed by `}`; a multi-line call whose arguments carry `//` comments; an expression continued by a trailing `&&` and by a leading `||`; a doc comment and `@actor` on a logic.
- [ ] **Step 2: Write the failing refusal tests** (`v1_body_refusals_test.go`): one case per table row, asserting code, message prefix and line:col; plus `TestBodyStatementFormsMatchTheParser` (every form the table lists is accepted by at least one case in `v1_body_test.go`, read from a table the test file exports -- adding a form without a parse test fails).
- [ ] **Step 3: Run** `go test github.com/znasllc-io/memql/component/language/parser/ -run 'TestV1Body'` -- expect failures.
- [ ] **Step 4: Implement.** `parseDefinition` routes `logic` / `automation` keywords to `parseV1LogicOrAutomation`; the `args { }` block reuses `parseArgsBlock`; `precondition NAME { ... }` blocks before the first statement are skipped by brace matching (the automations loader still extracts them from source, unchanged). The native path is reached before the struct-form rewriter's logic/automation stages run.
- [ ] **Step 5: The transitional dispatch.** Until Task 13 migrates the tree, a construct in a retired form must keep loading, so the dispatch is per construct: a `logic` whose braces open with `body {`, an `automation` holding a `step <name> {` block, and the terse header go to the legacy rewriter path; every other `logic` / `automation` goes to the native parser. The detector is exact for the tree as it stands (every legacy logic has `body {`, every legacy automation has a `step` block or is terse), and it is what lets Tasks 9-12 test v1 bodies through the real loaders before the flip. Task 13 deletes the legacy branch and turns each detector hit into its refusal. Add `TestTransitionalDispatchIsExact`: every logic and automation in `dsl/`, `examples/`, `deploy/fleet/dsl/` takes the legacy path today, and every case in `v1_body_test.go` takes the native one.
- [ ] **Step 6: Run** the new tests and `go test github.com/znasllc-io/memql/component/language/...` -- PASS, with every legacy test unchanged.
- [ ] **Step 7: Commit** `Issue #5371: the statement grammar of D12, parsed natively, with the retired forms refused`.

## Task 3: Scope and the construct rules (#5371, #5372)

**Files:**
- Create: `component/language/compiler/body_scope.go`, `component/language/compiler/body_scope_test.go`

**Interfaces:**
- Consumes: Task 1. (Built before Task 2 on hand-made bodies, so it does not wait for the parser; Task 2's parse tests add source-level cases through `CheckBody`.)
- Produces:

```go
// BodyProblemCodes lists every code CheckBody can return (the corpus gate reads it).
func BodyProblemCodes() []string

// BodyProblem is a load-time refusal of a body. Error() ends with " [<Code>]".
type BodyProblem struct {
	Code      string
	Message   string
	Construct string // "logic requestRouteStatus"
	Line, Col int
}

// CheckBody enforces the scope rules of the plan's Decisions and the
// construct rules of D14. kind is "logic" or "automation"; args are the
// declared args field names. Returns every problem, in source order.
func CheckBody(kind, name string, args []string, body *ast.Body) []BodyProblem

// BodyNames returns the statement names a body binds and, for each, the line
// that binds it -- the scope checker's own table, exported for Sense.
func BodyNames(body *ast.Body) map[string][]int
```

| Code | Rule | Message |
|---|---|---|
| `body_forward_reference` | a read of a name bound later | `` `<stmt>` reads `<x>`, which is bound on line <m>, after it: move line <m> above line <n> `` |
| `body_unknown_name` | a bare name that is nothing in scope | `` `<x>` is not a statement name, a loop variable or a root here `` + one of: `; write args.<x>` (x is an args field) / `; <x> is bound inside the loop on line <m> and exists only inside it` / `; <x> is bound in another branch of this if on line <m>, which cannot have run` |
| `body_duplicate_name` | a second binding in one scope | `` `<x>` is already bound on line <m>: a name is bound once, apart from the branches of one if/else chain or switch `` |
| `body_shadowed_name` | a loop variable or loop-body name shadowing | `` `<x>` on line <n> shadows `<x>` bound on line <m>: pick another name `` |
| `body_reserved_name` | binding `args actor event now config partition trace steps` | `` `<x>` is a reserved root and cannot name a statement or a loop variable `` |
| `body_publish_in_logic` | `publish` in a logic | `` a logic may not publish: move `publish "<topic>"` into the automation that calls <name>, or declare <name> as an automation `` |
| `body_call_not_in_logic` | `automation`/`action` call in a logic | `` a logic calls queries, mutations, builtins and other logic: `<kind> <callee>(...)` belongs in an automation `` |
| `body_logic_return` | a logic whose last top-level statement is not `return` | `` a logic ends with `return <value>` `` |
| `body_return_in_parallel` | `return` in a parallel branch | `` a parallel branch cannot return: bind a name and return after the parallel `` |

Name resolution walks each expression with `ast.WalkV1`: an `IdentExpr` that is not a lambda parameter in scope and not the callee of a function/method call is a read. Lambda parameters are collected from `ast.LambdaExpr.Params` while descending.

- [ ] **Step 1: Failing tests**, one per row plus the positives: a name bound in an `if` and read after it; the same name bound in `if` and `else` and read after; a name bound in a `switch` case and read after; a loop variable read in its body and refused after it; a parallel-branch name refused after the parallel; a lambda parameter shadowing nothing; `args.x` read in both keywords; `event` read in an automation; a logic whose `return` is inside an `if` but whose last statement is also a `return`.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/language/compiler/ -run TestCheckBody` -- fail.
- [ ] **Step 3: Implement** a scope stack (a map per scope, with the line that bound each name and, for once-block branches, the chain id and branch index the binding came from).
- [ ] **Step 4: Run** -- pass.
- [ ] **Step 5: Commit** `Issue #5371: names resolve in source order; forward references and the D14 construct rules are refused at load`.

## Task 4: The body compiler: statements to steps, in source order (#5371)

**Files:**
- Create: `component/language/compiler/body_compile.go`, `component/language/compiler/body_compile_test.go`
- Modify: `component/language/compiler/automation_generator.go` (`compileAutomation`: when `automation.Body != nil`, steps come from `CompileBody` and no sort runs; the `_return` output field is not written for a v1 body), `component/language/compiler/compiler.go` (logic: `CompileLogicBody` result carried on the output)

**Interfaces:**
- Consumes: Tasks 1-3, `ast.FormatExpr`.
- Produces:

```go
// CompileBody checks (CheckBody) and lowers a body to the executor's step
// list, in source order. Problems refuse the whole body.
func CompileBody(kind, name string, args []string, body *ast.Body) ([]map[string]any, []BodyProblem)
```

The JSON shapes are the table under "What a statement is at run time". `binds` is written only for a named statement. `condition` for a flattened branch is `ast.FormatExpr` of the conjunction, built as plain `ast.BinaryExpr{Op: "&&"}` / `ast.UnaryExpr{Op: "!"}` nodes: the printer parenthesises exactly where the grammar would otherwise read a child differently, so `(a || b) && c` and `!(x == 1)` come out right without forced parentheses; an else branch is `!c1 && !c2` over the earlier branch conditions. A switch case is `subject == l1 || subject == l2` (labels are unique, so a case needs no negation of the others); the default is the negation of every case, wherever it is written. `retryCount` and `onError` are written when set. A `function` step carries `kind`. A parallel writes `failFast: true` (the statement form has no spelling for the legacy `false`).

The integration into `compileAutomation` / the logic compile (the `Body != nil` branch, `"expressions": "v1"` on the output, no `_return`) lands with Task 2, which adds `AutomationDef.Body`; this task is `CompileBody` itself (committed before Task 2, on hand-made bodies).

- [ ] **Step 1: Failing tests** (`body_compile_test.go`), golden JSON per case (compare with `json.Marshal` of expected maps): every statement type; nested `if` inside `for` (the loop's `do` carries the inner conditions, the loop step carries only its own); `else if` / `else` conditions; switch with two labels and default; ids for two unnamed calls to one callee (`createArtifact`, `createArtifact#2`), a named statement claiming its name before an earlier unnamed call to the same callee, a rebinding in `else` (`quote`, `quote#2`, both `binds: "quote"`); `retry(2) on error continue` on a call; `publish` payload values as `$expr` leaves and literals as literals; `TestCompileBodyKeepsSourceOrder` -- a body whose legacy sort would have moved a statement keeps source order.
- [ ] **Step 2: Run** -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/component/language/...` -- pass.
- [ ] **Step 5: Commit** `Issue #5371: bodies compile to the step list in source order; the topological sort is not consulted`.

## Task 5: The `bodies` rewrite: the declaration index, the reader, statement emission (#5373)

**Files:**
- Create: `cmd/memqlmigrate/bodies.go`, `cmd/memqlmigrate/bodies_read.go`, `cmd/memqlmigrate/bodies_index.go`, `cmd/memqlmigrate/bodies_test.go`, `cmd/memqlmigrate/testdata/bodies/<case>/{in,out}/**`
- Modify: `cmd/memqlmigrate/rewrites.go` (one registry entry; delete the `terse-automation` entry, whose output is a form this epic retires, and `langparser.RewriteLonghandSingleStepAutomation` with it in Task 13), `cmd/memqlmigrate/main.go` (package doc lists the rewrite)

**Interfaces:**
- Consumes: nothing from the engine's parser except `langparser.BlankComments` and `langparser.QuoteString`; the embedded tree via `memqldsl` for the index.
- Produces:

```go
func rewriteBodies(root string, files map[string][]byte) (map[string][]byte, error)

// declIndex maps a construct name to its kinds, from every declaration in the
// tree being rewritten plus the engine's embedded tree.
type declIndex struct{ kinds map[string][]string } // "createArtifact" -> ["mutation"]
func buildDeclIndex(files map[string][]byte) (*declIndex, error)
func (ix *declIndex) kindOf(name string) (string, error) // refuses unknown and ambiguous

// legacyBody is one retired-form body as the reader saw it.
type legacyBody struct {
	Construct  string        // "automation" | "logic"
	Name       string
	File       string
	HeaderLine int
	Items      []legacyItem  // steps (automation) or statements (logic), source order
	Trailing   []string      // comment lines after the last item
}
type legacyItem struct {
	Leading []string        // comment and blank lines above it, verbatim
	Name    string          // step or statement name; "" for a bare call / if / for
	Form    string          // call | ifCall | forEach | forRange | switch | parallel | action | expr | condAssign | retryAssign | ifBlock | return | publishEvent
	Text    string          // the item's source, comments blanked for scanning, raw for emission
	Line    int
	// form-specific fields, filled by the reader:
	Call     *legacyCall
	Cond     string
	Var      string
	Source   string
	Filter   string
	Cases    []legacyCase
	Branches []legacyItem
	Body     []legacyItem
	Value    string
}
type legacyCall struct {
	Kind    string   // "" when the source wrote none
	Name    string
	Args    []legacyArg
	Braced  bool     // name { k: v } form
	Surface string
}
type legacyArg struct {
	Name     string // "" for a positional (punned) argument
	Value    string // source text
	Comments []string
}
```

The reader is text-level and comment-preserving: it locates construct headers and step blocks on the `BlankComments` view and slices the raw text, the technique `parser/rewriter.go` uses (it must not import the parser's legacy lowering, which Task 13 deletes). Emission writes two-space indentation, one statement per line, a multi-line call with one argument per line when the source had it, arguments' `//` comments kept above their argument.

- [ ] **Step 1: Failing golden tests** (`bodies_test.go` drives every `testdata/bodies/<case>/in` tree through `rewriteBodies` and diffs against `out`): `forge-state-machine` (the two forge automations and their logic), `decide-and-apply` (`workerModelPullStaleSweep`, `accessRequestExpirySweep`), `deploy-pipeline` (`deployEngineCluster` and `installInstance`), `cluster-bootstrap` (`bootstrapCluster`, comments between steps), `library-promotion` (`indexFileOnCreate`, comments inside the argument list), `bare-calls` (`routingEvidenceFold { }`, `trainSpecialist(...)`, `expireAccessRequest { requestId: item.id }` resolved through the index), `action-forms` (all three legacy action spellings, with and without `on surface`), `parallel` (the procedural parallel config), `ambiguous-name` (a bare call whose name is declared as two kinds: the file is returned unchanged and the error names both). Reference and order rewrites are asserted in Tasks 6 and 7; these goldens are written with their final expected text and fail until those land, which is intended -- mark them `t.Skip("lands with Task 6/7")` only where the reference rewrite is the sole difference, and remove the skip in that task.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/cmd/memqlmigrate/ -run TestBodiesGolden` -- fail.
- [ ] **Step 3: Implement** the index, the reader for both keywords' retired forms and the emitter.
- [ ] **Step 4: Run** -- pass for the cases not skipped.
- [ ] **Step 5: Commit** `Issue #5373: memqlmigrate --rewrite=bodies: the reader of the retired forms and statement emission`.

## Task 6: The `bodies` rewrite: references, arguments and rows (#5373)

**Files:**
- Create: `cmd/memqlmigrate/bodies_refs.go`, `cmd/memqlmigrate/bodies_refs_test.go`

**Interfaces:**
- Consumes: Task 5's `legacyBody`, `declIndex`.
- Produces:

```go
// refContext is what a body's references are rewritten against.
type refContext struct {
	construct string          // "automation" | "logic"
	steps     map[string]string // step name -> the step's call kind ("action", "query", ...) or "expr"
	args      map[string]bool   // declared args fields (automation)
	loopVars  map[string]string // loop variable -> the source text it ranges over
	rowNames  map[string]bool   // names whose value is rows (query calls, logic returning .nodes(), loop vars over rows)
}

// rewriteRefs rewrites one expression's source text. It never parses the
// expression grammar: it scans tokens (identifiers with their dotted and
// call-accessor tails, strings, comments, delimiters, `:` after a map key or
// named argument) so it works on the tree before AND after epic 2's rewrite.
func rewriteRefs(src string, rc *refContext) (string, []string, error) // text, notes, error
```

Rules, in order, each with a table-driven case: `steps.x.result` -> `x`; `steps.x.result.f.g` -> `x.f.g`; `x.result[.f]` (x a step) -> `x[.f]`; an action step's `steps.x.result.result.result.f` -> `x.f` (fewer climbs on an action reference is an error naming the reference); `x.First()` / `.Last()` / `.Empty()` / `.Nodes()` -> lowercase; `first(x)` / `last(x)` -> `x.first()` / `x.last()`; `.payload.f` after `.first()` / `.last()` / on a loop variable over rows / on an element read from a rows name -> `.f` (never on `event`, `args.event` or `args.<field>`); in an automation, a bare args field -> `args.f`, `event.payload.f` -> `args.f` (collecting `f` for Task 8 to declare); a map shorthand entry `{ a.b.c }` -> `{ c: a.b.c }` (after its own rewrite); positional call arguments -> `name: value` where value is the pun's own rewrite (`event` -> `event: event`, a loop variable -> `v: v`, an args field -> `f: args.f`, a step -> `x: x`). A reference the rules cannot classify (`steps.x.status`, an `index` read) is an error naming it, and the file is left unchanged.

- [ ] **Step 1: Failing tests**: the table above, one row per rule and one per refusal; plus the goldens Task 5 skipped now un-skipped.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/cmd/memqlmigrate/ -run 'TestBodiesRefs|TestBodiesGolden'` -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** -- pass.
- [ ] **Step 5: Commit** `Issue #5373: the bodies rewrite retires the step-result spellings, the argument pun and bare args reads`.

## Task 7: The `bodies` rewrite: the order policy and the move comments (#5373)

**Files:**
- Create: `cmd/memqlmigrate/bodies_order.go`, `cmd/memqlmigrate/bodies_order_test.go`, `cmd/memqlmigrate/bodies_order_legacy_parity_test.go` (deleted in Task 13 with the compiler it cross-checks)

**Interfaces:**
- Consumes: Tasks 5-6.
- Produces:

```go
// legacyOrder reproduces compileAutomation's sort (automation_generator.go
// topoSortSteps + collectAllStepReferences) with co-released consumers in
// source order. ties lists groups of item indexes that one release freed
// together -- the orders today's engine chose between at random.
func legacyOrder(b *legacyBody) (order []int, ties [][]int)

// trueDeps is every reference between items as Task 3's scope checker would
// see it: bare names dotted or not, steps.-rooted, first(x).
func trueDeps(b *legacyBody) map[int][]int

// planOrder applies the policy and returns the order to emit plus one comment
// per item that needs one.
func planOrder(b *legacyBody) (order []int, comments map[int]string)
```

Comment texts (pinned):
- moved: `// memqlmigrate: moved above <next> -- the engine ran this here, ordering steps by the references it could see rather than by source (memql#5373)`
- unbound read: `// memqlmigrate: the engine used to run this before <x>, which it reads, so it read nothing; it now runs after <x>, where it is written (memql#5373)`
- unfixed tie: `// memqlmigrate: the engine ran this and <other> in no fixed order; they now run in the order written (memql#5373)`

- [ ] **Step 1: Failing tests**: the three census bodies as goldens -- `releaseWorkspaceOnRunTerminal` (teardown moves above the loop, one moved comment), `driveDeploymentInProgress` and `recordReconciledState` (source order kept; unbound-read comments on `observed` and `reconciled`, resp. `observed` and `reconciled`); a synthetic body whose source order has a forward reference the legacy sort fixed (emitted in T, moved comment); a synthetic tie between two mutations (tie comment on the second); a synthetic tie between two pure statements (no comment); `TestLegacyOrderMatchesTheCompiler` (`bodies_order_legacy_parity_test.go`): for every automation and logic in `dsl/`, `examples/`, `deploy/fleet/dsl/`, parse with the legacy parser, compile with `compiler.NewDefault()` 20 times, and assert every compiled order the compiler produced is consistent with `legacyOrder`'s constraints, and that when `ties` is empty all 20 equal `legacyOrder`.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/cmd/memqlmigrate/ -run 'TestBodiesOrder|TestLegacyOrder'` -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** -- pass. Record the census (every body with a move, an unbound read or a side-effect tie) in the commit body.
- [ ] **Step 5: Commit** `Issue #5373: the bodies rewrite reproduces today's execution order and comments every move`.

## Task 8: The `bodies` rewrite: terse automations, publishing logic, declarations, triggers (#5373)

**Files:**
- Create: `cmd/memqlmigrate/bodies_inline.go`, `cmd/memqlmigrate/bodies_inline_test.go`

**Interfaces:**
- Consumes: Tasks 5-7.
- Produces:

```go
// expandTerse turns `automation N @trigger(...) => logic L` into the
// one-statement automation; annotations and doc comments above it stay above it.
func expandTerse(src string) (string, error)

// inlinePublishingLogic moves each publishing logic whose only caller is one
// automation into that automation, across files, and deletes the logic.
func inlinePublishingLogic(files map[string][]byte, ix *declIndex) (map[string][]byte, []string, error)

// rewriteDeclarationsAndTriggers: mutate -> mutation, partition= removed,
// @schedule(cron=) -> @trigger(schedule=).
func rewriteDeclarationsAndTriggers(src string) string
```

- [ ] **Step 1: Failing tests**: the eight terse automations of the tree (goldens, including `conflictDetection`'s `@filter` line kept above its trigger); `onDelegationCreated` inlined (args declared: `agentId createdBySubject id identityId identitySubject identityType roleCeiling scopes`, shorthand payload entries expanded, the logic deleted from `dsl/identity/logic.memql`); `accountDeletionReminder7Days` inlined (the `for` body's `publish` under its `if`, the `platform.queries` use copied); a publishing logic with a second caller (reported, both files unchanged); `mutate` headers with a doc comment and annotations above them; a `mutation foo(...)` call left alone; `partition="*"` first, middle and last among the kwargs; `@schedule(cron="0 * * * * *")`.
- [ ] **Step 2: Run** -- fail.
- [ ] **Step 3: Implement**; then wire the whole rewrite in `rewriteBodies` in this order per tree: index, terse expansion, inlining, per-body statements with references and order, declarations and triggers.
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/cmd/memqlmigrate/...` -- pass. Add `TestBodiesRewriteIsIdempotent` (the rewrite over its own output over the whole `testdata/bodies` tree changes nothing) and run it.
- [ ] **Step 5: Dry-run over the real tree** into a scratch copy (`cp -r dsl examples deploy/fleet/dsl <scratch>/` then `go run ./cmd/memqlmigrate --edition=2026 --rewrite=bodies -w <scratch>/...`), read the whole diff, and fix every surprise in the rewrite with a new golden case -- never by hand-editing output.
- [ ] **Step 6: Commit** `Issue #5373: the bodies rewrite expands terse automations, moves publishing logic into automations, and renames the declarations`.

## Task 9: The executor runs statements (#5372)

**Files:**
- Create: `component/automations/sequence.go`, `component/automations/sequence_test.go`
- Modify: `component/automations/types.go` (`Step.Binds`; `Step.Returns` -- the `return <call>` form: the sequence ends after the step with its value; step types `expression`, `return`, `block`; `ExpressionStepConfig{Expression string}`, `ReturnStepConfig{Value string}`, `BlockStepConfig{Steps []*Step}`; `FunctionStepConfig.Kind`; `AutomationExecution.Output any`), `component/automations/executor.go` (the main loop becomes a call to `runSequence`), `component/automations/run_scope.go` (bindings by name, child scopes, the row projection), `component/automations/steps/foreach.go` (children through `runSequence` in a child scope named by `As`), `steps/parallel.go` (branches as `block` steps; `wait any`), `steps/action.go` (value is the capability result; envelope to metadata), `steps/automation.go` (value is the sub-run's `Output`), `steps/function.go` (value unwrapped), `steps/steps.go` (register `expression`, `return`, `block`), `component/automations/loader.go` (`validateSteps` knows the new types), `component/work/kind.go` (the new step types), `component/automations/journal.go` (`closeRun` writes `outcome.returned`). The `switch` step type and executor stay until Task 13: legacy bodies still compile to them until the tree migrates.

**Interfaces:**
- Consumes: epic 2's `memql.EvalExpr`, `memql.EvalCondition`, `memql.ExprRow`, `RunScope`; Task 4's JSON.
- Produces:

```go
// runSequence runs steps in order: condition (EvalCondition over scope),
// execute, bind on success, retry, on error, return. It is the ONE loop: the
// Executor's body, each forEach iteration, each parallel branch and every
// logic call run through it.
func runSequence(ctx context.Context, steps []*Step, sc *sequenceContext) (seqOutcome, error)

type seqOutcome struct {
	Returned bool
	Value    any
}
type sequenceContext struct {
	Scope    *RunScope
	StepCtx  *StepContext
	Journal  *workJournal // nil when not journaled
	KeyPath  string       // "" at a run's top level; "decide" for a logic's steps inside it
	Observer func(*StepResult)
}
```

- [ ] **Step 1: Failing tests** (`sequence_test.go`, DB-free with a fake step registry): source order is execution order; a skipped step binds nothing and a later read is absent; the sibling-branch rebinding binds whichever ran; `retry(2)` runs a failing step three times then fails; `retry(1) on error continue` continues with the name absent; `return` stops the loop and a following step never runs; a call step with `returns: true` ends the loop with the call's value (and, retried, with the value of the attempt that succeeded); `return` inside a `for` body stops the enclosing sequence; `for` binds its variable in a child scope and a body name is gone after the loop; `parallel wait any` finishes on the first success; a `block` branch runs its steps in order; an `expression` step evaluates through `EvalExpr`; a query result reads as rows (`rows.first().email`) and marshals back to node maps inside a call argument; an action step's value is its capability result with the envelope in metadata.
- [ ] **Step 2: Run** `go test github.com/znasllc-io/memql/component/automations/ -run 'TestSequence'` -- fail.
- [ ] **Step 3: Implement.** As built (memql-10's runtime merged first, and its flip deletes the legacy branches of these files, so the statement runtime is ADDITIVE and keyed on a marker rather than threaded through the legacy loop): the statement compile writes `"body": "statements"`; `Automation.IsStatementBody()` sends such a run to `runStatementAutomation` -> `runSequence` (sequence.go), and every other automation keeps the loop in `executeWithEvent`, which Task 13 deletes, leaving `runSequence` the one loop. Names live in `nameFrame`s on the Evaluator (statement_scope.go); `RunScope.Lookup` answers from them first, then from the roots, and never from the legacy tiers; a name a skipped statement did not bind is declared and reads absent. A `for` (`forEachStatements`) and a parallel's `block` branches (`BlockExecutor`) reach the runner through the context (`automations.RunStatementBody`), each in a child frame. Rows are `rowView`s: intrinsics and payload fields read directly, and `ResolveV1Map` / `ResolveV1Value` hand the original row map to a call or a payload. A return inside a nested list reaches the enclosing list through the step result's `metadata.returned`.
- [ ] **Step 4: Run** `MEMQL_REQUIRE_DB=1 ... go test -count=1 ./component/automations/...` and `go test github.com/znasllc-io/memql/component/automations/...`.
- [ ] **Step 5: Commit** `Issue #5372: one sequence runner executes statements in order; parallel, retry and on error are statement forms`.

## Task 10: Logic runs on the same model; journal parity (#5372)

**Files:**
- Create: `core/common/write_observer.go`, `component/automations/logic_journal_db_test.go`
- Modify: `component/automations/logic_runner.go` (`RunLogic` runs a v1 body on `runSequence`; the runner parses each loaded function's step list once and caches it by function), `component/memql/engine.go` (`LogicRunner` interface takes the compiled body; `executeLogicFunctionCall` dispatches every v1 logic, one statement or many), `component/memql/function_loader.go` (a v1 logic body compiles at load through `compiler.CompileBody` into `fn.LogicBody`), `component/memql/function_types.go`, `component/memql/executor_mutation.go` (`common.NotifyWrite(ctx, concept, id)` after each successful write), `component/automations/journal.go` (nested keys; `openLogicRun`; back-fill)
- Delete in Task 13, not here (the legacy logic in the tree still runs on them until it migrates): `component/automations/logic_logical.go`, `component/automations/logic_arithmetic.go`; in `logic_runner.go` every `tryEvaluate*`, `EvaluateLocalExpr`, `reconstructPositionalBuiltinCall`, `evaluateCoalesceArgs`, `evaluateScalarArg`, `normalizeStepMethodCalls`, `splitTopLevelComparison`, `compileBodyToAutomation`; `fn.LogicSteps` and the single-return `fn.Expr` logic path; the tests that assert short-circuit mechanics (tests that assert behaviour are re-pointed at `RunLogic` over a v1 body)

**Interfaces:**

```go
// core/common
type WriteObserver func(concept, id string)
func ContextWithWriteObserver(ctx context.Context, obs WriteObserver) context.Context
func ContextWithoutWriteObserver(ctx context.Context) context.Context
func NotifyWrite(ctx context.Context, concept, id string)

// component/memql
type LogicRunner interface {
	RunLogic(ctx context.Context, fnName string, body []map[string]any, args map[string]any) (any, error)
}
```

- [ ] **Step 1: Failing tests**: a logic with a `publish` refuses at load naming the automation form (loader test over a throwaway tree); a logic calling `automation x()` refuses; `TestDirectWritingLogicLeavesARun` (db-gated: call a logic that runs `mutation` through `engine.Execute` as a client; one `v1:work:run` named `logic:<name>` with a step row per statement, including the statements before the write); `TestReadOnlyLogicLeavesNoRun`; `TestLogicInsideARunJournalsAsItsSteps` (an automation whose statement calls a writing logic: the run has rows keyed `decide/<id>` and no second run); `TestSingleStatementLogicRunsOnTheSequence` (`return query x(...)` returns the rows, through `RunLogic`); the journal's own writes never trigger the observer (a journaled logic opens exactly one run).
- [ ] **Step 2: Run** the db-gated package tests -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `MEMQL_REQUIRE_DB=1 ... go test -count=1 ./component/automations/... ./component/memql/...` and `make test`.
- [ ] **Step 5: Commit** `Issue #5372: logic and automations share one execution model; a direct writing logic opens a run and a read-only one leaves none`.

**As built.** `LogicRunner` keeps `RunLogic` for the legacy bodies until the flip. The statement form goes through a second interface beside it, `memql.StatementLogicRunner.RunLogicBody(ctx, fnName, body []map[string]any, args)`, which Task 13 folds into `LogicRunner` when `RunLogic` goes. How each case journals (`component/automations/logic_statements.go`):
- **Direct call.** The journal HOLDS every write, unrendered, in order: the run row, each statement's rows, each heartbeat. The engine's first graph-write report (`common.NotifyWrite` at the end of `executeWrite`) releases it, and after that writes go straight through. A journal that is never released writes nothing. There is no separate back-fill bookkeeping. The run is closed with `closeRunRecord`, never the failure path: the caller already has the error, and nothing resumes a logic's run.
- **Inside a run.** The executor pairs its journal with the run on the ctx (`withRunJournal`, in `executeWithEvent` and resume). A logic journals rows only, through that journal, keyed `<calling key>/<id>`. A nil pairing (`journalSkipsAutomation`) journals nothing. A logic nested in an unopened direct call therefore rides the caller's held journal, and its write opens the one run.
- **Keys.** A step's key is its list's path and its id (`stepKeyIn`): `for_x/<item index>/<id>` inside a `for`, `<parallel>.<label>/<id>` inside a branch. Ids are unique per list only.
- **Expression and return statements** now write an intent row before their receipt, like every other step.
- **Masking.** The journal's own writes are masked from the observer on both of `write`'s paths. Were they not, a flush would re-enter the release that is flushing it.

The run belongs to the deployment (the synthetic journal actor), as an automation's does. Whether a client's direct call should own its run is not decided by D14; raise it in the PR. **Left for Task 11:** `sandbox_registry.interceptLogicFunction` still delegates a `LogicBody` logic to the real executor, and the sandbox's logic runner journals through the engine. Before any statement-body logic ships, it must run the body through the sandbox registry with no journal.

## Task 11: Dry run and resume over the compiled form (#5372)

**Files:**
- Modify: `component/automations/steps/sandbox_registry.go` (`event` intercepted -- a preview publishes nothing; `expression`, `return` pass through; `block` children re-enter the sandbox dispatch; `function` interception keys on `Kind == "mutation"` as well as the registry lookup), `component/automations/resume.go` (rehydrate names from step rows' `Binds`; skip `done`; ignore nested keys), `component/automations/steps/dryrun.go`
- Test: `component/automations/steps/dryrun_v1_db_test.go`, `component/automations/resume_v1_db_test.go`

- [ ] **Step 1: Failing tests**: a dry run of an automation with `publish`, an `if`-gated `mutation` and a `for` over rows reports every step and leaves zero rows and publishes zero events; a run that fails at its fourth statement resumes and finishes with rows equal to an uninterrupted run's; a resume after an `on error continue` failure does not re-run the continued step; a resume of a run whose statement called a logic re-runs that statement once.
- [ ] **Step 2: Run** -- fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the db-gated trees.
- [ ] **Step 5: Commit** `Issue #5372: dry run and resume behave identically over the statement form`.

**As built.** `dryrun.go` needed no change: the driver already runs a statement body through the same executor.

*The sandbox:*
- A statement `for` and a `block` (a parallel branch) re-enter the sandbox through the executor's sequence runner, whose registry is the sandbox. `block` needed classifying; before, the fail-closed arm refused it.
- A function statement that says `mutation` is intercepted even where the registry can't answer.
- A statement-body logic runs through the sandbox registry on a runner `WithoutJournal`.

*Resume:* a statement body doesn't jump into its list. It runs again from its first statement over names rehydrated from the journal.
- Each receipt records the value its statement bound (`StepResult.Bound`, journaled as `MinimalStepResult.Value`).
- Before the resume point, a `done` statement rebinds its recorded value and doesn't run, and a statement that failed under `on error continue` stays absent.
- The resume point is the first statement in the body's order that failed without `on error continue` or was left `running`. Rows come back in no particular order, so `FailedStep` can name a continued failure. The resume point's attempts carry on from the one it recorded.
- `runJournalFromRows` drops nested keys (`decide/a`, `for_x/0/touch`), so a logic's row never becomes the resume point.
- A statement `mutation` needs `AllowSideEffects`, as a mutation step does.
- A query's rows past 100 aren't recorded, and the resumed body reads them again.

*Known limit, inherited from `forEach`:* a `for` or `parallel` that failed part-way runs again whole on resume, because its iterations aren't journaled individually.

## Task 12: The corpus: statement cells, the completeness gate, scenarios (#5374)

**Files:**
- Create: `test/conformance/2026/statements/<construct>/<form>/{expect.json,fixture.memql,*.memql}` for construct in `logic automation` and form in `BodyStatementForms()` plus `scope` and `retired`; `test/conformance/statements_gate_test.go`; `test/conformance/2026/scenarios/<suite>/scenario.json` for `decide-and-apply`, `forge-state-machine`, `deployment-pipeline`, `campaigns-engine`; `test/conformance/scenarios_db_test.go`
- Modify: `test/conformance/2026/README.md` and `scenarios/README.md` (the statements directory, the scenario format), `test/conformance/2026/cells/logic/**` and `cells/automation/**` (after memql-b1's scaffold: cases for every placement; the partition kwarg and terse cells become refusals)

**The run verdict.** Statement semantics need execution, not only loading. Propose to memql-b1, who owns `corpus_test.go`, before writing any such cell: when an `evaluate` case's `file` holds a `logic` rather than a bare expression, the runner loads it and calls it with `args`, comparing the return value with `expect`. If memql-b1 prefers a sixth verdict, use theirs; the cells change only their `verdict` word. DB-free: statement cells use literals and `args`, never rows.

**Scenarios** run the SHIPPED constructs from the embedded tree, never copies (a copy could pass while the shipped automation broke). `scenario.json`:

```json
{
  "automation": "workerModelPullStaleSweep",
  "seed":  [{ "mutation": "createModelPull", "args": { "pullId": "p1", "status": "requested" }, "backdate": "PT10M" }],
  "fire":  { "schedule": true },
  "expect": { "rows": [{ "concept": "v1:worker:modelPull", "id": "p1", "payload": { "status": "failed" } }] },
  "dryRun": { "expectNoRows": true },
  "resume": { "failAt": 1, "expectSameRows": true }
}
```

`scenarios_db_test.go` boots the engine over the embedded tree against the throwaway database, seeds through the named mutations (under a cluster-owner actor), fires the trigger through the real `Executor` (event: `ExecuteWithEvent` with a `graph.node.*` event built from `fire.event`; schedule: `Execute`), queries the expected rows by id, then repeats in a fresh database with the sandbox (`dryRun`) and with a step-registry wrapper that fails the statement at `resume.failAt` once, followed by `ResumeFrom`. Actions (deployment pipeline) run against a stub dispatcher that records calls; the scenario asserts the recorded capability order. Suites: decide-and-apply (`workerModelPullStaleSweep`, `accessRequestExpirySweep`, `pruneStaleClusterNodes`), forge (`routeRequest` then `recordTransition` over three roles and a no-op update), deployment (`deployEngineCluster` for `docker-local` and `azure`, asserting action order and the two deployment rows), campaigns (a generated event-email automation from `component/emailrules.Generate` over a seeded rule, firing on a seeded row, asserting the `v1:campaigns:delivery` row the builtin writes through the stub sender).

- [ ] **Step 1: Write the gate first** (`statements_gate_test.go`): for each construct and each form in `parser.BodyStatementForms()`, `statements/<construct>/<form>/` holds at least one `load_ok` (or logic `evaluate`) case and one refusal; `statements/<construct>/retired/` holds one refusal per retired-form code of Task 2; a directory the list does not name fails. Run -- FAIL naming every missing cell.
- [ ] **Step 2: Write the cells** until the gate passes; every refusal's message is measured by running the runner, then pinned.
- [ ] **Step 3: Negative control:** delete one form's refusal case, confirm the gate names it, restore.
- [ ] **Step 4: Scenarios**: write `scenarios_db_test.go` and the four suites; run `MEMQL_REQUIRE_DB=1 ... go test -count=1 -run TestScenarios ./test/conformance/` -- every suite passes, and each suite's negative control (a deliberately wrong expected row) fails naming the row.
- [ ] **Step 5: Commit** `Issue #5374: statement cells for every form, their completeness gate, and the four scenario suites with dry-run and resume cases`.

**As built, in two slices.** Steps 1-3 landed first; Step 4 (the scenarios) lands after Task 13, because a scenario runs the SHIPPED automations and they aren't statements until the tree migrates.

*The run verdict* is memql-b1's choice. It's an explicit `call` field on an `evaluate` case, not a new verdict and not keyed on the file's first declaration.
- The file loads like a load case.
- The named logic compiles as the function loader compiles it, then runs through `LogicRunner.RunLogicBody` (`corpus_call_test.go`). The step registry is the production one with every step that could reach the outside refused, so a `for` or a parallel runs as it does on a node. The only construct call it runs is a call to another logic the case declares.
- memql-b1 owns `corpus_test.go` and asked that the hook be confined to the field, its validation, the call path and one README sentence.

*The layout* is `statements/<construct>/<form>/` plus `scope/`, `body/` and `retired/`. `body/` is added to the plan's list, for refusals of a body as a whole (empty, two statements on a line).

*What the gate derives:*
- the forms from `BodyStatementForms()`;
- the retired codes from the `_retired` members of `BodyRefusalCodes()`;
- the codes deferred to the flip (`statementRetiredAtTheFlip`), which the transitional dispatch still accepts: `body_step_retired` and `body_terse_retired` for automations, `body_block_retired` for logic. The flip empties that map and adds the cells.

*The scenarios, as built* (agreed with memql-b1, who owns the corpus layout).
They land before the flip, not after it, because running the same scenarios
over both body forms is itself the flip's behaviour check.
- The format is one `scenarios/<suite>/scenario.json` per suite, naming
  shipped automations and mutations: seeds, fires, expected rows (by id or by
  `where`, with an optional version `history`), expected calls, a `dryRun`
  fire and a `resume` point. `test/conformance/2026/scenarios/README.md`
  documents it.
- The runner (`scenarios_db_test.go`) boots the conformance rig's engine,
  fires through the real Executor over the production step registry, and
  stubs the capability dispatcher. It is database-gated like the
  differential lane, so it runs in mcp-conformance.
- Suites: decide-and-apply (the model-pull sweep, the stale-node prune),
  forge (owner, writer, reader, and the unchanged status), deployment
  (docker-local, and azure failing its gate and rolling back).
- Campaigns is pending: it needs the campaigns integration wired in the rig.
- The one legacy difference found is transitionEventKind's `old == st`,
  already recorded as a legacy defect in the logic goldens. The scenario
  states the body's meaning and carries a `legacy` count, which the runner
  refuses once the tree is statements.

*Constraints the fixtures met:*
- Every load case boots in one engine, and function names are flat, so each directory's fixture declares its constructs under corpus-unique names. A duplicate either fails the cross-namespace import gate or leaves a diagnostic no domain claims, which silently drops the runner into one boot per case (5s became 106s).
- A fixture concept must not share a bare name with a tree concept: `order` made Shopify's ambiguous.

## Task 13: The flip: the tree migrated, the legacy body grammar deleted (#5371, #5373)

**Files:** `dsl/**`, `examples/**/dsl/**`, `deploy/fleet/dsl/**`, `test/**/*.memql`, every Go test fixture that embeds a body or a `mutate` header (87 files embed `step`, 174 `automation`/`logic`, 67 `mutate`; measure again), `component/emailrules/generate.go` (+ its goldens), `dsl/authoring/**` material that shows bodies, `dsl/_reference/_automation.memql` and `_agent.memql` (don't-do-this skeletons gain the retired body forms), `component/language/parser/{rewriter.go,parser.go,grammar_version.go,grammar_surface_drift_test.go,suggest.go,acceptstamp_migrate.go}`, `component/language/ast/ast.go` (legacy statement nodes and `AutomationDef.Steps` deleted), `component/language/compiler/automation_generator.go` (`topoSortSteps`, `collectAllStepReferences`, `extractStepReferences`, `stepExpressionStrings` and the legacy step compile deleted), `component/automations/args_resolution.go` (G2 deleted; the scope checker is the gate), `component/language/dslspec/*` (constructs: `mutation`; the statement keywords and clause words in the lexicon; body blocks), `component/memql/sense/*` (snippets and completion for statements; in-scope names from `compiler.BodyNames`; hover for statement keywords), `cmd/memql-lsp/internal/grammar/*` and `editors/vscode/syntaxes/*` (regenerated), `editors/vscode/{package.json,CHANGELOG.md}` and `parser.EditorRelease` (D25), the `mutate` keyword sites (`function_slices.go`, `construct_catalog.go`, `dslgate/imports.go`, `authoring_bundle_slices.go`, `authoring_diagnostic_position.go`, `sense/{spec,context,runnable,complete,snippets}.go`, `core/baseparser/iface.go`, `sdk/gen/gen.go`, `cmd/memqlmigrate/rowauthz_infer.go`, `function_loader.go`, `edge/publish.go`), `cmd/memqlmigrate/rewrites.go` (`terse-automation` deleted), `cmd/memqlmigrate/bodies_order_legacy_parity_test.go` (deleted), `docs_retired_keywords_test.go` (`retiredDeclarationKeywords` becomes `{"mutate", "mutation", "memql#5370"}`; the `cmd/memqlmigrate/testdata/bodies/` exemption stays, its inputs now being the retired side) with `component/memql/callgraph/keyword_test.go` (its negative fixture writes `mutate`), and every tracked file that shows a `mutate <Concept> <name> {` declaration, because that gate sweeps every tracked file and would red the flip commit (8 markdown, 36 `.memql`, 38 Go, 3 other on the plan's base; measure again), the runtime's legacy halves Tasks 9 and 10 left standing (`StepTypeSwitch`, `SwitchStepConfig`, `SwitchCase`, `steps/switch.go` and every reference in `sandbox_registry.go`, `loader.go` `validateSteps`, `args_resolution.go`; `logic_logical.go`, `logic_arithmetic.go`, the short-circuits and `compileBodyToAutomation` in `logic_runner.go`; `fn.LogicSteps` and the single-return logic `fn.Expr` path; the "no `binds` means bind under the id" fallback in `runSequence`)

**The logic equivalence goldens (memql-10, first at a0b13d6df).** `component/automations/steps/logic_v1_corpus_test.go`, `TestLogicCorpusRuns`: 34 logic constructs, 1136 runs, goldens in `component/automations/steps/testdata/logic_corpus/<domain>/<name>.json` (`construct`, `runs` keyed "<variant> | actor <role> | <n> rows"; each run's `input`, `outcome` -- the result plus the recorded construct calls and published events, instants masked as "<ts>" -- and `legacyDefect` on the 248 runs where the legacy build did not do what the body says, the golden holding the v1 result). `-update` is defined in that package only (`go test github.com/znasllc-io/memql/component/automations/steps -run TestLogicCorpusRuns -update`) and writes only when every arm agrees. memql-10's flip deletes the legacy arm. Here, before anything is deleted: append `logicArm{name, legacy: false, source, run}` to `arms` -- the source through `--rewrite=bodies`, the run through `RunLogicBody` over the gate's probe -- and require it to match the goldens and the bridge arm on every run; then delete the bridge arm with `LogicSteps`/`RunLogic`, leaving this arm against the goldens.

**As built, before the flip.** The bodies rewrite is a library, `component/language/bodymigrate` (moved out of `cmd/memqlmigrate`, which keeps the CLI glue, the embedded-tree index, the golden test over `testdata/bodies` and `--go-fixtures`). `TestLogicCorpusRuns` carries a third arm, `bodiesArm`: every source's V1 text through `bodymigrate.RewriteTree` over the whole tree (`corpusSource.Bodies`), each logic required to build to `fn.LogicBody`, each run through the engine to `RunLogicBody`. It matches the goldens and both other arms on all 912 runs of the 27 logic it can run. It has no run of the 7 the rewrite moves into their automations (a logic may not publish): `conflictDetection`, `accountDeletionReminder7Days`, `accountDeletionReminder25Days`, `auditEventRetentionSweep`, `onDelegationCreated`, `purgeExpiredOutputScreenings`, `purgeExpiredSafetyClassifications`; the test fails if it skips a logic none of whose golden runs publishes. At the flip:
- The legacy arm and the expressions codemod step go with memql-10's flip; after the bodies flip `Bodies == Current`, so `bodiesArm` IS the v1 arm -- delete `todayArm(t, "v1", ...)` and keep one.
- The 7 moved logic leave the tree, so their golden files become runs no fixture makes: delete those 7 files. Their statements now run as automation bodies, which the scenario suites cover (Task 12 step 4) -- add the two deletion reminders and `onDelegationCreated` there.
- `retiredDeclarationKeywords`' exemption gains `component/language/bodymigrate/` beside `cmd/memqlmigrate/testdata/bodies/` (the rewrite's code and tests spell `mutate` on purpose), and `scripts/migrations/expressions_go_fixtures`' `defaultExcludes` gains it too (memql-10's file; `--go-fixtures` already excludes it).
- The migrated tree is checked ahead of the flip by `component/automations/migrated_tree_load_test.go`: the boot walk (`loadFromTree`) loads the same 58 automations, each a statement body, and `dslimports`' integrity lanes find exactly what they find on the shipped tree. Its first run found the rewrite leaving an import only a moved logic used (`safety/logic.memql`'s `globalVariable`), which the import gate refuses; the rewrite now prunes such imports (golden case `inline-prunes-imports`). The test reads `mutation` declaration headers back as `mutate` until step 6 gives the parser the keyword; delete that line in the flip.
- The rewrite leaves `dsl/data/logic.memql` and `dsl/safety/logic.memql` with no construct (every logic in them moved): `git rm` both.

- [ ] **Step 1:** Rebase onto memql-10's tree-migration commit (epic 2 Task 9). Run `go run ./cmd/memqlmigrate --edition=2026 --rewrite=bodies -w dsl examples deploy/fleet/dsl test` and read the whole diff (`git diff --stat`, then every automations/logic file in full, then a sample of the `mutate` renames).
- [ ] **Step 2:** Run it again; `-check` reports nothing (idempotent).
- [ ] **Step 3:** Hand-edit the prose the rewrite cannot: the header comments in `dsl/deployment/automations.memql` ("EXECUTION ORDER" and the switch/topo-sort paragraphs), `dsl/cluster/automations.memql` ("an automation has no let-binding"), `dsl/forge/automations.memql`, `dsl/library/automations.memql`'s authoring note, and every comment that describes the topological sort, `steps.<id>` or `step` blocks as current. Each edit states what is true now.
- [ ] **Step 4:** Migrate Go fixtures with the rewrite's library entry (`migrateBodiesSource(src string, ix *declIndex) (string, error)`) over each string literal that holds a body or a `mutate` header, then read every changed fixture.
- [ ] **Step 5:** Update `component/emailrules/generate.go` to emit `builtin emailRuleFire(emailRuleId: "...", nodeId: args.id, event: event)` as a statement under `@trigger(event=..., concept=...)` (no `partition`), regenerate its goldens; update the authoring material.
- [ ] **Step 6: Flip.** Delete Task 2's transitional dispatch: every detector hit is now its refusal. Delete the legacy body grammar, the rewriter's logic/automation/terse stages, the legacy AST nodes, the topological sort, G2 and the runtime legacy halves listed above; the `mutation` declaration keyword replaces `mutate` in `mutationStructHeader` and `StructFormKeywords`, and `mutate` refuses with `mutate_keyword_retired`.
- [ ] **Step 7:** Bump `GrammarVersion` (`2026.09-dsl-v1-bodies-<digest>`), add grammar-surface corpus entries for every retired form and every new form, `make vscode-grammar`, move the extension pins, version and CHANGELOG (D25).
- [ ] **Step 8: Verify** `TestUnifiedTreeLoadsClean`, `TestStrictBoot_EmbeddedTreeIsClean`, `go run ./cmd/memqllint dsl` (zero refusals -- the #5373 acceptance), `go run ./cmd/memqllint examples/deploypack/dsl` and the other packs, `make test`, the db-gated trees.
- [ ] **Step 9: Commit** `Issue #5373: the tree migrated to the body language; the parser refuses the retired forms`.

## Task 14: The gates that read bodies (#5371)

**Files:** every gate that reads automation or logic text or the legacy step AST: `test/dslconformance/*` (list them with `grep -ln 'step \|StepDef\|AutomationDef\|steps\.' test/dslconformance/*.go`), `component/memql/dslgate/*`, `component/memql/callgraph/callgraph.go`, `component/memql/sense/runnable.go`, `component/memql/keyword_slices.go`, `component/memql/authoring_catalog.go`, `component/architecture` (if it derives automation topology from steps), `sdk/gen/gen.go`, `component/automations/loader.go`'s raw-text gates (`$steps.`, inline step blocks, direct `query()`/`mutation()` -- delete the ones the parser now refuses, keep G5)

Rule, as in epic 2: a gate that matched text now walks the parsed `ast.Body`, and every gate asserts a reachable positive (it found at least N constructs of the kind it checks), so it cannot go blind silently.

- [ ] **Step 1:** For each gate, a failing test with a v1 fixture the gate must still catch (the logic-purity gate still flags a logic that writes where it must not; the callgraph validator still sees a `mutation` statement inside a `for`).
- [ ] **Step 2:** Port each gate.
- [ ] **Step 3:** `go test -count=1 github.com/znasllc-io/memql/test/dslconformance/... github.com/znasllc-io/memql/component/memql/...` and `make test`.
- [ ] **Step 4: Commit** `Issue #5371: the gates read the body language`.

## Task 15: Documentation (#5371-#5374)

**Files:** `docs/public/language/memql.md` (a "Bodies" section: the statement table, names and scope with the once-block rule, trailing clauses, what each statement is at run time, the D14 rules, the retired forms and the rewrite; the Automations and Logic sections rewritten), `docs/public/language/authoring-rules.md` (every rule about steps, `steps.`, G2, G5, terse, `body { }`, `mutate` restated for v1), root `CLAUDE.md` ("Argument resolution", "Automations", "Logic", "Functions" examples, "Retired author-side forms", the `mutate` examples) -- verify with `go test -count=1 .`, `component/language/CLAUDE.md` (the native body parser; the struct-form rewriter no longer covers logic/automation), every ```memql fence under `docs/public` and README (`TestDocsMemqlSnippets`), `test/conformance/2026/README.md`

- [ ] **Step 1:** Write the docs; every example is copied from a corpus cell that loads.
- [ ] **Step 2:** `go test -count=1 . ./docs/...`.
- [ ] **Step 3: Commit** `Issue #5371: the body language documented`.

## Task 16: Integration, verification and delivery

- [ ] **Step 1:** Rebase on the latest `origin/epic/dsl-v1-expressions` (and on `main` once epics 1 and 2 merge); rerun the rewrite over `dsl/**` rather than resolving migrated files by hand; `make arch-model`.
- [ ] **Step 2: Verification matrix**, every output read: `make test`; `MEMQL_REQUIRE_DB=1` over `scripts/ci/db-gated-packages.sh --trees` plus `./test/conformance/...`; the seven node-tag builds (`go build .`, `-tags identity|agent|planner|workbench|mcp|edge`); `scripts/ci/module-boundaries.sh`; `go run ./cmd/memqllint dsl`; `make vscode-test`; `make sdk-ts-typecheck`; `make frontdoor-paths-check`; `go test -count=1 . ./scripts/...`; `gitleaks dir .` over the diff.
- [ ] **Step 3:** Delete this plan file and its checkpoint (`2026-09-13-dsl-v1-bodies-checkpoint.md`).
- [ ] **Step 4:** Push; open the PR (one `Closes #n` line per issue; the behaviour changes listed above; the census of reordered bodies; the frontend note: none, the wire is unchanged); watch CI; enqueue with `gh pr merge <n> --repo znasllc-io/memql` only after epics 1 and 2 have merged.
- [ ] **Step 5:** After merge: close any issue the merge did not, delete the local branch and the worktree, prune refs.

# Handoff: chat tab daily-space + multi-statement Logic parser

**Branches:** `feature/chat-daily-space` in `memql` and `memql-cockpit`.
**Companion PRs:** see this branch's PR description for the cockpit
companion. The `memql-bff-copresent` branch of the same name was
created but holds no changes -- safe to delete on merge.

---

## What shipped on this branch

### memql

| Change | File(s) |
|---|---|
| Top-level Execute now dispatches a `BuiltinFunctionExpression` at `plan.Root` straight to the builtin executor instead of falling into SQL-filter compilation (which tripped "expected collection literal"). Closes the gap for single-statement Logics whose body is `return <builtin>({...})`. | `component/memql/engine.go` |
| `ASTConverter` gained cases for every typed expression-builtin parser node: `CoalesceExpr`, `CondExpr`, `ConcatExpr`, `HashExpr`, `CanonicalIdExpr`, `FirstExpr`, `LastExpr`, `LowerExpr`, `UpperExpr`, `TrimExpr`, `NilExpr`. Each normalises to a `FunctionCallExpression` with positional args -- the same shape `expressionToFunctionCall` produces at step-RHS position. Before this, Logic bodies that ended with `return coalesce(...)` failed to load with `unsupported parser expression type: *ast.CoalesceExpr`. | `component/memql/ast_converter.go` |
| `compileBodyToAutomation` re-attaches a synthetic `_return` Step on the way out. The compiler peels the parser's `_return` step out of the steps slice and emits it as a top-level JSON field; the automations `Loader` has no struct field for it and a vanilla unmarshal drops it on the floor. The runtime then refused to run the Logic with "logic %q has no `_return` step." | `component/automations/logic_runner.go` |
| `parseIfStatement` actually parses the body now. The legacy implementation only matched `continue`/`break`/`return` tokens inside the then-block and silently advanced past anything else, so `if cond { name := funcCall(...) }` at the top level of a Logic body silently ran nothing. Rewritten to call a new `parseIfBodyStatements` helper that accepts assignments, `for item := range`, nested `if`, and bare function-call expressions (the bare form emits an anonymous function step keyed by source position). | `component/language/parser/parser.go` |
| `ifStatementToSteps` flattens the parsed if into a list of conditional `StepDef`s. Each step in `ThenSteps` gets the if's condition stamped on top of its own (combined with `and` for nested conditions). `ElseSteps` and `else-if` chains layer the negated parent condition. `ForEachStepConfig.Do` inner steps inherit the outer if's condition too -- a for-range nested under an if gate keeps the gate on its body steps. | `component/language/parser/parser.go` |
| `name := if cond { multi-stmt }` now reports a targeted error instead of crashing the parser with "expected '}', got ':='". The single-call form `name := if cond { funcCall(...) }` keeps its existing semantics; multi-statement bodies are routed to the top-level form (the LHS only binds one result -- silently rewriting would lie about which step's result it actually carries). | `component/language/parser/parser.go` |
| `IfStmt` AST gained `ThenSteps`, `ElseSteps`, and `ElseIf` fields. The legacy `Then []Node` / `Else []Node` slots stay for callers that walk them. | `component/language/ast/ast.go` |
| `dailyspace` integration gained `ensureForCaller` -- resolves the calling user's id from the request's auth context (JWT/PAT subject) and delegates to the existing `ensureForUser`. The cockpit chat view calls this on connect so the daily space lands without needing the cockpit to know its own canonical user id. Wired through a `cognitionEnsureDailySpaceForCaller` builtin and a `logicEnsureDailySpaceForCaller` Logic wrapper so top-level Execute can dispatch it. | `integrations/dailyspace/dailyspace.go`, `dsl/cognition/builtins.memql`, `dsl/cognition/logic.memql` |
| Regression tests: parser side (multi-stmt if + for-range gating + if/else negation), converter side (typed-builtin conversion), runner side (`_return` round-trip). | `component/memql/multistep_logic_test.go`, `component/automations/logic_runner_test.go` |

### memql-cockpit

| Change | File(s) |
|---|---|
| Tab order: Clusters, **Chat**, Concepts, Planner, Agents, Settings. F2 now opens Chat as the daily-conversation surface. F-key hints in the help overlay updated to match. | `cli/app.go`, `cli/ui/help.go`, `cli/CLAUDE.md` |
| Chat view calls `logicEnsureDailySpaceForCaller({})` once per session on first refresh (the call is content-addressable on `(userShortId, dateKey)` so repeat ticks collapse server-side). | `cli/chat/view.go` |
| Spaces list pins `kind=daily` rows at the top and auto-selects today's daily on first paint, until the user moves the highlight with Up/Dn. Daily rows render with a `Today: ` prefix. | `cli/chat/view.go` |
| Refresh + ensure errors route through `OnStatus -> notifications.Sync("chat", SeverityError, msg)` so the user sees them in the header notification center (with copy + dismiss) instead of truncated in the bottom chrome strip. | `cli/chat/view.go`, `cli/app.go` |

---

## What's left

### 1. Verify the daily space appears in Chat end-to-end

The plumbing is in place but I never saw a `v1:cognition:space` row land in the DB during the session. The running cockpit's `ensureRan` latch may be stuck from an earlier failed attempt, and queries to the bff stopped flowing partway through testing. To verify:

```bash
# Quit the running cockpit (Ctrl+Q on the TUI), then:
/home/znas/projects/memql/memql-cockpit/bin/memql-cockpit

# Open Chat (F2). Expected: "Today: Daily YYYY-MM-DD" appears at the
# top of the SPACES list, auto-selected.

# DB check:
docker exec memql-db psql -U memql -d memql -c \
  "SELECT id, payload->>'kind' FROM \"MemoryNodes\" \
   WHERE concept='v1:cognition:space';"
# Expected: one daily-<userShortId>-<dateKey> row.
```

If the call still fails the error lands in the header notification center (copy-able). The most likely surface for residual failure is the partition envelope -- the cockpit's selected partition may differ from where the daily lands.

### 2. Method-call dispatch on bound step results (runtime evaluator)

`logicRevokeExpiredDelegations` and friends now parse + load + reach the runtime, but fail at evaluation time with:

```
function "expiredDelegations.Len" not found
```

The Logic body uses `<stepName>.<method>()` syntax to read methods on a bound step result (`expiredDelegations.Len()`, `expiredDelegations.Nodes()`, `existing.First()`, `existing.Empty()`). The runtime evaluator doesn't recognise this shape today -- it treats the dotted identifier as a top-level function name.

Where to look:
- `component/memql/runtime_evaluator.go` -- expression evaluator. The `<stepName>.<method>()` shape lands as a function-call expression whose `Name` is `"<stepName>.<method>"`. Need to detect the dotted form, look up the step result, and dispatch the method.
- `component/automations/evaluator.go` -- step result binding. Already stores results by step ID; the runtime needs to expose method-style access on those results (length, iteration, first, empty, etc.).
- DSL bodies in `dsl/identity/logic.memql`, `dsl/workbench/logic.memql`, `dsl/cluster/logic.memql` -- the callers of `.Len()` / `.Nodes()` / `.First()` / `.Empty()`. Once the evaluator supports it, several existing failing logics should start working.

### 3. Orphan-fragment cleanup in the .memql tree

The loader emits a stream of:

```
WARN unified function loader: skipping slice that failed to parse
  file=cognition/logic.memql function=autoJoinSI kind=logic
  error=expected File, got *ast.SpecReferenceExpr
```

These aren't from the parser changes on this branch -- they come from stray `logic <name> { event: event }` fragments scattered at the file's top level (outside any automation step block). They look like leftover paste artefacts from an earlier refactor. The slice walker tries to parse each as a top-level definition, the parser returns a `SpecReferenceExpr` (the slice is a bare identifier reference), and the loader rejects it.

Where they live:
- `dsl/cognition/logic.memql` -- lines after the legitimate `logic` declarations, e.g. `logic purgeExpiredArchivedSpaces { event: event }` repeated.
- `dsl/cognition/automations.memql` -- similar tail fragments.
- `dsl/cluster/automations.memql`, `dsl/cluster/logic.memql` -- same pattern.
- `dsl/worker/`, `dsl/identity/`, `dsl/workbench/automations.memql` -- spot-check.

Fix: open each affected file, find the orphan `logic NAME { event: event }` lines outside any automation block, delete them. These lines aren't referenced by any automation step (the step bodies use `@useLogic(...)` annotations + inline `logic <name> { event: event }` WITHIN a step). Once they're gone the warnings stop and the loader stops working through 100+ failed parse attempts on every boot.

### 4. The two pre-existing automation failures the boot logs flag

Unrelated to this branch but worth knowing:

- `logicBootstrapCluster execution failed: function "logicBootstrapCluster" not found` -- the cluster bootstrap automation references a Logic the loader couldn't register. Likely a casualty of one of the orphan fragments in `dsl/cluster/`.
- `logicBootstrapDefaultPartition execution failed: mutationCreatePartition: argument validation failed: required argument "name" is missing` -- the `bootstrapDefaultPartition` automation builds a mutation call without the required `name` arg. Pre-existing; the mutation contract probably changed without updating the caller.

These have been failing in this cluster for as long as I've been looking -- not introduced by this work.

---

## How the pieces hang together

```
Cockpit (Chat tab refresh tick, every 3s)
    |
    | qc.Execute("logicEnsureDailySpaceForCaller({})")
    v
bff: MemqlService.Stream  ->  engine.Execute(query)
    |
    | resolvePlanFunctions:
    |   - logicEnsureDailySpaceForCaller (FunctionKind=logic, LogicSteps=nil)
    |   - expandFunctionCall: clones fn.Expr (a FunctionCallExpression
    |     for ensureDailySpaceForCaller), recurses via expandExpression,
    |     hits the BuiltinFunctionExpression branch -> returns a
    |     BuiltinFunctionExpression{Executor: "integration.dailyspace.ensureForCaller"}.
    | plan.Root = BuiltinFunctionExpression
    v
engine.Execute: top-level BuiltinFunctionExpression dispatch (NEW)
    |
    | evaluateBuiltinFunctionExpression -> handleEnsureForCaller
    v
dailyspace.handleEnsureForCaller
    |
    | auth.TokenInfoFromContext(ctx).Subject -> userId
    | ensureForUser(ctx, userId):
    |   - loadUser -> read preferences (timezone, dailySpaceEnabled)
    |   - dateKey = now in user's timezone, formatted YYYY-MM-DD
    |   - spaceId = "daily-<userShortId>-<dateKey>"
    |   - mutationCreateDailySpace under systemActorContext
    v
DB: v1:cognition:space row with kind="daily", partition=active.

Cockpit (next 3s refresh):
    qc.Execute("concept==v1:cognition:space; payload.status==\"active\"")
    -> daily row appears in the list, auto-selected.
```

---

## Verification checklist (for the merger)

- [ ] Cockpit relaunch shows the daily space in Chat within one refresh tick.
- [ ] `docker exec memql-db psql ... 'SELECT ... FROM "MemoryNodes" WHERE concept=...space'` returns exactly one daily row per active user.
- [ ] `make test` is green in both repos.
- [ ] The `unsupported parser expression`, `has no _return step`, and "expected collection literal" warnings stay gone from the bff boot logs.
- [ ] No new loader warnings appear that weren't already there (you'll still see the orphan-fragment warnings -- those are item 3 above, not regressions).

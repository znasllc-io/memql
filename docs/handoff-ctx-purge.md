# ctx-envelope purge — handoff for follow-up sessions

> **Status:** Logic slice + F.1 (rewriter) + F.3 (executor) + F.6
> (Logic→mutation dispatch) landed. F.2, F.4, F.5, F.7 are open
> follow-up work. Branch: see the most recent
> `feature/dsl-engine-ctx-purge-e2e` / successor PR.

This doc tracks the remaining work for **removing the legacy `ctx`
runtime envelope from every DSL construct** -- the directive being
that authors should never see `(ctx any)`, `ctx.input.X`,
`ctx.output = ...`, or `return ctx, nil` anywhere in `.memql` files
or in the rewriter's emitted procedural form.

---

## What landed so far

The Logic construct is the only receiver that's been ctx-purged
end-to-end on the parser + executor side:

- **Parser** (`component/language/parser/parser.go`) -- added the
  missing `case FunctionTypeLogic` in `parseGoStyleFunction`'s body
  switch. Previously every logic body was silently dropped because
  no case matched; the failure surfaced downstream as `function
  "logicX" not found` at automation trigger time. Logic bodies now
  reuse `parseGoStyleAutomationBody`, which accepts the
  multi-statement form with a trailing `return <expr>` terminator.
- **Function loader**
  (`component/memql/function_loader.go`) -- added a Logic case that
  extracts the `_return` step's expression off the parsed
  `*AutomationDef` and converts it to the engine's `fn.Expr` for the
  evaluator. Multi-step logic bodies (with intermediate `:=`
  assignments whose side effects matter) are explicitly rejected
  with a "not yet supported" error pointing at the structural fix.
- **Function validator**
  (`component/memql/function_validator.go`) -- `expandFunctionCall`
  now emits a `BuiltinFunctionExpression` when the called function
  has an `Executor` set but no DSL body (the shape of every
  integration-backed builtin). Previously the validator returned a
  bare `FunctionCallExpression` that the evaluator immediately
  errored on with "function was not expanded during parsing; this
  is a bug."
- **Declared-usage validator**
  (`component/memql/declared_usage_validator.go`) -- skips the "args
  field must be referenced as `args.X`" check for Logic functions.
  Logic args come from the automation step caller (always
  `event: event`); cron-fired logics legitimately ignore the
  `event` payload.
- **Step args resolver**
  (`component/automations/steps/function.go`) -- bare `event` /
  `item` / `index` identifiers in step arg config now resolve as
  runtime references (matching the `event.X` / `item.X` shapes that
  already worked).
- **DSL** -- `dsl/cognition/logic.memql` and `dsl/platform/logic.memql`
  updated so the daily-space and bootstrap-default-partition logic
  bodies use `return <expr>` instead of `ctx.output = <expr>`. The
  `event` arg is declared optional (no `@required`) for cron-fired
  logics that don't read it.

After this slice: the daily-space rollover cron executes
successfully end-to-end (parse → load → register → schedule → fire →
walk users → call `ensureForUser` per user). The user-create and
auth-session automations also fire and load their logics without
error -- the daily-space row creation runs the moment a `v1:identity:user`
or `v1:identity:authSession` lands.

---

## What's still left

### F.1 -- Rewriter: stop emitting `ctx.output = ...` _(LANDED)_

`component/language/parser/rewriter.go` now emits the canonical
`return <expr>, nil` shape:

- `emitQuery` writes `return <queryExpr>, nil`
- `emitMutation` writes `return insert(...)` / `return update(...)`
- `translateArgsRefsToCtx` is a no-op -- `args.X` passes through to
  the engine parser, which learned `args.X` natively in F.3

Carry-overs:

- `emitFuncHeader` still emits `(ctx any)` as the parameter name
  (just a placeholder identifier; the parser's
  `validateGoStyleFunctionSignature` only checks the type). Renaming
  to `_` / `args` is cosmetic and can land alongside F.4.
- `emitLogic` still appends a trailing `return ctx, nil` when the
  body has no explicit terminator. Bodies the rewriter touches in
  practice all carry their own `return <expr>`, so this is dead
  emission -- safe to clean up alongside F.2.

### F.2 -- Parser: drop the ctx.output body parsers

Once F.1 lands, the parser's transitional dual-form code paths can
all delete:

- `parseGoStyleCtxOutputBody` (parser.go:870)
- `parseGoStyleCtxOutputMutationBody` (~parser.go:1086)
- `parseGoStyleCtxOutputAutomationAssignment` (parser.go:932)
- `looksLikeCtxOutputBody` (parser.go:916)
- The `if p.looksLikeCtxOutputBody() { ... } else { ... }` branch in
  the Policy case of `parseGoStyleFunction` -- collapse to the
  spec-style `return <expr>` parser
- The corresponding fallback branches in
  `parseGoStyleQueryBodyOrLegacy` and
  `parseGoStyleMutationBodyOrLegacy`, which become straight
  delegations to `parseGoStyleQueryBody` / `parseGoStyleMutationBody`

### F.3 -- Executor: resolve `args.X` without the ctx envelope _(LANDED)_

The engine parser (`component/memql/parser.go`), the mutation-template
parser (`component/memql/mutation_templates.go`), and the policy
field-ref resolvers (`policy_evaluator.go`, `policy_function_loader.go`)
all accept `args.X` as a first-class caller-arg reference. Both
`args.X` and `ctx.X` produce the same `ArgReference` AST node so the
rest of the validator / executor pipeline stays single-shape.

Regression coverage: `component/memql/args_ref_test.go` pins the
parser path; the existing `caller_ref_test.go` shape sits next to it.

### F.4 -- Strip `ctx.X` and `(ctx any)` from every remaining .memql

Files still carrying `ctx.output =` or `(ctx any)` (one-line bodies
are the natural first batch):

```
dsl/cluster/logic.memql
dsl/cognition/logic.memql           (multi-step logics only -- daily-space ones done)
dsl/cognition/queries.memql         (legacy func form)
dsl/common/queries.memql            (legacy func form)
dsl/data/logic.memql
dsl/identity/automations.memql
dsl/identity/logic.memql
dsl/memql/queries.memql
dsl/planner/queries.memql
dsl/platform/logic.memql            (bootstrap done)
dsl/policies/policies.memql
dsl/router/queries.memql
dsl/workbench/logic.memql
dsl/worker/logic.memql
dsl/_reference/_spec.memql
dsl/_reference/_trait.memql
```

The `.memql` rewrite is mechanical:
- `ctx.output = <expr>` → `return <expr>`
- `func (Kind) NAME(ctx any) (any, error) { ... }` → the struct form,
  or the post-rewriter signature once F.1 drops the parameter
- `ctx.input.X` (only in legacy procedural files / comments) →
  `args.X`

### F.5 -- Multi-step Logic execution

This is the bigger structural item. The function loader's Logic case
currently rejects multi-step bodies:

```
function "X": multi-step logic bodies are not yet supported by the
function executor (N intermediate steps before `_return`); restructure
as a single `return <expr>` statement
```

Several existing logics need full step-runner-backed invocation to
work end-to-end:

- `logicAutoJoinSI` (joins owner GA + checks/inserts participants +
  publishes event)
- `logicBootstrapSession`
- `logicGenerateResponse`
- `logicPurgeExpiredArchivedSpaces`
- `logicVoiceMigrationOnSecondHuman`
- `logicAccessRequestExpirySweep`
- `logicAccountDeletionReminder{7,25}Days`
- `logicAccountDeletionSweep`
- `logicAuditEventRetentionSweep`
- `logicMagicLinkExpirySweep`
- `logicOnDelegationCreated`
- `logicProvisionPersonalPartitionOnFirstLogin`
- `logicRevokeExpiredDelegations`
- `logicPurgeExpiredPolicyTraces`
- `logicConflictDetection`
- `logicRefreshDueKnowledgeDomains`
- `logicReleaseWorkspaceOnPlanTerminal`
- `logicKillSwitchSuspendsRunningPlans`
- `logicWorkerInvocationRetentionSweep`
- `logicRegisterNode`, `logicDeregisterNode`, `logicBootstrapCluster`

Design sketch: invoke Logic functions through the same step-runner
the automation scheduler uses (`component/automations/steps/`).
Bind step IDs as local variables visible to later steps; the
`_return` step's evaluated value is the function's return. The
glue lives somewhere between the function dispatcher
(`engine.executeMutationFunctionCall` and the query evaluator's
function-call expansion) and the automation step package.

### F.6 -- Logic-calls-mutation dispatch _(LANDED)_

`resolvePlanFunctions` (`component/memql/function_validator.go`)
detects the case where a top-level call resolves to a Logic whose
return expression is a mutation call, and hoists the inner
mutation call into `plan.MutationCall` (with caller args
substituted into the mutation's args map). The engine's existing
`executeMutationFunctionCall` path runs it. The `function_validator.go:325`
"function X is a mutation and cannot be used inside query
expressions" guard remains in place for the general case --
this hoist intercepts the Logic-leaf pattern before that guard
fires. Regression coverage:
`TestResolvePlanFunctions_LogicReturningMutationDispatches`
and `..._SubstitutesArgs` in `mutation_functions_test.go`.

`logicBootstrapDefaultPartition` and the identity logics that
forward to a mutation now run end-to-end.

### F.7 -- Documentation cleanup

When F.1-F.3 land, the legacy text in `CLAUDE.md`
(lines ~966-976 -- "Procedural form. The legacy
`func (...) NAME(ctx any) (any, error)`...") and
`docs/core/memql-authoring-rules.md` (lines ~860-868 --
"Procedural form (legacy escape hatch)...") should be deleted, not
softened. There's no escape hatch once the rewriter stops emitting
the form -- the parser will reject it.

---

## Why this matters

Until F.1-F.4 land, the system has two ways to express "return a
value from a function body" and they're not interchangeable:

- Author-facing: `return <expr>` in Logic bodies (new, clean).
- Internal: `ctx.output = <expr>; return ctx, nil` everywhere else.

The transitional split is visible in error messages, in the
rewriter's emitted text, and in every non-Logic `.memql` file. New
DSL authors hit this immediately. Closing the loop removes a
load-bearing piece of confusion.

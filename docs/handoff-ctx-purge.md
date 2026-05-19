# ctx-envelope purge — handoff for follow-up sessions

> **Status:** F.1, F.2, F.3, F.4, F.6, F.7 all landed via
> `feature/dsl-engine-ctx-purge-e2e`. F.5 (multi-step Logic via the
> automation step runner) is the only remaining piece -- it's a
> structural change, not cleanup, and needs its own PR with care
> taken around the cross-package interface between the engine's
> function dispatcher and the automation step runner.

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

### F.2 -- Parser: drop the ctx.output body parsers _(LANDED)_

`parseGoStyleCtxOutputBody`, `parseGoStyleCtxOutputMutationBody`,
`parseGoStyleCtxOutputAutomationAssignment`, and
`looksLikeCtxOutputBody` are deleted. The `parseGoStyleQueryBodyOrLegacy`
and `parseGoStyleMutationBodyOrLegacy` dual-form switches now go
straight to the `return <expr>` parsers; the Policy receiver case
delegates to `parseGoStyleSpecBody`. The automation-body loop's
ctx-output branch is gone.

### F.3 -- Executor: resolve `args.X` without the ctx envelope _(LANDED)_

The engine parser (`component/memql/parser.go`), the mutation-template
parser (`component/memql/mutation_templates.go`), and the policy
field-ref resolvers (`policy_evaluator.go`, `policy_function_loader.go`)
all accept `args.X` as a first-class caller-arg reference. Both
`args.X` and `ctx.X` produce the same `ArgReference` AST node so the
rest of the validator / executor pipeline stays single-shape.

Regression coverage: `component/memql/args_ref_test.go` pins the
parser path; the existing `caller_ref_test.go` shape sits next to it.

### F.4 -- Strip `ctx.X` from every remaining .memql _(LANDED)_

Every `.memql` file that carried `ctx.output = <expr>; return ctx, nil`
has been converted to `return <expr>, nil`. The `func (Kind)` headers
keep the `(ctx any)` parameter -- it's a placeholder identifier the
validator only type-checks, and renaming to `_` / `args` is cosmetic
churn we left for a future migration that also converts the bodies
to struct form.

Files touched (mechanical sweep, single commit on the F.4 branch):
cluster, cognition, common, data, identity, memql, planner, platform,
policies, router, workbench, worker logic + queries plus the two
`_reference/` doc files.

### F.5 -- Multi-step Logic execution _(OPEN -- needs its own PR)_

This is the one piece of the purge that's still pending. The function
loader's Logic case rejects multi-step bodies with an actionable error
pointing at this handoff:

```
function "X": multi-step logic bodies are not yet executable by the
function dispatcher (N intermediate steps before `_return`). F.5 of
the ctx-envelope purge tracks the step-runner integration; until
then, restructure the body as a single `return <expr>` statement
or move the multi-step orchestration into an automation that calls
single-step logics
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

**Design sketch.** Invoke Logic functions through the same step
runner the automation scheduler uses (`component/automations/steps/`).
The cross-package shape that minimises coupling:

1. Define a `LogicRunner` interface in `component/memql/` with a
   single method `RunSteps(ctx, args, *AutomationDef) (any, error)`.
2. Engine has a `SetLogicRunner(LogicRunner)` setter; the Logic call
   path (function loader stores the `*AutomationDef` on the
   Function, dispatcher checks `logicRunner != nil` before falling
   back to the single-step `fn.Expr` path) delegates when set.
3. Implement `LogicRunner` in `component/automations/` (new file)
   as a thin wrapper that builds a minimal `StepContext` with the
   existing `Evaluator`, seeds `args` as a custom variable, and
   walks the steps via `Registry.Execute` -- skipping the heavy
   automation Executor machinery (concurrency limits, dedup,
   execution-storm detection, retention, event publishing). The
   `_return` step's evaluated value is the Logic's return.
4. Wire at app bootstrap (`app/integrations.go` or the closest
   phase that already has both the engine and the step registry).

What makes this F.5's-own-PR-worthy rather than fitting in the
cleanup pass:

- Substituting step references (`getGA.First()`, `getGA.Empty()`,
  `expired.Nodes()`, `candidates.Len()`) in subsequent step args
  needs the automation Evaluator's expression resolution path --
  the simple ArgReference substitution used by F.6 isn't enough.
- forEach + switch + parallel step shapes appear in real logics
  (`logicPurgeExpiredArchivedSpaces` iterates `expired.Nodes()`;
  `logicKillSwitchSuspendsRunningPlans` iterates `plans.Nodes()`)
  and must Just Work via the existing executors.
- Logic invocations should NOT trigger automation lifecycle events
  (started/completed/failed), persist execution rows, or burn
  concurrency slots -- so the wrapper bypasses
  `Executor.ExecuteWithEvent` and orchestrates the per-step calls
  directly.

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

### F.7 -- Documentation cleanup _(LANDED)_

The legacy "Procedural form" paragraphs in `CLAUDE.md` and
`docs/core/memql-authoring-rules.md` are rewritten to describe the
canonical post-rewrite shape (`return <expr>, nil`, no `ctx.output`
boilerplate) and the dual `args.X` / `ctx.X` parser recognition.
The "in flight -- see handoff" pointer is removed since this handoff
now tracks only F.5.

---

## Why this matters

Closing F.5 removes the last reason to handle anything but
`return <expr>` in DSL bodies. After F.1-F.4 + F.7 landed, the
transitional dual-form text is gone from the codebase + docs;
multi-step Logic is the only remaining reason an author would still
hit a confusing error message.

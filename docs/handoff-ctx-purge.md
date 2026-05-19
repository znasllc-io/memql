# ctx-envelope purge — handoff doc (closed out)

> **Status:** F.1 through F.7 all landed via
> `feature/dsl-engine-ctx-purge-e2e`. The DSL engine has a single
> canonical body shape (`return <expr>, nil`), multi-step Logic
> dispatches through the automation step runner, and the docs
> describe the as-shipped behaviour. This file is kept as the
> historical record of the migration; it can be deleted once the
> repo has shipped a release with the purged form.

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

### F.5 -- Multi-step Logic execution _(LANDED)_

The engine carries a `LogicRunner` interface
(`component/memql/engine.go`); when wired, top-level calls to a
Logic function whose body has intermediate `name := <call>` steps
delegate to it instead of evaluating just the `_return`
expression. `resolvePlanFunctions` hoists the call into
`plan.LogicCall` (analogous to `plan.MutationCall` from F.6) so
the engine's Execute dispatches via `executeLogicFunctionCall`.

The runner lives at `component/automations/logic_runner.go`. It
translates the parsed `*AutomationDef` body through the existing
`compiler` + JSON-loader path so the resulting runtime steps
arrive in topological dependency order with `Condition` strings,
step-reference rewrites, helper-builtin recognition, and the
canonical `_return` step shape already baked in. Then it walks
the steps via `steps.Registry.Execute`, binding each result on a
fresh `Evaluator` so later steps + the `_return` expression can
reference them.

**Isolation properties.** Logic invocations do NOT publish
automation lifecycle events, persist an execution row, burn a
concurrency slot, participate in dedup / storm detection, or
trigger sub-automations. The runner bypasses
`Executor.ExecuteWithEvent` entirely; only the per-step registry
and the evaluator are reused.

**Caller args.** Args are seeded under three spellings --
`$args.X` (author-facing), `$ctx.input.X` (legacy runtime form),
and `$event.X` (when args carries an `event` key, matching how
graph-event-triggered logics see the trigger payload). The
existing function-step arg resolver picks up whichever form the
compiled step body references.

**Wiring.** `app/engine.go` calls
`engine.SetLogicRunner(automations.NewLogicRunner(engine, stepRegistry, logger))`
once the step registry is built. Stripped-down binaries that
omit the automations package keep the engine's "no LogicRunner
wired" error path; single-step Logic dispatch continues to work
through the standard `fn.Expr` path either way.

Regression coverage:
`component/automations/logic_runner_test.go` exercises the
compile-body path, the conditional-step shape, the multi-spelling
arg seeding, and the nil-body defensive check.

Logics now unblocked at load time:
`logicAutoJoinSI`, `logicBootstrapSession`, `logicGenerateResponse`,
`logicPurgeExpiredArchivedSpaces`, `logicVoiceMigrationOnSecondHuman`,
`logicAccessRequestExpirySweep`,
`logicAccountDeletionReminder{7,25}Days`, `logicAccountDeletionSweep`,
`logicAuditEventRetentionSweep`, `logicMagicLinkExpirySweep`,
`logicOnDelegationCreated`, `logicProvisionPersonalPartitionOnFirstLogin`,
`logicRevokeExpiredDelegations`, `logicPurgeExpiredPolicyTraces`,
`logicConflictDetection`, `logicRefreshDueKnowledgeDomains`,
`logicReleaseWorkspaceOnPlanTerminal`, `logicKillSwitchSuspendsRunningPlans`,
`logicWorkerInvocationRetentionSweep`, `logicRegisterNode`,
`logicDeregisterNode`, `logicBootstrapCluster`.

End-to-end smoke-test on a running cluster (Cluster bootstrap
mutations + cognition automations firing on space creation) is
the recommended next verification step before declaring the
migration fully shipped.

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

With F.5 landed the DSL has one canonical body shape
(`return <expr>` / `return <expr>, nil`) regardless of receiver
kind or step count. The transitional dual-form text is gone from
the codebase + docs; multi-step Logic dispatches through the same
step registry the automation scheduler uses. Author surface and
runtime shape now line up.

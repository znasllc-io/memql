# Automations + Logic refactor — handoff for follow-up session

> **Status:** Phases A → E shipped to `origin/main`. Tip: `43ce563`.
> This doc captures the design state and the open follow-up work for
> the next session.

---

## What landed

| Commit | Phase | What |
|---|---|---|
| `e5cd0cf` | A + B + C | `logic` construct + `automation NAME { step ... }` struct form + first migrated automation (`expireDelegations`) |
| `5efe049` | D | All 34 remaining automations migrated. 35 logic files now live under `dsl/v1/logic/v1/<domain>/`. Zero `func (Automation)` declarations remain in author files. |
| `43ce563` | E | Author-facing `func (Automation) NAME(...)` rejected at load time with a migration hint. Internal parse path still alive (rewriter feeds it). |

### Design recap (read before resuming)

Two constructs with sharply different jobs:

- **`automation`** — declarative orchestration. A trigger + a flat list of named steps. No control flow.
- **`logic`** — imperative procedure with `args { ... }` + `body { ... }`. Reusable. `if` / `for` / mutation + query calls live here.

Step body grammar (strict, one call per step):

```memql
step <stepName> {
  <kind> <bareName> { <args-object> }
}
```

`<kind>` is one of `logic` / `mutation` / `query`. The rewriter binds
`<kind> <bareName>` to the registry name `<kind><PascalName>`. So
`logic revokeExpired { now: timestamp() }` calls the function named
`logicRevokeExpired`. `publishEvent { ... }` works without a kind
keyword. `(...)` parenthesised form passes through unchanged.

`event.X` (or bare `event`) in step bodies translates to `ctx.input.X`
(or `ctx.input`) — the legacy trigger envelope.

Step output references via bare `<priorStep>.<field>` — the dotted
suffix rides through the rewriter verbatim.

---

## Open follow-up work

These didn't fit Phase D's "uniform single-step shell" sweep. Each
benefits from hand-curation rather than a script.

### F.1 — Refactor multi-step automations into multiple named logic blocks

Phase D was deliberately mechanical: every automation became
`automation NAME { step run { logic NAME { event: event } } }`. The
imperative body lifted verbatim into one logic block.

That's the safe landing pad, not the end state. Several automations
have natural multi-step shape that should be teased apart:

| Automation | Current logic block | Proposal |
|---|---|---|
| `autoJoinSI` (~89 body lines) | one big block | three logic blocks: `resolveCreatorAgents` (query GA + selected specialists), `joinAgentsToSpace` (iterate + insert participants), `emitObservabilityEvent` (publish "si.auto-joined"). Automation becomes 3 steps. |
| `seedWelcomeCurriculum` (~108) | one block | several phases: lookup, plan, batch-create. |
| `provisionGeneralAssistant` (~126) | one block | already touches several concepts — split per concept. |
| `bootstrapCluster` (~37) | one block | cluster vs. database vs. identityProvider — three steps. |
| `handleRefinementPlan` (~67) | one block | classify, refine, persist. |
| `voiceMigrationOnSecondHuman` (~45) | one block | detection + migration steps. |
| `generateResponse` (~46) | one block | input-shape + agent-dispatch + persist. |

Pattern: identify "phases" by looking at where the body branches on
prior outputs, then extract each phase into a `logic <verbPhrase>`
block. The automation's step names then read as a recipe.

**No urgency.** The mechanical migration works fine at runtime.
This is an authoring-clarity follow-up.

### F.2 — `@onlyIf` step gating? Re-examine.

In the original design conversation I leaned against `@onlyIf(spec)`
on steps; the conditional should live inside the logic block. After
seeing Phase D's output, this still holds: every conditional in
every automation body migrated cleanly to live inside the logic
body. No automation needed `@onlyIf` to read well.

Decision so far: keep step bodies strict. Don't add `@onlyIf` unless
F.1's hand-refactors surface a real need.

### F.3 — Step output binding sugar

Currently step output refs are bare `<priorStep>.<field>` and ride
through the rewriter as identifiers. The legacy step extractor
walks the rewritten body and resolves them via the dependency
graph. This works but the engine-level resolution is approximate
(only `.metadata` suffixes are handled by the bare-step rewriter
according to comments in `autoJoinSI`).

If the F.1 refactors produce multi-step automations with rich
output references, this resolution may need teaching. The current
single-step automations don't exercise it.

### F.4 — Logic-to-logic composition test coverage

The design supports a logic body calling other logic blocks:

```memql
logic outer {
  args { spaceId string @required }
  body {
    inner := logicInner({ spaceId: args.spaceId })
    return inner
  }
}
```

No DSL file currently exercises this. The rewriter + runtime
should handle it (logic blocks register as regular functions; a
call to `logicInner(...)` resolves like any other function call).
Worth adding a test case under
`component/language/parser/logic_automation_rewrite_test.go`.

### F.5 — Description quality on migrated logic blocks

The Phase D migration script auto-generated `@description` text on
every logic block:

```
@description("Imperative body of automation foo (migrated from the legacy func form).")
```

That's a placeholder. Each logic block deserves a real description
that explains what the procedure does, in terms of inputs +
side-effects. A sweep with per-block hand-written descriptions
would dramatically improve the help text agents and authors see.

### F.6 — Step-body `parallel` / `forEach` kinds

The existing step-runtime supports `parallel`, `foreach`, `switch`
step types. The new struct form doesn't surface them yet — every
step is implicitly sequential. If a real automation needs parallel
fan-out, we'd add `step <name> parallel { ... }` or
`step <name> foreach { ... }` as explicit kinds. Defer until a
real automation asks for it (most can be expressed inside a logic
body's `for` / `parallel(...)` today).

---

## Files to know

| Path | What |
|---|---|
| `component/language/parser/logic_rewrite.go` | `logic NAME { args { } body { } }` -> `func (Logic) NAME(...) (any, error) { body }` rewriter. |
| `component/language/parser/automation_rewrite.go` | `automation NAME { step ... }` -> `func (Automation) NAME(ctx any) { name := <call>; ...; return ctx, nil }` rewriter. Also: `LooksLikeLegacyAutomation` for loader rejection. |
| `component/language/parser/logic_automation_rewrite_test.go` | Locks rewriter behaviour. Four cases: basic logic, step single-call, multi-step + output refs, legacy-form detection. |
| `dsl/v1/logic/` | New embed package. Mirror shape of `dsl/v1/queries/` / `dsl/v1/mutations/`. |
| `dsl/v1/logic/v1/<domain>/<name>.memql` | 35 logic files. The verb-shaped procedures. |
| `dsl/v1/automations/v1/<domain>/<name>/automation.memql` | 35 automations in struct form. |
| `component/memql/function_loader.go` | Walks `logicfs.Source()` alongside queries + mutations. |
| `component/automations/loader.go` | `parseResolveCompile` runs the rewriters + rejects legacy author input. |
| `component/language/compiler/api.go` | `CompileSource` / `ValidateMemQL` run the rewriters too. NO legacy rejection here -- tests use these entry points with legacy fixtures. |

---

## Ground rules (durable, from prior memory)

- **Commit + push directly to `main` on both repos.** No feature branches. Bypass-protection warnings expected.
- **No backwards-compat shims.** Pre-release. Rename or delete cleanly + fix callers in one commit.
- **`git add <explicit-path>` only.** Never `-A` or `.`.
- **Commit messages via `/tmp` + `git commit -F /tmp/msg.txt`.** Heredoc breaks on colons + quotes.
- **No emojis** anywhere (code, commits, docs, replies).
- **Backend = "SI"**, frontend = "AI".
- **Don't run CoPresent preview server** — the user does `npm run dev` themselves.
- **You troubleshoot; user pastes log output.** Don't ask them to grep / psql / navigate UI.

---

## Sanity check before resuming

```bash
cd /Users/znas/projects/memql
git log --oneline -5
# top entry should be: 43ce563 dsl: retire the `func (Automation)` author surface (Phase E)

grep -rn '^func (Automation)' dsl/v1/automations 2>&1 | wc -l
# expected: 0

go test ./... -count=1 2>&1 | grep -E 'FAIL'
# expected: empty
```

If anything diverges, STOP and reconcile before touching new code.

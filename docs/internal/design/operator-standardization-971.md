---
title: DSL operator standardization — DESIGN (#971)
audience: internal
status: historical
area: design
sinceVersion: 0.9.0
owner: znas
---

# DSL operator standardization — DESIGN (#971)

> Historical: shipped in 0.9.0; kept for rationale.

Status: APPROVED ARCHITECTURE; build not started. Decisions locked with the code
owner. Phased child issues #972-#978 carry the build, low-risk first.

Scoped out of the syntax audit (#964). Goal: collapse the MemQL operator surface
onto Go conventions so there is **one boolean-expression grammar** across every
context — and so the planner-authoring LLM (#954) has a single, consistent syntax
to emit.

## Problem (from the audit)

Comparisons are already Go (`==` `!=` `<` `<=` `>` `>=`). The logical-connective
surface is fragmented — three forms coexist and two are broken:

| Context | AND | OR | Status |
|---|---|---|---|
| filter (queries) | `;` (separator) | — (no OR; two-query workaround) | works, RSQL-style |
| spec | `&&` | `\|\|` | `\|\|` does not tokenize → spec silently fails to load |
| logic `if` | `&&` and word `and` | word `or` | word forms silently misevaluate at runtime |

Plus: `?.` optional-chain (deprecated, still used), `in`/`has`/`not in` keyword
operators (`has` is reversed `in`), and dead prefix-call builtins (`and()`,
`or()`, `gt()`, …) with zero call sites.

## Locked decisions

### 1. One Go boolean-expression grammar, everywhere

Filters, specs, traits, logic `if`-conditions, and `cond` predicates all use the
**same** expression grammar:

- Logical: `&&` (and), `||` (or), `!` (not).
- Precedence: `!` > comparisons > `&&` > `||`; parens `( )` to override (Go's).
- Retire `;`-AND, `,`-OR, and the infix words `and`/`or`.

```
filter: payload.stage == "won" || payload.score >= 90
spec:   actor.role == "admin" || actor.role == "owner"
logic:  if a && (b || !c) { ... }
```

> **What actually shipped, recorded against the decision above**
> (memql#3630). The decision text is left as it was written; these two
> parts of it were never implemented the way they read.
>
> **`!` reaches only ONE of the five surfaces named above.** It lexes and
> parses everywhere, and `convertNotExpr` then rejects every target it is
> handed — with the `#2542` expression-led scope error in filters, specs
> and traits, and with `NOT/! does not convert; write the != comparison
> form` in logic `return` expressions and collection lambdas. What does
> work is the `logic: if ...` line of the example: a logic `if`, an
> automation `Step.Condition`, a trigger `@filter` and a `cond` predicate
> are all evaluated as CONDITION STRINGS by
> `component/automations/evaluator.go`, which honours a leading `!`.
> `dsl/data/logic.memql:41` is the corpus's single use. So "the same
> expression grammar, everywhere" is not what the two evaluators
> implement, and the precedence line reads as a specification for
> something a query has never been able to use.
>
> **"Retire" turned out to mean an authoring convention enforced by a TEXT
> SCAN** (`TestNoRetiredOperatorForms`), not a parser rejection. `;`, `,`,
> `has` and `?.` all still parse and load, and the engine still computes
> `;` as AND and `,` as OR — which is why the parenthesised `,` was an
> authorization bypass closed in the scanner (memql#3612) and why a
> runtime-mounted product bundle is outside the gate entirely
> (memql#3629). `has` additionally cannot be removed: it is the
> desugaring target of `in` (#976).

**Filters become true boolean expressions** (today a filter is a flat
`;`-separated AND-list compiled straight to SQL `WHERE`). This is the largest
piece of work and the largest feature win — filters gain OR, and filters + specs
converge on one grammar.

### 2. OR in SQL pushdown is tractable (no split-evaluation)

The row/context boundary looked like the hard part but isn't: `actor.*` values
are **constants at query time** (resolved auth envelope), so a mixed predicate
like `payload.stage=="won" || actor.role=="admin"` pushes to SQL with the actor
term **bound as a query parameter** (`... OR $1='admin'`). The real work is the
SQL codegen (OR + parenthesization + precedence + constant-binding), not a change
to the evaluation model.

### 3. `when(arg){}` replaces `?.` for arg-conditional filtering

`?.payload.x == args.y` today means "apply this predicate only if `args.y` is
present." That implicit drop is **ambiguous under OR** (dropping a term from
`a || b` silently changes the result). Replacement:

```
filter when(args.groupId) { args.groupId in payload.groupIds }
       && payload.stage == args.stage
```

**Semantics: syntactic drop** — if the guard arg is absent, the guarded block AND
its connective are removed as if never written. Unambiguous in any position,
including inside `||`; self-documenting; an LLM author cannot misuse it.

### 4. Single `in` membership operator

Go has no membership operator, so this is a deliberate DSL extension; keep it
lean. Standardize on `<scalar> in <collection>` for both list-literals and
collection fields; retire `has` (reversed `in`); negate with `!(x in y)`.

```
payload.kind in ["meeting", "reminder"]
args.groupId in payload.groupIds          // was: payload.groupIds has args.groupId
!(payload.stage in ["won", "lost"])
```

### 5. Keep / remove

- Keep `cond(pred, a, b)` (Go has no ternary). Keep `if`, `for … := range`, `:=`
  for control flow in logic/automation.
- Remove the dead prefix-call builtins (`and()`/`or()`/`not()`/`eq`/`lt`/`gt`/…).
- Comparisons + `== nil` / `!= nil` unchanged (already Go).

## Live bugs fixed along the way

- `||` does not tokenize → specs with `||` silently fail to load
  (`dsl/deployment/specs.memql` `requiresOwnerOrAdmin`, plus `_reference`).
- Infix `and`/`or` in logic conditions silently misevaluate (the runtime
  normalizer rewrites only `&&`/`||`).

## Phased build (low-risk first) — child issues

| Issue | Phase | Blocked by |
|---|---|---|
| #972 (A) | Unified Go expression grammar (`||` token + `!` + precedence + parens; one parser across contexts). Fixes the `||` silent-load bug. | — (foundation) |
| #973 (B) | Fix logic word `and`/`or` runtime miseval (migrate to `&&`/`||`; remove the word path). | #972 |
| #974 (C) | OR in SQL pushdown (WHERE OR/parens/precedence + actor-constant binding; unlocks `||` in filters). | #972 |
| #975 (D) | `when(arg){}` guard + retire `?.`. | #972, #974 |
| #976 (E) | Collapse `has` → `in`. | #972 |
| #977 (F) | Tree-wide codemod (`;`→`&&`, `has`→`in`, `?.`→`when`) + conformance test rejecting retired forms + docs/skeletons. | #974, #975, #976 |
| #978 (G) | Remove dead prefix-call builtins. | — (independent quick win) |

```
        #972 A (foundation)
       /   |    |     \
  #973 B #974 C #976 E \
          |  \    |     \
       #975 D \   |      \
          \    \  |       \
           \    #977 F ────┘   #978 G (independent)
```

## Relationship to other work

- Audit #964 surfaced these issues; this epic is its first standardization
  workstream. Siblings (separate efforts): insert/update concept-restatement,
  annotation cleanup, the dead policy-tier loader, doc-vs-code inversions.
- Feeds #954 (planner-authored automations): a single, consistent grammar is
  what the authoring LLM emits and what the dry-run validates.

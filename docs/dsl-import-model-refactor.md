# DSL Import-Model Refactor — shipped

> **Status:** Shipped 2026-05-19 via memql PRs #47 / #48 / #49.

The canonical post-migration shape is the only one the engine accepts:

- **File-top Form B imports:** `use <dotted.path>.{ name1, name2 }`
  where the dotted path maps to `dsl/<a>/<b>.memql` and the
  brace-list names the constructs pulled into local scope.
- **Concept binding in the signature:** `query <Concept> <name>`,
  `mutation <Concept> <name>`, `shape <Concept> <name>`,
  `seed <Concept> <name>`.
- **No `@use*` annotations.** The legacy `@useConcept`, `@useShape`,
  `@useQuery`, `@useMutation`, `@useLogic`, `@useBuiltin` family is
  rejected at parse time with migration-pointing errors.
- **No Form A `use <ns>.<concept>` clauses.** The legacy
  single-binding shape (and the `as <alias>` surface that went with
  it) is rejected at parse time.
- **Seed cleanup:** seeds shed `@scope("global")` (default),
  `@version(...)` (vestigial), `id:` body field (auto-derived from
  the seed name for global seeds), and `description:` body field
  (use the `@description` annotation).

## Authoring example

```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord }

@description("Get space participants")
query participant querySpaceParticipants {
  args   { spaceId string @required }
  filter payload.spaceId == args.spaceId; traitIsActiveRecord
  shape  participantFull
}
```

## Where to look

- Authoring reference: `dsl/_reference/_shape.memql`,
  `dsl/_reference/_spec.memql`, `dsl/_reference/_trait.memql`,
  `dsl/_reference/_concept.memql`.
- Language reference: `docs/core/memql.md`,
  `docs/core/memql-functions.md`,
  `docs/core/memql-naming-conventions.md`.
- Migration tooling (kept for posterity / future kinds):
  `scripts/dsl-imports/build_index.py`,
  `scripts/dsl-imports/migrate_seeds.py`,
  `scripts/dsl-imports/migrate_use_annotations.py`.

## Earlier design notes

The original locked design (Variant A — Go-style file imports with
aliasing, `import "./path" as cog` + `cog.participant` references)
landed parser machinery but never finished the tree migration. PR
#47 pivoted to Form B (`use <module>.{ names }` + bare references
after import); PR #48 migrated the tree; PR #49 locked down the
legacy forms. The Variant A `ImportDecl` AST node and its
surrounding tests are retained in the parser as dead code for now
and can be cleaned up in a follow-up.

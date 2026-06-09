---
title: MemQL Naming Conventions
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Naming Conventions

> Last Updated: February 10, 2026

## Function Prefixes

Use receiver-specific prefixes so intent is obvious at call sites and in diffs.

- Query functions: `query*`
- Mutation functions: `mutation*`
- Spec functions: `spec*`
- Automation functions: use verb-first names (for example `bootstrapSession`)

Examples:

```memql
use identity.concepts.{ user }
use identity.shapes.{ userFull }

query user queryUserById {
  args {
    userId  string  @required
  }
  filter  id == args.userId
  shape   userFull
}

mutation user mutationArchiveUser {
  args {
    userId  string  @required
  }
  update {
    id:     args.userId
    status: "archived"
  }
}

spec specStatusIsActive {
  payload.status == "active"
}
```

## Why This Matters

- Improves readability in automations with many calls.
- Makes CQS intent visible before compile-time checks.
- Reduces naming collisions when files are grouped by domain.

## Enforcement

Compiler lint emits non-fatal warnings for naming mismatches:

- `naming.query-prefix`
- `naming.mutation-prefix`
- `naming.spec-prefix`

Warnings can be promoted to errors using strict compile settings.

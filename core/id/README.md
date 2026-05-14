# contentid

Deterministic, content-addressable ID generation for MemQL.

## Overview

This package generates reproducible IDs from content using SHA256. Given the same input, it always produces the same ID - making inserts idempotent and enabling natural deduplication.

## Core Axioms

The ID generation satisfies three mathematical properties:

1. **Deterministic**: Same content always produces the same ID
2. **Commutative**: `Combine(a, b) == Combine(b, a)`
3. **Idempotent**: `Combine(a, a) == a`

## Usage

### Basic ID Generation

```go
engine := contentid.New()

// From a string
id := engine.FromString("hello world")

// From a map (keys are sorted for determinism)
id, err := engine.FromMap(map[string]any{
    "name": "John",
    "email": "john@example.com",
})

// Combine two IDs
combined := engine.Combine(id1, id2)
```

### How MemQL Uses This

When inserting a record without an explicit `id`, MemQL derives the ID from:

```go
input := map[string]any{
    "concept": "v1:lead",           // concept name
    "payload": payload,              // the record payload
    "salt":    "optional-salt",      // deployment-specific salt (if configured)
}
id := engine.MustFromMap(input)
```

This produces a 64-character hexadecimal SHA256 hash.

## Deterministic JSON

The `Marshal` function produces deterministic JSON output:

- Keys are sorted alphabetically
- Whitespace is minimized
- Identical data always produces identical bytes

This is critical for content-addressing - without deterministic serialization, the same logical data could produce different IDs.

## Chaining

For ordered sequences where each state depends on the previous:

```go
chain := contentid.NewChain()
id, _ := chain.Next(engine.FromString("action 1"), engine)
chain.Advance(id)
id, _ = chain.Next(engine.FromString("action 2"), engine)
```

## Configuration

The salt is configured via environment variable:

```
MEMORY_NODES_VISIONARYS_LAB_CONTENTID_SALT=your-deployment-salt
```

Different salts produce different IDs for the same payload, enabling environment isolation.

## See Also

- [`docs/memql.md`](../docs/memql.md) - MemQL query language reference, including content-addressed insert syntax

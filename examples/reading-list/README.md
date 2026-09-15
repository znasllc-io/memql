# Reading list

A standalone `.memql` example with a caller-owned concept, mutation, query, and
tool. Follow [Your first MemQL program](../../docs/public/language/first-program.md)
for validation and cluster execution. It is not part of the engine's seeded DSL.

From the repository root:

```bash
go run ./cmd/memqllint examples/reading-list/reading.memql
```

Lint is offline. Running the mutation against a cluster writes a real row;
saving the file does not deploy it.

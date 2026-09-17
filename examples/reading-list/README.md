# Reading list

A standalone `.memql` example with a caller-owned concept, mutation, query, and
tool. Follow [Your first MemQL program](../../docs/public/language/first-program.md)
for validation and cluster execution. It is not part of the engine's seeded DSL.

From the repository root:

```bash
go run ./cmd/memqllint examples/reading-list
```

Keep `namespace.pin` and `memql.toml` beside the source; they preserve its
namespace and language edition. Directory-mode lint is offline. Running the mutation against a cluster writes a real row;
saving the file does not deploy it.

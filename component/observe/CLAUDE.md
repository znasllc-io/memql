# Observe Runtime

**Purpose:** Per-invocation instrumentation surface. Captures (FQN, duration, args, result, error, trace_id) per call and writes them to TimescaleDB. Joins back to the static architecture model by FQN.

**Companion static map:** `component/architecture/` -- defines the FQN format, embeds the `topology.model.json` the cockpit reads, parses the `//memql:observe` source markers that feed this runtime's per-FQN levels.

**On-disk surface:**
- TimescaleDB hypertable `code_invocation` (migration `20260515000000_observability_hypertable.up.sql`)
- Continuous aggregates `code_invocation_1m` and `code_invocation_1h`
- Concepts under `dsl/observability/`: `codeProfile`, `invocation`, `codeMetric`

---

## How it composes

```
                                +--------------------------+
                                | //memql:observe <level>  |  source marker
                                +----------+---------------+
                                           |  (parsed by extract/observe_markers.go)
                                           v
                                +--------------------------+
                                | topology.model.json      |  static map (embedded)
                                |   Method.Attrs[          |
                                |     observe_level,       |
                                |     redact_args,         |
                                |     observable           |
                                |   ]                      |
                                +----------+---------------+
                                           |
                                           v
+---------------------+        +-----------+--------------+        +---------------------+
| MEMQL_OBSERVE_LEVEL +------->| observe.DefaultLevel     |<-------+ codeProfile concept |
| (.env override)     |        +-----------+--------------+        | (live override)     |
+---------------------+                    |                       +----------+----------+
                                           |                                  |
                                           v                                  v
                                +--------------------------+        +---------------------+
                                | observe.Method(ctx, fqn) |        | CodeProfileSubscriber|
                                |   .Args(...).End(&err)   |        |  graph.node.created  |
                                +-----------+--------------+        |  ...codeProfile      |
                                            |                       +----------+----------+
                                            v                                  |
                                +--------------------------+                   |
                                | TimescaleSink            |<------------------+
                                |   buffered, drop-on-full |
                                +-----------+--------------+
                                            |
                                            v
                                +--------------------------+
                                | code_invocation hypertable
                                |  + 1m / 1h continuous aggs |
                                +--------------------------+
```

---

## Level resolution at call time

```
observe.Method(ctx, fqn):
  level := lookupProfile(fqn)        // per-FQN override from codeProfile CDC
  if !found:
    level := DefaultLevel()          // process-wide, set from MEMQL_OBSERVE_LEVEL
  if level == LevelOff:
    return Invocation{}              // zero-cost off-path
```

The off-path is intentionally tiny: one map lookup + one comparison + a stack-allocated zero-value return. No allocations, no channels, no syscalls. This matters because the instrumentation gets pasted on every Method in critical paths.

---

## Levels

| Level | Captures | Cost | Default env |
|---|---|---|---|
| `off` | nothing | 0 | prod |
| `count` | count, duration, error | ~ns | always-safe baseline |
| `meta` | + arg names, types, sizes (no values) | low | UAT default |
| `verbose` | + full arg + return values (subject to redaction) | meaningful | dev default, on-demand in prod |

---

## Redaction

Two complementary mechanisms; both default to "redact rather than leak":

1. **Name pattern.** Any captured arg whose name matches `/(?i)pass|token|secret|key|auth|credential/` is rendered as `<redacted>` regardless of level. Defined in `helper.go:secretNamePattern`.
2. **Source marker.** `//memql:observe verbose redact=name1,name2` lists the args you explicitly *trust* to capture verbatim -- the inverse of the auto-redact set. The static extractor stamps the list on the Method node's `redact_args` attr; the runtime sink (forthcoming completeness pass) consults it before serializing.

When in doubt, the runtime over-redacts. Recovering a false positive ("a non-sensitive var named `key`") is a one-line marker; recovering a leaked secret is a security incident.

---

## Files

| File | What |
|---|---|
| `observe.go` | Level enum, ParseLevel, DefaultLevel, env init, sink registry, trace extractor seam |
| `helper.go` | `Method() / Func()`, `Invocation.Args() / Result() / End()`, redact-by-name, metaShape |
| `record.go` | Record + Arg / RedactedArg constructors |
| `sink.go` | Sink interface + default slog sink |
| `timescale_sink.go` | TimescaleSink: buffered, drop-on-full, batched bun inserts |
| `dependency.go` | SinkComponent: app/database.go phase wiring; ComponentName, Order, Ready |
| `profile_cache.go` | Per-FQN cache: SetProfile / ClearProfile / SweepExpiredProfiles / lookupProfile |
| `codeprofile_subscriber.go` | events.Bus subscriber on the codeProfile concept; expiry sweeper |

---

## Lifecycle

The two Dependency wrappers register in different phases:

- `SinkComponent` (Order 50) is wired in `app/database.go`. It needs `*bun.DB`, so it starts after Phase 2.
- `CodeProfileSubscriber` (Order 10) is wired in `app/engine.go`. It only needs `*events.Bus`, which the engine phase creates.

Stop drains the buffer (with deadline) and reinstalls the default slog sink so any in-flight callers writing after shutdown don't NPE.

---

## How to use

### From a function you want to instrument

```go
//memql:observe verbose redact=password
func (h *Handler) Login(ctx context.Context, user, password string) (err error) {
    defer observe.Method(ctx, "method:github.com/znasllc-io/memql/component/auth.(*Handler).Login").
        Args(observe.Arg("user", user), observe.Arg("password", password)).
        End(&err)
    ...
}
```

The FQN must match the Method node's ID in `topology.model.json`. The model's `model.MethodID(pkgPath, recvType, methodName)` is the authoritative constructor; the format is documented in `component/architecture/model/model.go`.

### Tune from a developer's machine

```bash
echo 'MEMQL_OBSERVE_LEVEL=verbose' >> ~/projects/memql/.env
```

That bumps the process-wide default. Per-FQN tuning goes through the codeProfile concept (insert/upsert from the cockpit drill-down, the engine, or any tool that can write concept rows). The CDC subscriber picks it up within the events-bus latency.

### Connect a trace ID

```go
observe.SetTraceExtractor(func(ctx context.Context) (string, string) {
    span := trace.SpanContextFromContext(ctx)
    return span.TraceID().String(), span.SpanID().String()
})
```

The trace ID lands on every captured Record so the cockpit (or any external trace viewer) can cross-link an invocation to its distributed span.

---

## What's deliberately out of scope here

- **The static IR.** Lives in `component/architecture/`. This runtime references its IDs but does not depend on the model package at runtime.
- **DSL concepts and migrations.** Lives in `dsl/observability/` and `component/database/memory-nodes/migrations/`. The runtime references their on-disk shapes but does not author them.
- **Cockpit rendering.** The drill-down overlay lives in `memql-cockpit/cli/cluster/architecture.go` -- it consumes the codeMetric concept that the continuous aggregates feed, not this runtime directly.

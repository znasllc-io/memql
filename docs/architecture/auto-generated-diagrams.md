# Auto-Generated Architecture Diagrams

**Status:** Shipped 2026-05-15 on `feature/auto-generated-diagrams`.

**One-line:** Walk the workspace's Go source, build a typed graph of cluster / service / package / type / function nodes plus relationship edges, embed the graph in the binary, and let memQL Cockpit render it as an interactive drill-down topology with live observability data layered on top.

---

## Why this exists

The cockpit's Topology pane was previously a hand-drawn grid of live cluster nodes. It told you "what's running right now" but nothing about "what's in this code." Without a code-level view, the only way to understand the platform's shape was reading source. Hand-maintained UML diagrams drift the moment a function is renamed.

The framework treats the diagram as a **compiled artifact**: same lifecycle as the code, same version, same review trail. Architectural change shows up in a PR diff the same way any other source change does.

---

## End-to-end picture

```
+--------------------+
| Go source          | <-- workspace: memql, memql-bff-copresent, memql-cockpit
| arch.yaml per repo |
+---------+----------+
          | go generate / memql-arch
          v
+--------------------+
| topology.model.json| <-- IR: nodes + edges + attrs
|  (embedded by      |     committed; embedded into every binary
|   //go:embed)      |
+----+---------------+
     |                                     +--------------------+
     |                                     | live cluster nodes | <-- existing tcell grid
     |                                     |  (red/green health)|     (live snapshot)
     +-->+----------------------+          +---------+----------+
         | memQL Cockpit        |                    |
         |  Topology pane       |<-------------------+ same pane, toggled with X
         |                      |
         |  ArchView (architecture)
         |    + drill-down navigator (cluster -> service -> package -> type -> method)
         |    + observability overlay (n / p95 / err% per row)
         +----------+-----------+
                    | concept==v1:observability:codeMetric
                    v
         +----------------------+      +-------------------------------+
         | continuous aggregates|<-----+ code_invocation hypertable     |<--+
         |  1m + 1h rollups     |      |  (TimescaleDB, 1d compression) |   |
         +----------------------+      |  (7d retention default)        |   |
                                       +-------------+------------------+   |
                                                     ^                      |
                                                     | bun batched insert   |
                                                     |                      |
                                       +-------------+------------------+   |
                                       | component/observe TimescaleSink|   |
                                       |  buffered, drop-on-full        |   |
                                       +-------------+------------------+   |
                                                     ^                      |
                                       defer observe.Method().End()         |
                                                     |                      |
                                                     +----------------------+
                                                            joined by FQN
```

The whole picture comes together at the FQN -- the fully-qualified node id (`method:.../component/auth.(*Handler).Login`). One join key, three concerns:

1. **Static map** -- the FQN identifies the Method in `topology.model.json`.
2. **Runtime instrumentation** -- the FQN keys the per-call records the observe runtime emits.
3. **Live config** -- the FQN keys the `v1:observability:codeProfile` rows that bump verbosity on demand.

The cockpit's overlay just zips the three together at render time.

---

## Levels (C4 + UML)

The model uses **C4** as the spine for zoom levels and classic **UML** for the per-level diagram shape:

| Level | C4 name | What it draws | Source kind |
|---|---|---|---|
| L1 | System Context | Cluster + external actors | Synthetic cluster root + (forthcoming) RunsOn edges |
| L2 | Container | Services + RPC/HTTP/CDC links | `KindService` + `EdgeDependsOn` |
| L3 | Component | Packages within a service | `KindPackage` + `EdgeImports` |
| L4 | Code | Structs / interfaces / methods / fields | `KindType` / `KindInterface` / `EdgeContains` / `EdgeHasField` / `EdgeEmbeds` / `EdgeImplements` |

Behavior diagrams ride on top:

| Diagram | Source kind |
|---|---|
| Class diagram | L4 nodes + their `EdgeHasField` / `EdgeImplements` / `EdgeEmbeds` |
| Sequence diagram | Walk forward from an entrypoint over `EdgeCalls` |
| Call graph | `EdgeCalls` directly |
| Activity / flowchart (CFG) | (Planned) `golang.org/x/tools/go/cfg` per-function |
| State diagram | (Planned) annotation-driven, doc-comment hints |
| ERD | (Planned) parse the `.memql` concept tree |
| Deployment | (Planned) `deploy/k8s/` manifests + `scripts/deploy/` |

---

## Components

### Static side (`component/architecture/`)

- **`model/`** -- `Node`, `Edge`, `Kind`, `EdgeKind`, `Model`, `Index`. The IR. JSON read/write with schema versioning.
- **`extract/`** -- the analyzer. `Run(Options) -> *Model` is the public entry. Composes the workspace walker, per-module arch.yaml loader, L2/L3 import-graph pass, L4 types pass, `//memql:observe` source-marker parser, CHA call-graph pass, and synthetic cluster root.
- **`embedded/`** -- `//go:embed topology.model.json` + a `//go:generate` directive pointing at `cmd/memql-arch`. The model is committed; the embed package's `Load()` is the only consumer hook anyone else needs.

CLI: `cmd/memql-arch` -- flags `--root`, `--out`, `--cluster`, `--types`, `--calls`.

### Runtime side (`component/observe/`)

- Level enum + env-driven default (`MEMQL_OBSERVE_LEVEL`)
- Fluent helper `observe.Method(ctx, fqn).Args(...).Result(v).End(&err)` -- one-allocation fast-path, zero-allocation off-path
- Redaction by name pattern, opt-out via `//memql:observe redact=...` source marker
- Per-FQN cache populated by the `CodeProfileSubscriber` (events.Bus subscriber on `graph.node.created._system.v1:observability:codeProfile`)
- `TimescaleSink` -- buffered, drop-on-full, batched bun inserts into the `code_invocation` hypertable
- Two Dependency wrappers (`SinkComponent`, `CodeProfileSubscriber`) so the app bootstrap can wire it like any other component

### Storage side (`dsl/observability/` + `component/database/memory-nodes/migrations/`)

Three concepts under `dsl/observability/concepts.memql`:

- `v1:observability:codeProfile` -- live per-FQN level switchboard. CDC events drive the observe runtime's cache.
- `v1:observability:invocation` -- per-call rows. Backed by the `code_invocation` TimescaleDB hypertable.
- `v1:observability:codeMetric` -- per-(FQN, window) aggregates. Backed by the `code_invocation_1m` + `_1h` continuous aggregates.

One concept change on the cluster side: `v1:cluster:nodeType` gains an optional `codeReference` field so a live node-type row can link back to its architecture-model service id.

The migration creates the hypertable, sets up 1-day compression chunks, a 7-day default retention policy, and the two continuous aggregates with refresh policies (1m bucket / 1 min refresh, 1h bucket / 5 min refresh).

### Cockpit side (`memql-cockpit/cli/cluster/`)

- **`architecture.go`** -- `ArchView`: loads the embedded model, owns the zoom stack, renders the navigator with `[kind] name observable observe=level n=N p95=Xms err=Y%`. `X` toggles, `Enter` zooms in, `Backspace` zooms out, `Esc`/`X` returns to live.
- **`metrics_fetcher.go`** -- `QueryClientMetricsFetcher`: calls `concept==v1:observability:codeMetric` over the gRPC stream, picks the latest window per FQN, hands the map to `ArchView.SetMetrics`.
- **`pool.go`** -- on connection-up, fires a one-shot metrics refresh in a goroutine.

The existing live-topology grid still runs underneath; `X` flips between the two views without changing the surrounding chrome.

---

## ID format (the join key)

```
cluster   "cluster:<name>"
service   "service:<name>"
package   "pkg:<import-path>"
type      "type:<import-path>.<TypeName>"
interface "iface:<import-path>.<InterfaceName>"
func      "func:<import-path>.<FuncName>"
method    "method:<import-path>.(<RecvType>).<MethodName>"
field     "field:<import-path>.<TypeName>.<FieldName>"
```

Constructed via `model.ServiceID(...)`, `model.PackageID(...)`, etc. **Never inline the format string.** The whole stack joins on these strings; renaming a function changes its ID and orphans any codeProfile / observability rows that referenced the old name. Future iteration: a `model_id_alias` concept that migrates rows across renames.

---

## Local override: `.env` over genesis

Genesis seals secrets + variables into an encrypted envelope. For local development, retyping a value into `memql-cockpit genesis init` for every tweak was friction. The framework adds a `.env` override layered on top:

- Each repo's root may contain a `.env` (gitignored). Each repo also has a checked-in `.env.example` documenting the schema.
- At process start, every binary (memql, memql-cockpit) calls `genesis.ApplyLocalOverride(".")` which applies each line via `os.Setenv` BEFORE the config component reads its values.
- `MEMQL_MASTER_KEY` is the one exception: it's the trust anchor for the envelope itself; letting `.env` override it would mean a stray file silently switches which envelope is decrypted. Everything else is fair game.

The implementation lives in `component/genesis/localenv.go`; `ReservedFromOverride` is the allow-list of names the override skips.

---

## Verbosity model (recap)

Four levels, four redactors, three resolvers, two storage tiers:

- Levels: `off`, `count`, `meta`, `verbose`.
- Per-arg redaction: name-pattern auto-redact + `Redact()` interface + `redact=` source marker + per-FQN `redactArgs` on codeProfile.
- Resolution: codeProfile cache (per FQN) -> DefaultLevel (process-wide) -> off.
- Storage: raw rows in the hypertable (short retention, compressed), aggregates in continuous-aggregate views (long-lived, indexed for the drill-down sparkline).

---

## Trade-offs and intentional gaps

- **Call graph is CHA**, not RTA/VTA. CHA is sound + linear + cheap. The cockpit doesn't need runtime-exact precision; a "could be called" edge is fine on the diagram. Switch to RTA/VTA later if false-positive edges become noisy.
- **Generic instantiations are coalesced** back to their generic decl. One edge per `(generic-caller, generic-callee)` pair, not per type-argument combination. Avoids N-edge blow-up on the L4 view.
- **Self-loops are dropped.** Recursive calls don't draw a useful edge in the navigator; they show up as a count in `EdgeContains` already.
- **`observe.Method` is intentionally synchronous on the hot path.** The fast-path is one map lookup + one comparison. The sink's batching makes the "slow path" (LevelCount or higher) constant-time in the worst case.
- **No SDK dependencies** -- everything is stdlib + `golang.org/x/tools` + the bun bunch that memql already uses. No PlantUML, no Python, no Node, no Java required at any tier.
- **Refactor drift on FQNs** -- renaming a function orphans its codeProfile rows. Mitigated by an "alias" follow-up; for now, treat codeProfile + invocation rows as TTL'd by definition (codeProfile.retentionDays).

---

## Day-to-day usage

```bash
# Regenerate the architecture model after code changes:
cd memql/component/architecture/embedded && go generate

# Tune locally without re-sealing genesis:
echo 'MEMQL_OBSERVE_LEVEL=verbose' >> /Users/me/projects/memql/.env

# Instrument a function:
//memql:observe verbose redact=password
func (h *Handler) Login(ctx context.Context, user, password string) (err error) {
    defer observe.Method(ctx, "method:github.com/.../auth.(*Handler).Login").
        Args(observe.Arg("user", user), observe.Arg("password", password)).
        End(&err)
    ...
}

# In cockpit's Topology pane:
#   X         toggle Architecture navigator
#   Enter     zoom in
#   Backspace zoom out one level
#   Esc / X   back to live cluster grid
```

---

## Where to read more

- Static side: [`component/architecture/CLAUDE.md`](../../component/architecture/CLAUDE.md)
- Runtime side: [`component/observe/CLAUDE.md`](../../component/observe/CLAUDE.md)
- Cockpit side: [`memql-cockpit/cli/CLAUDE.md`](../../../memql-cockpit/cli/CLAUDE.md) (X:Architecture key binding)
- Concept surfaces: `dsl/observability/concepts.memql`, `dsl/cluster/concepts.memql` (`v1:cluster:nodeType.codeReference`)
- Hypertable: `component/database/memory-nodes/migrations/20260515000000_observability_hypertable.up.sql`
- Genesis local override: `component/genesis/localenv.go`

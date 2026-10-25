# Architecture Framework

**Purpose:** Define and generate two separate graphs. The source topology
contains Go packages, types, methods and the DSL automation trigger graph.
The platform graph describes declared roles, deployment resources, protocols,
routing, concepts and navigation. They share stable IDs with observability.

## Source topology: generate during verification

The owner selected build-time generation instead of a committed topology.
`make arch-model` writes the ignored `build/architecture/topology.model.json`.
Do not add this file to Git or regenerate it as the last source edit in a PR.
A verified checkout generates its own model, so another PR's line offsets do
not cause a shared JSON conflict.

```bash
make arch-model
make arch-model-check
# Optional call graph, a different generation profile:
make arch-model ARCH_MODEL_CALLS=1
```

The canonical command pins the toolchain declared by `go.work`, the repository
workspace, linux/amd64/v1 source loading, cgo off, no explicit build tags,
default toolchain experiments, FIPS off, no ambient GODEBUG, read-only module
metadata and no external package driver. It uses the current checkout's Go
compiler driver and embedded core DSL, not a previously installed generator.
The types, automations, reproducible-output and `memql` cluster flags are
required by `--canonical`. Runtime product DSL overrides are cleared.

All roots and their dependency closures must load without errors. Missing
packages, type errors, incomplete syntax/type data or an empty load fail the
command before it writes a model. `--calls` has the same completeness rule.
Excluding a node from the rendered graph does not excuse a broken dependency.
A success message reports the completed passes and target/toolchain. It is not
a signed artifact receipt or a substitute for the build's source identity.

`make build` and the native pipeline's `go-checks` step invoke generation.
The normal architecture test package and `make arch-model-check` generate
fresh temporary output and check its decoder, ordering, size floors, source
paths, automation graph and call-graph consistency. The structural output is
reused within that test process; a call-graph profile is generated separately.
Independent jobs do not share this cache. The native step currently retains
no topology artifact: its output is ephemeral checkout data, not a published
receipt or an image payload. `-short` excludes whole-workspace
generation and must not be reported as full architecture qualification.

A clean checkout needs the ordinary pinned Go and identity CSS prerequisites;
`arch-model` runs the existing CSS prerequisite itself. It does not need an
old topology, a placeholder embed or a prior build output. If loading fails,
fix the source/prerequisite. Never accept a partial graph as current evidence.

## Consumers and Go compatibility

No current engine runtime, OS screen, Cockpit release producer, SDK or docs
bundle generator consumes the source topology. It is an inspection and build
validation output, not an image payload. Docker and Fleet role builds therefore
need no topology artifact input and do not embed one. Ordinary source builds
and standalone public Go modules compile without generating it first.

The unused `embedded.ModelJSON` and `embedded.Load` exports have been removed
as a coordinated pre-release Go change. Callers needing a topology generate it
and use `model.ReadJSON` or `extract.Run`; those APIs and the model schema remain.
There is no DSL/wire change. The `embedded` package still exposes the unchanged
`PlatformGraphJSON` and `LoadPlatformGraph` API.

Nodes have unique IDs. Edges are a **multiset**: identical edge records and
attribute-bearing duplicates are legal. Preserve their multiplicity; do not
reject or deduplicate them. `model.WriteJSON` sorts full records deterministically.
The reproducible profile omits the wall clock and absolute workspace path.

## Platform graph: retained committed contract

```bash
make platform-graph
make platform-graph-check
```

`cmd/platformgraph` writes `component/architecture/embedded/platform.graph.json`.
It remains committed and embedded, with its existing drift and well-formedness
gates. Regenerate it when its inputs change: roles, deployment base, protocols,
front door, routing, concepts, automations or navigation. The topology migration
does not remove those inputs, the platform embed or its public parser.

## Source layout and stable IDs

- `model/`: graph types, IDs, strict JSON decoder, stable writer and indexes.
- `extract/`: workspace plans, package/type/call passes, source markers and
  synthetic cluster root. `arch.yaml` selects services and roots.
- `embedded/`: only the committed platform graph and its loader.
- `cmd/memql-arch`: the source topology command and static automation pass.

A role is `service:<type>`, a package `pkg:<path>` and an automation
`automation:<name>`. Use the constructors in `model/ids.go`. Observability
records these IDs; it does not load the topology at runtime. Source marker
attributes can be inspected in generated output, while live capture levels
come from the runtime's own configuration and profile cache.

The historical diagram design is retained in
`docs/internal/design/auto-generated-diagrams.md`; its old embedding and
Cockpit rendering descriptions are historical, not current runtime contracts.

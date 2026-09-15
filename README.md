<p align="center">
  <img src="assets/memql-lockup.png" alt="MemQL" width="440">
</p>

# MemQL

**Build applications where AI can work with your data, tools, and workflows.**

MemQL is an open-source AI platform. Its engine combines typed, versioned
records and relationships with queries, mutations, model routing, agent work,
and event-driven automations. A single `.memql` language connects those pieces.
Integrations bring in external systems; hosted sites and SDKs give people a way
to use what you build.

> **Alpha / pre-1.0 — not production-ready.** Expect breaking changes to the
> language, engine API, and wire protocol. Start with experiments and prototypes.

[Get started](docs/public/overview/quickstart.md) ·
[Documentation](docs/public/overview/index.md) ·
[Visual Studio Code and Cursor extension](editors/vscode/README.md) ·
[MemQL OS](docs/public/operate/memql-os.md)

## One engine, several ways to work

| Part | What it does | Where you use it |
|---|---|---|
| **MemQL engine** | Stores the memory graph, enforces declared access rules, executes constructs, routes model calls, and records agent work | A cluster, reached through gRPC or the browser WebSocket bridge |
| **MemQL OS** | The browser workspace for managing that cluster and working with its data | Apps such as Fleet, Files, Deployables, Nexus, Concepts, and Logs |
| **MemQL for Visual Studio Code and Cursor** | Edits `.memql` files offline; connects to clusters to inspect, run, and train constructs | Your editor |
| **Your applications** | Present your own product using MemQL's capabilities | A hosted site or an external client built with an SDK |

The engine is built on a time-series memory graph backed by PostgreSQL,
TimescaleDB, and pgvector. That storage supports the platform; MemQL also runs
the behavior around it. The agent harness is its durable work system, one part
of the broader product. [Explore the capabilities](docs/public/overview/what-is-memql.md).

## Start with something you can inspect

A concept describes a kind of record. A query names a reusable read operation.
Here is a caller-owned reading list:

```memql
@namespace("reading")
@rowAuthz(owner="ownerUserId")
concept readingItem {
  ownerUserId  string!
  title        string!
  finished     bool
}

@actor
query readingItem readingItems {
  args {
    finished  bool
  }
  filter row => row.ownerUserId == actor.userId && (args.finished == nil || row.finished == args.finished)
  sort "row.createdAt", "desc"
  paginate 50
}
```

`@actor` supplies the authenticated caller. The query filters to that caller,
and `@rowAuthz` declares the ownership tier. `paginate 50` bounds the first
page. The [complete reading-list tutorial](docs/public/language/first-program.md)
adds a mutation and a tool, explains how to validate the file, and walks through
running it in VS Code or Cursor. Saving a file does not deploy it.

## Get started

- **Try the language:** [build and install the Visual Studio Code and Cursor extension](docs/public/language/vscode.md#get-the-extension), then open the [example](examples/reading-list/reading.memql). Editing works without a cluster.
- **Use an existing cluster:** add it in the extension, sign in, and inspect a query before running it.
- **Run MemQL locally:** follow the [quickstart](docs/public/overview/quickstart.md). The supported local stack uses Docker, k3d, and ArgoCD. An initial image build can take time.

## A visual workspace for the cluster

MemQL OS is a single-page application made of focused apps. Fleet manages
machines and execution resources; Files holds artifacts; Deployables manages
what is served; Nexus exposes goals, runs, and approvals. It uses the same
engine APIs as other clients.

**[Supervised Visual Composition](docs/public/operate/supervised-visual-composition.md)**
is the approved design direction: compose objects and their relationships
with mouse and keyboard, and review MemQL's proposals in the same visible
workspace. Fleet provides visual composition, persistent routing-policy editing,
and review of typed Ask proposals; generating proposals requires compatible
inference to be configured. The approved Deployables redesign is being
implemented. New Settings/Logs layouts await approval; generalized autonomous
UI driving is future work.

## Does it work?

The platform measures itself, and publishes what it did **not** measure with
the same prominence as what it did. Every figure carries a median, its spread,
its N, and the commit it came from; a figure nothing measured says so in words
rather than reporting a zero.

Measured on every pull request, against the same model in a bare tool loop with
the platform's machinery switched off:

- A run stopped mid-plan resumes from its own journal and re-executes **no step
  that already completed**.
  <!-- proving: metric=durability.resumedStepsReExecuted arm=platform value=0 -->
- Its side effects are **not delivered twice**.
  <!-- proving: metric=durability.duplicatedSideEffects arm=platform value=0 -->
- A goal the catalog already holds is compiled **without reaching a model** --
  not even the cheap triage classifier.
  <!-- proving: metric=amortizedCost.compileCallsOnCatalogHit arm=platform value=0 -->

Each zero ships with a negative control that must produce a non-zero, because
a counter that never rises on any path reads as zero forever. Full figures,
including everything a replay cannot honestly answer: **[the proving
scorecard](docs/public/overview/proving-scorecard.md)**.

## Go deeper

- [What is MemQL?](docs/public/overview/what-is-memql.md) — capabilities, boundaries, and maturity.
- [Documentation home](docs/public/overview/index.md) — choose a path by task.
- [Language reference](docs/public/language/memql.md) and [authoring rules](docs/public/language/authoring-rules.md).
- [Operating MemQL](docs/public/operate/memql-os.md), [AI routing](docs/public/operate/ai-routing.md), and [site hosting](docs/public/operate/site-hosting.md).
- [Full documentation index](GLOSSARY.md) and [architecture](docs/public/concepts/architecture.md).

## Contribute

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and checks. Run
`make test` for the workspace suite; testing only the root module does not
reach all engine modules. Engine implementation guidance lives in [CLAUDE.md](CLAUDE.md).
Report reproducible problems through [GitHub issues](https://github.com/znasllc-io/memql/issues).
For security reports, follow [SECURITY.md](SECURITY.md).

## License

MemQL is licensed under [Apache 2.0](LICENSE). Bundled infrastructure has its
own licensing; see [the database platform guide](docs/public/operate/database-platform.md).

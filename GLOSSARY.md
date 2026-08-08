# Documentation Index

The complete map of memQL documentation. Layout + rules:
[docs/DOCS_STANDARD.md](docs/DOCS_STANDARD.md).

- **`docs/public/`** — user/developer-facing reference. This is the
  single source of truth the memql.io site renders, versioned per
  release. Areas mirror the site sidebar.
- **`docs/internal/`** — design rationale (ADRs), active plans, and ops
  runbooks. In-repo only; not published.
- Root files are repo governance + the docs standard.

---

## Public docs (`docs/public/`)

### Overview (`overview/`)
- [Why memQL Is a Harness, Not a Library](docs/public/overview/why-memql-harness.md) — the proof-driven thesis: memQL is a running harness + memory substrate.
- [memQL vs. Other Harnesses](docs/public/overview/vs-other-harnesses.md) — honest comparison with the Go + Python field.
- [Quickstart](docs/public/overview/quickstart.md) — get running fast.
- [Tech Stack & Practices](docs/public/overview/tech-stack.md) — the stack + engineering practices.

### Concepts (`concepts/`)
- [Architecture](docs/public/concepts/architecture.md) · [Events](docs/public/concepts/events.md) · [Identifiers](docs/public/concepts/identifiers.md)
- [Data Validation](docs/public/concepts/data-validation.md) · [Concept Versioning](docs/public/concepts/concept-versioning.md) · [Concept Seeding](docs/public/concepts/concept-seeding.md)
- [Permissions & Access Control](docs/public/concepts/permissions-and-access-control.md) · [Tool ↔ Knowledge-Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md)
- [Partition Scoping](docs/public/concepts/partition-scoping.md) -- the canonical tenant scope; core scopes by partition, not spaceId
- [Display Cards & the Fallback Contract](docs/public/concepts/display-cards.md) -- `@displayCard` slots, the `// @no-displayCard:` marker, and what a view does with a concept that declares neither
- [View Elements & the Fitness Contract](docs/public/concepts/view-elements.md) -- the element library (table, calendar, checklist, timeline, board, charts, map) and how a view decides which element fits a concept and which of its fields fill each slot

### The Language (`language/`)
- [MemQL Language](docs/public/language/memql.md) — the DSL reference (also embedded in the binary; see `docs/embed.go`).
- [Functions](docs/public/language/functions.md) · [Authoring Rules](docs/public/language/authoring-rules.md) — read before writing `.memql`.
- [MemQL Sense & the DSL Spec](docs/public/language/sense.md) — language intelligence (tokenize/complete/diagnose/hover) + the `dslspec` source of truth, drift guard, and portable JSON export.
- [MemQL in VS Code](docs/public/language/vscode.md) — the offline VS Code extension + language server (`cmd/memql-lsp`), a second Sense consumer.
- [VS Code Runtime Panel](docs/public/language/vscode-runtime-panel.md) — the extension's activity-bar panel: cluster selection + a generic, live-updating concept browser against a running cluster.
- [Naming Conventions](docs/public/language/naming-conventions.md) · [Reserved Identifiers](docs/public/language/reserved.md) · [Specifications](docs/public/language/specifications.md) · [Attribute Matrix](docs/public/language/attribute-matrix.md)

### AI (`ai/`)
- [LLM Cost Control](docs/public/ai/llm-cost-control.md) — the layered guardrails. · [Operator Capabilities](docs/public/ai/operator-capabilities.md) — capability slugs.

### Build Against It (`build/`)
- [Audio Streaming](docs/public/build/audio-streaming.md) · [Build Tags](docs/public/build/build-tags.md) · [Plugin SDK](docs/public/build/plugin-sdk.md) · [Building a Pack](docs/public/build/building-a-pack.md) — worked-example developer guide (the `examples/referencepack` reference pack)
- Generated reference (DSL constructs + concept catalog) lands in `docs/public/reference/_generated/` at release time (docs-gen).

### Operate (`operate/`)
- **[Minimum Requirements (running beyond local)](docs/public/operate/minimum-requirements.md)** — what you need to run memQL outside the local dev cluster: Tiger Cloud (the only supported DB provider), the pooler/connection model, k8s, secrets, images, GitOps. Start here.
- [Deployment Console](docs/public/operate/deployment-console.md) · [Infrastructure](docs/public/operate/infrastructure.md) · [Database Setup](docs/public/operate/database-setup.md) · [DB Connection Budget & Graceful Deploy](docs/public/operate/db-connection-budget.md)
- [Deploy-bundle runbook](docs/public/operate/deploy-bundle-runbook.md) -- deploying the engine mesh via deployEngineCluster from the cockpit (dry-run ladder, dev flow, staging digests, timeline evidence)
- [Environment Variables](docs/public/operate/env-vars.md) · [LiveKit Provisioning](docs/public/operate/livekit-provision.md) · [Connect to the MCP server](docs/public/operate/mcp-connect.md)
- [Telephony — PSTN calling](docs/public/operate/telephony.md) · [Telephony local-dev (LiveKit Cloud)](docs/public/operate/telephony-local-dev.md)
- [Forge — Company Operating System](docs/public/operate/forge.md) — the role-gated request pipeline, MCP tool surface, and end-to-end employee flow.
- [memQL Portal](docs/public/operate/portal.md) — the browser operations console: why the cluster is derived from the serving origin (no registry), the magic-link / OAuth + PKCE sign-in, where each token lives and the threat model behind that split, and the identity-side config an operator must add.
- [Workbench Runbook](docs/public/operate/workbench-runbook.md) · [Workers Runbook](docs/public/operate/workers-runbook.md) · [Voice Bring-up](docs/public/operate/voice-bringup-verification.md) · [Voice EOU Tuning](docs/public/operate/voice-eou-tuning.md) · [Realtime GA Reference](docs/public/operate/voice-realtime-ga.md)
- **Auth** (`operate/auth/`): [Access Model](docs/public/operate/auth/access-model.md) · [Identity Service](docs/public/operate/auth/identity-service.md) · [User Provisioning](docs/public/operate/auth/user-provisioning.md) · [Actor Envelope](docs/public/operate/auth/actor-envelope.md) · [Per-row Authz](docs/public/operate/auth/per-row-authz-audit.md) · machine creds: [node](docs/public/operate/auth/node-jwt.md) / [voice-agent](docs/public/operate/auth/voice-agent-jwt.md) / [service-account](docs/public/operate/auth/service-account-jwt.md)

### Cockpit (`cockpit/`)
- [Cockpit Editor](docs/public/cockpit/editor.md) — the read-only DSL pack browser (domains/files/Sense-colored source + hover), pack-vs-bundle terminology, and `Ctrl+B` authoring mode: write a local `.memql` bundle with live IntelliSense, then Validate (Gate-1 sandbox) and Inject (session-scoped session-define; durable promotion is a separate Phase-2 action).
- The Cockpit (terminal IDE + ops console) ships from its own repo,
  `github.com/znasllc-io/memql-cockpit`; the rest of its product docs live there.

---

## Internal docs (`docs/internal/`)

- **`design/`** — ADRs / point-in-time design rationale (`status: historical`), kept for the "why": engine audits, DSL syntax/operator standardization, deployment-v2, authored-automations, the voice `4xx` series, the auth threat model, the auto-generated architecture model.
- **`planning/`** — active multi-phase plans (`status: draft`); deleted when shipped. Includes [roadmap.md](docs/internal/planning/roadmap.md).
- **`ops/`** — internal runbooks: [DR runbook](docs/internal/ops/dr-runbook.md), [merge queue](docs/internal/ops/merge-queue.md), tier-4 build graph, safety rollout, blob provisioning, workbench production, and [migrations/](docs/internal/ops/migrations/README.md).

---

## Root governance

- [README](README.md) · [CONTRIBUTING](CONTRIBUTING.md) · [SECURITY](SECURITY.md) · [CODE_OF_CONDUCT](CODE_OF_CONDUCT.md)
- [VERSIONING](VERSIONING.md) — semver + docs-versioning policy · [COMPATIBILITY](COMPATIBILITY.md) — the single-overlay release pin ({engine, bundle, client} digests in one repo)
- [docs/DOCS_STANDARD.md](docs/DOCS_STANDARD.md) — where docs live, front-matter, the repo→site pipeline.

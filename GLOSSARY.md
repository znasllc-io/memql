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
- [VS Code Runtime Panel](docs/public/language/vscode-runtime-panel.md) — the extension's activity-bar panel: **Deployments** (what you operate, at what version, and the runs that changed it), **Clusters** (what you can reach, as whom), and a generic, live-updating concept browser. The plugin/portal boundary that decides which surface a question belongs to is stated in [the Deployments surface design](docs/superpowers/specs/2026-08-14-vscode-deployments-surface-design.md) and in the extension's README.
- [VS Code Runtime Panel — Manual Verification Checklist](docs/public/language/vscode-runtime-panel-verification.md) — what a human presses F5 and works through, and where that sits relative to `make vscode-test` (unit) and `make vscode-test-host` (the Extension Development Host smoke lane).
- [Training Constructs Into a Running Cluster](docs/public/language/training.md) — **seeded** (loaded from disk at boot; needs a rollout) versus **trained** (promoted into the database; live in seconds), and the per-construct states an editor reports against a connected cluster: **untrained**, **drifted**, **trained**, **seeded**, **unknown**. The four actions (dry-run, try in session, promote, demote) and what each commits you to; why **saving is not promoting**; concepts specifically — demote **retires** a concept that has rows and **removes** only an empty one, and a re-promote whose schema changed is **classified** so additive lands and breaking is refused with the field named; the read-only rule (a file is read-only exactly when editing it cannot change what the cluster runs); and why staging and production are trained separately.
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
- **[Environments: staging and production in one installation](docs/public/operate/environments.md)** — the operator model for the two-environment cluster: where a write lands (the connection decides, and nothing else does), why `public` is left empty so a mistyped search path fails loudly instead of silently meaning production, the role × environment host table and why role hosts hyphenate while site hosts nest, what promotion moves (engine digests, bundle digests, site `bundleRef`s) — and what it does NOT move: trained constructs do not cross environments.
- [The cluster front door](docs/public/operate/front-door.md) — the five host rules and what is behind each, the per-Service backend-protocol constraint that explains `bff` vs `bff-http` and the MCP host, why a missing Ingress rule fails with a protocol error rather than a 404, why the count must not grow (a site is a row), and the media plane that is permanently separate.
- [Site hosting](docs/public/operate/site-hosting.md) — the runbook for deploying a website or app onto a memQL cluster: the static-bundle contract and what does/doesn't work (and why static satisfies SEO), the commerce case and the prerender budget, bake vs upload, publishing from CI with a service-account credential, rollback, `draft`/`live`/`disabled`, `apiProxy`, live data over graph subscriptions, and the escape hatch for a site that genuinely needs SSR.
- [Connected — a site that stays where it is](docs/public/operate/connected-integration.md) — the hosting ladder and its cheapest rung: a customer's existing site adds the SDK and points at `api.<domain>`. Generating a typed client, registering an OAuth client, the owner/admin CORS grant (and the three places an origin has to be named), and why the cross-origin token lives in memory rather than in the `SameSite=Lax` cookie.
- [Environment Variables](docs/public/operate/env-vars.md) · [LiveKit Provisioning](docs/public/operate/livekit-provision.md) · [Connect to the MCP server](docs/public/operate/mcp-connect.md)
- [Telephony — PSTN calling](docs/public/operate/telephony.md) · [Telephony local-dev (LiveKit Cloud)](docs/public/operate/telephony-local-dev.md)
- [Forge — Company Operating System](docs/public/operate/forge.md) — the role-gated request pipeline, MCP tool surface, and end-to-end employee flow.
- [memQL Portal](docs/public/operate/portal.md) — the browser operations console: why the cluster is derived from the serving origin (no registry), the magic-link / OAuth + PKCE sign-in, where each token lives and the threat model behind that split, and the identity-side config an operator must add.
- [Campaign sending](docs/public/operate/campaign-sending.md) — the email-campaign sending engine: cluster-wide digest-keyed suppression enforced at the point of send, the delivery ledger that makes a resumed send idempotent, our token bucket plus the provider's 429, RFC 8058 one-click unsubscribe, what a hard bounce does to an audience membership, and why campaigns do not use the transactional outbox.
- [Workbench Runbook](docs/public/operate/workbench-runbook.md) · [Workers Runbook](docs/public/operate/workers-runbook.md) · [Voice Bring-up](docs/public/operate/voice-bringup-verification.md) · [Voice EOU Tuning](docs/public/operate/voice-eou-tuning.md) · [Realtime GA Reference](docs/public/operate/voice-realtime-ga.md)
- **Auth** (`operate/auth/`): [Access Model](docs/public/operate/auth/access-model.md) · [Identity Service](docs/public/operate/auth/identity-service.md) · [Sign-in Paths](docs/public/operate/auth/sign-in-paths.md) · [User Provisioning](docs/public/operate/auth/user-provisioning.md) · [Actor Envelope](docs/public/operate/auth/actor-envelope.md) · [Per-row Authz](docs/public/operate/auth/per-row-authz-audit.md) · machine creds: [node](docs/public/operate/auth/node-jwt.md) / [voice-agent](docs/public/operate/auth/voice-agent-jwt.md) / [service-account](docs/public/operate/auth/service-account-jwt.md)

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

# Documentation Index

The complete map of MemQL documentation. Layout + rules:
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
- [What Is MemQL](docs/public/overview/what-is-memql.md) — the platform: modules it runs, clients you build on it, the memory graph underneath.
- [The Harness Module](docs/public/overview/why-memql-harness.md) — the proof-driven tour of the module that runs agents: loop, budgets, memory consolidation.
- [MemQL vs. Agent Libraries and Frameworks](docs/public/overview/vs-other-harnesses.md) — honest comparison with the Go + Python field.
- [Quickstart](docs/public/overview/quickstart.md) — get running fast.
- [Tech Stack & Practices](docs/public/overview/tech-stack.md) — the stack + engineering practices.

### Concepts (`concepts/`)
- [Architecture](docs/public/concepts/architecture.md) · [Events](docs/public/concepts/events.md) · [Identifiers](docs/public/concepts/identifiers.md) · [Component vs integration vs pack](docs/public/concepts/component-integration-pack.md) · [Modules](docs/public/concepts/modules.md) · [Clients](docs/public/concepts/clients.md)
- [Data Validation](docs/public/concepts/data-validation.md) · [Concept Versioning](docs/public/concepts/concept-versioning.md) · [Concept Seeding](docs/public/concepts/concept-seeding.md)
- [Permissions & Access Control](docs/public/concepts/permissions-and-access-control.md) · [Tool ↔ Knowledge-Domain Pattern](docs/public/concepts/tool-knowledge-domain-pattern.md)
- [Partition Scoping](docs/public/concepts/partition-scoping.md) -- the canonical tenant scope; core scopes by partition, not spaceId
- [Display Cards & the Fallback Contract](docs/public/concepts/display-cards.md) -- `@displayCard` slots, the `// @no-displayCard:` marker, and what a view does with a concept that declares neither
- [View Elements & the Fitness Contract](docs/public/concepts/view-elements.md) -- the element library (table, calendar, checklist, timeline, board, charts, map) and how a view decides which element fits a concept and which of its fields fill each slot
- [Composed Views](docs/public/concepts/composed-views.md) -- a screen for a concept nobody designed a screen for: how the system works out which view elements fit and composes them automatically
- [Library Document Version History](docs/public/concepts/document-version-history.md) -- the append-only version history every Library document carries; producing, editing, or restoring never overwrites prior content
- [Data Origins: Mirror, Origin, Native](docs/public/concepts/data-origins.md) -- what MemQL's relationship is to a concept's data. **Mirror**: an external system owns it and MemQL's copy is read-only BY CONSTRUCTION. **Origin**: MemQL owns it and pushes changes out to external mirrors through a durable **outbox**. **Native**: MemQL owns it and nobody else has a copy. Declared with `@origin` / `@mirroredTo`; a **connector** is the integration that fills a mirror or drains an outbox, and runs as a named actor admitted only to the concepts that name it

### The Language (`language/`)
- [MemQL Language](docs/public/language/memql.md) — the DSL reference (also embedded in the binary; see `docs/embed.go`).
- [Functions](docs/public/language/functions.md) · [Authoring Rules](docs/public/language/authoring-rules.md) — read before writing `.memql`.
- [MemQL Sense & the DSL Spec](docs/public/language/sense.md) — language intelligence (tokenize/complete/diagnose/hover) + the `dslspec` source of truth, drift guard, and portable JSON export.
- [MemQL in VS Code](docs/public/language/vscode.md) — the offline VS Code extension + language server (`cmd/memql-lsp`), a second Sense consumer.
- [VS Code Runtime Panel](docs/public/language/vscode-runtime-panel.md) — the extension's activity-bar panel: **Deployments** (what you operate, at what version, and the runs that changed it), **Clusters** (what you can reach, as whom), and a generic, live-updating concept browser. The extension/portal boundary that decides which surface a question belongs to is stated in [the Deployments surface design](docs/superpowers/specs/2026-08-14-vscode-deployments-surface-design.md) and in the extension's README.
- [VS Code Runtime Panel — Manual Verification Checklist](docs/public/language/vscode-runtime-panel-verification.md) — what a human presses F5 and works through, and where that sits relative to `make vscode-test` (unit) and `make vscode-test-host` (the Extension Development Host smoke lane).
- [Training Constructs Into a Running Cluster](docs/public/language/training.md) — **seeded** (loaded from disk at boot; needs a rollout), **staged** (in the database, durable, callable by its author alone), and **trained** (in the database, live for everyone in seconds), and the per-construct states an editor reports against a connected cluster: **untrained**, **drifted**, **trained**, **staged**, **seeded**, **unknown**. The five actions (dry-run, try in session, stage, promote, demote) and what each commits you to — staging is the promote path with the cross-node broadcast omitted, and **that omission is the tier**; training a staged construct is the same Promote, flipping the same row. Why **saving is not promoting**; why a **concept cannot be staged**; concepts specifically — demote **retires** a concept that has rows and **removes** only an empty one, and a re-promote whose schema changed is **classified** so additive lands and breaking is refused with the field named; the read-only rule (a file is read-only exactly when editing it cannot change what the cluster runs); and why a separate installation is trained separately.
- [Naming Conventions](docs/public/language/naming-conventions.md) · [Reserved Identifiers](docs/public/language/reserved.md) · [Specifications](docs/public/language/specifications.md) · [Attribute Matrix](docs/public/language/attribute-matrix.md)

### AI (`ai/`)
- [LLM Cost Control](docs/public/ai/llm-cost-control.md) — the layered guardrails. · [Operator Capabilities](docs/public/ai/operator-capabilities.md) — capability slugs.

### Build Against It (`build/`)
- [Audio Streaming](docs/public/build/audio-streaming.md) · [Build Tags](docs/public/build/build-tags.md) · [Plugin SDK](docs/public/build/plugin-sdk.md) · [Building a Pack](docs/public/build/building-a-pack.md) — worked-example developer guide (the `examples/referencepack` reference pack)
- Generated reference (DSL constructs + concept catalog) lands in `docs/public/reference/_generated/` at release time (docs-gen).

### Operate (`operate/`)
- **[Minimum Requirements (running beyond local)](docs/public/operate/minimum-requirements.md)** — what you need to run MemQL outside the local dev cluster: self-hosted CloudNativePG (the only supported DB provider), the pooler/connection model, k8s, secrets, images, GitOps. Start here.
- [Before you install a local cluster](docs/public/operate/install-prerequisites.md) — the short list of things the install wizard deliberately does not place for you, and why.
- [Reproduce the cloud locally (k3d + ArgoCD)](docs/public/operate/reproduce-the-cloud-locally.md) — the blessed local dev topology: same Kustomize base, same ArgoCD-reconciled manifests as the cloud cluster.
- [Environment parity — one topology everywhere](docs/public/operate/environment-parity.md) — the non-negotiable standard: every installation runs the same topology, deploy process, and connection model; only configuration values and hardware resources vary.
- [Deployment Console](docs/public/operate/deployment-console.md) · [Infrastructure](docs/public/operate/infrastructure.md) · [Database Platform (CloudNativePG)](docs/public/operate/database-platform.md) · [DB Connection Budget & Graceful Deploy](docs/public/operate/db-connection-budget.md)
- [Downstream product stacks (the DSL-bundle contract)](docs/public/operate/downstream-stacks.md) — the engine is product-agnostic; a product built on MemQL is a DSL bundle plus client surfaces deployed as one overlay.
- [Upgrade barriers](docs/public/operate/upgrade-barriers.md) — what turns a normal retag upgrade into something that needs an operator's attention, and why.
- [Node Lifecycle, Graceful Drain & Maintenance Runbook](docs/public/operate/lifecycle-runbook.md) — the explicit node state machine, graceful SIGTERM drain, on-demand maintenance trigger, and the coordinated/ordered rollout driver.
- [Inbound delivery](docs/public/operate/inbound-delivery.md) — the webhook receiver: per-source allowlists and signature verification for third-party events landing in pure DSL.
- [Outbound delivery](docs/public/operate/outbound-delivery.md) — staging rows, allowlists, and the drain worker for product-DSL-initiated outbound sends.
- [Cutting a release from the portal](docs/public/operate/release-cutting.md) -- the owner-only button that tags `main` and publishes the GitHub Release the image-build cascade fires on: the two values to seed (`MEMQL_RELEASE_REPO`, `MEMQL_GITHUB_RELEASE_TOKEN`) and why the engine carries no repository default, the `v*` tag protection that bounds a leaked token, the first `dryRun`, what each typed refusal means -- including the half-done `tag_created_release_failed` and the two ways out of it -- and why an image check that ERRORED never becomes a claim that the images are missing.
- [Deploy-bundle runbook](docs/public/operate/deploy-bundle-runbook.md) -- deploying the engine mesh via deployEngineCluster from the cockpit (dry-run ladder, local flow, cloud digests, timeline evidence)
- [The recorded cluster version](docs/public/operate/cluster-version-record.md) -- the `version` key in the shared `clusters.yaml`: why no installed cluster can state its release honestly, why the record is readable with the cluster switched off, the three-state write semantics the version learners depend on, and the rule that a write may never downgrade record quality
- [The cluster front door](docs/public/operate/front-door.md) — the six host rules and what is behind each, the per-Service backend-protocol constraint that explains `bff` vs `bff-http` and the MCP host, the two TLS regimes -- HTTP-01 naming exact hosts only (memql#4224) and, where the overlay declares a DNS-01 issuer, a second wildcard certificate that finally covers `*.<domain>` (memql#4347) -- and why the portal carries an exact rule of its own either way, why a missing Ingress rule fails with a protocol error rather than a 404, why the count must not grow (a site is a row), and the media plane that is permanently separate.
- [Azure entry install -- sanctioned first bring-up on AKS](docs/public/operate/azure-entry-install.md) — standing up an entry-shape instance on `overlays/cloud-entry`: the hosts and the exact-name HTTP-01 certificate, the Argo host-patch list, the voice-off hold on the LiveKit Services, the Graph mail sender on the mailbox tenant, and the handoff to an instance repo -- today's source of truth, the remote-composed target shape, the External Secrets caveat, capture / switch / rollback, and the twice-daily entry pin loop.
- [The Library](docs/public/operate/library.md) — files, the index behind them, search by meaning, training into a knowledge domain, export and archive: the upload and content routes and their caps, why upload and train are two acts, why the promotion filter and the archive filter are spelled the way they are, and the two things that are deny-by-default or approximate rather than finished.
- [Deployables](docs/public/operate/deployables.md) — the surface that replaces Sites: who may own a site and why a cluster owner cannot personally own one, the `<slug>.<domain>` hostname policy and its DERIVED reserved set, the three live kinds and why Android / iOS / macOS have no schema, deploying a zip from the Library with its named refusal reasons, and the Shopify storefront binding the edge injects at serve time.
- [Site hosting](docs/public/operate/site-hosting.md) — the runbook for deploying a website or app onto a MemQL cluster: the static-bundle contract and what does/doesn't work (and why static satisfies SEO), the commerce case and the prerender budget, bake vs upload, publishing from CI with a service-account credential, rollback, `draft`/`live`/`disabled`, `apiProxy`, live data over graph subscriptions, and the escape hatch for a site that genuinely needs SSR.
- [Connected — a site that stays where it is](docs/public/operate/connected-integration.md) — the hosting ladder and its cheapest rung: a customer's existing site adds the SDK and points at `api.<domain>`. Generating a typed client, registering an OAuth client, the owner/admin CORS grant (and the three places an origin has to be named), and why the cross-origin token lives in memory rather than in the `SameSite=Lax` cookie.
- [Environment Variables](docs/public/operate/env-vars.md) · [LiveKit Provisioning](docs/public/operate/livekit-provision.md) · [Connect to the MCP server](docs/public/operate/mcp-connect.md)
- [Telephony — PSTN calling](docs/public/operate/telephony.md) · [Telephony local-dev (LiveKit Cloud)](docs/public/operate/telephony-local-dev.md)
- [Forge — Company Operating System](docs/public/operate/forge.md) — the role-gated request pipeline, MCP tool surface, and end-to-end employee flow.
- [MemQL Portal](docs/public/operate/portal.md) — the browser operations console: why the cluster is derived from the serving origin (no registry), the magic-link / OAuth + PKCE sign-in, where each token lives and the threat model behind that split, and the identity-side config an operator must add.
- [MemQL Cloud — the fleet control plane](docs/public/operate/memql-cloud.md) — running a fleet of MemQL instances by subscription: why the instance row is desired state and writing its status is how you act on a tenant, why that design is loop-free without a lock, what each tier buys (and why the trial throttles where every other tier meters), driving the four lifecycle capability scripts by hand, and the four ways an operator gets bitten.
- [MemQL Cloud billing](docs/public/operate/memql-cloud-billing.md) — Stripe over the existing inbound receiver (and the composite signature header that made that possible at all), the two layers of allowance and why the meter alone cannot bound spend, why zero means unlimited and is therefore refused, dunning keyed per cycle rather than per account, and the one missing piece four remaining gaps share.
- [Orbit — the MemQL Cloud customer console](docs/public/operate/memql-cloud-orbit.md) — the three consoles and which is which, why a client cannot be the gate and what enforces that instead (two server-side halves, both gated), why Orbit never talks to ArgoCD, the two-write tier change and why the join lives in the client, and what a customer deliberately cannot call.
- [MemQL Cloud trials, hibernation, and the condensed profile](docs/public/operate/memql-cloud-trials.md) — the 14-day trial as five scheduled sweeps, why the clock bounds a trial in time and only the in-tenant ceiling bounds it in spend, why the `where` clause is the only thing between a sweep and the whole fleet, why hibernation applies to paying customers too — and the measured finding that there is no single-process build and cannot be one by combining tags, so the floor is three app pods and `solo` is a replica-count condensation.
- [MemQL Cloud — the launch checklist](docs/public/operate/memql-cloud-launch.md) — the one hard gate (and why the meter alone does not satisfy it), the seven checklist items a test decides rather than a person, and the two things deliberately left unticked: the parity-cluster run that proves automation step semantics, and a dollar figure nobody has measured.
- [The Shopify connector](docs/public/operate/shopify-connector.md) — a complete, GENERATED mirror of a store (65 concepts, pinned to one Admin API version), the origins a wholesale business needs on top of it, and the boundary of what nobody can mirror. Why a webhook is a trigger rather than a payload, why reconciliation is a requirement rather than a backstop and drift is its output, why a store's three credentials are references and never tokens, why a Shopify `userError` dead-letters instead of retrying, and the quarterly bump as a reviewed regeneration.
- [The Shopify storefront completeness checklist](docs/public/operate/shopify-storefront-checklist.md) — what a headless storefront has to build to lose nothing a Liquid theme gave the store: the Headless channel's two tokens and why the private one needs the buyer IP, `@inContext` on every catalog query, the Customer Account API (legacy accounts deprecated 2026-02-26), the B2B buyer context and the responses that must never be cached, what headless loses and what it keeps, and the per-app inventory a launch depends on.
- [Campaign sending](docs/public/operate/campaign-sending.md) — the email-campaign sending engine: cluster-wide digest-keyed suppression enforced at the point of send, the delivery ledger that makes a resumed send idempotent, our token bucket plus the provider's 429, RFC 8058 one-click unsubscribe, what a hard bounce does to an audience membership, and why campaigns do not use the transactional outbox.
- [Workbench Runbook](docs/public/operate/workbench-runbook.md) · [Workers Runbook](docs/public/operate/workers-runbook.md) · [Local apps as execution surfaces](docs/public/operate/local-apps.md) · [Voice Bring-up](docs/public/operate/voice-bringup-verification.md) · [Voice EOU Tuning](docs/public/operate/voice-eou-tuning.md) · [Realtime GA Reference](docs/public/operate/voice-realtime-ga.md)
- **Auth** (`operate/auth/`): [Access Model](docs/public/operate/auth/access-model.md) — includes **shared mailboxes** (the `sharedMailbox` hint) and **passkey-only sign-in** (`signInPolicy`), plus the self-scoped sessions read · [Identity Service](docs/public/operate/auth/identity-service.md) · [Sign-in Paths](docs/public/operate/auth/sign-in-paths.md) · [User Provisioning](docs/public/operate/auth/user-provisioning.md) — the **device-bound, approve-on-click** magic-link flow · [Actor Envelope](docs/public/operate/auth/actor-envelope.md) · [Per-row Authz](docs/public/operate/auth/per-row-authz-audit.md) · [Recovery Key](docs/public/operate/auth/recovery-key.md) · [The operator credential (`MEMQL_OPERATOR_KEY`)](docs/public/operate/auth/operator-credential.md) · [Account tokens](docs/public/operate/auth/account-tokens.md) — a credential an operator mints against a managed customer account · [Badge operator grants](docs/public/operate/auth/badge-operator-grant.md) — shared-terminal attribution · [Anthropic workload identity federation](docs/public/operate/auth/anthropic-federation.md) — the engine proves who it is instead of holding a vendor key · machine creds: [node](docs/public/operate/auth/node-jwt.md) / [voice-agent](docs/public/operate/auth/voice-agent-jwt.md) / [service-account](docs/public/operate/auth/service-account-jwt.md)

### Cockpit (`cockpit/`)
- [Cockpit Editor](docs/public/cockpit/editor.md) — the read-only DSL pack browser (domains/files/Sense-colored source + hover), pack-vs-bundle terminology, and `Ctrl+B` authoring mode: write a local `.memql` bundle with live IntelliSense, then Validate (Gate-1 sandbox) and Inject (session-scoped session-define; durable promotion is a separate Phase-2 action).
- The Cockpit (terminal IDE + ops console) ships from its own repo,
  `github.com/znasllc-io/memql-cockpit`; the rest of its product docs live there.

---

## Internal docs (`docs/internal/`)

- **`design/`** — ADRs / point-in-time design rationale (`status: historical`), kept for the "why": engine audits, DSL syntax/operator standardization, deployment-v2, authored-automations, the voice `4xx` series, the auth threat model, the auto-generated architecture model.
- **`planning/`** — active multi-phase plans (`status: draft`); deleted when shipped. Includes [roadmap.md](docs/internal/planning/roadmap.md).
- **`ops/`** — internal runbooks: [DR runbook](docs/internal/ops/dr-runbook.md), [merge queue](docs/internal/ops/merge-queue.md), [ruleset baseline](docs/internal/ops/ruleset-baseline.md) (what `main`'s protection rulesets should be, and what asserts it), tier-4 build graph, safety rollout, blob provisioning, workbench production, and [migrations/](docs/internal/ops/migrations/README.md).

---

## Root governance

- [README](README.md) · [CONTRIBUTING](CONTRIBUTING.md) · [SECURITY](SECURITY.md) · [CODE_OF_CONDUCT](CODE_OF_CONDUCT.md)
- [VERSIONING](VERSIONING.md) — semver + docs-versioning policy · [COMPATIBILITY](COMPATIBILITY.md) — the single-overlay release pin ({engine, bundle, client} digests in one repo)
- [docs/DOCS_STANDARD.md](docs/DOCS_STANDARD.md) — where docs live, front-matter, the repo→site pipeline.

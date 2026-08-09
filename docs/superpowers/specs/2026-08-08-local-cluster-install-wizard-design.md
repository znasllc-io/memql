# Local Cluster Install & Uninstall Wizard

**Date:** 2026-08-08
**Status:** Design approved; ready for implementation planning
**Surfaces:** VS Code extension (`editors/vscode`), memQL Cockpit (`memql-cockpit` repo)

---

## 1. Problem

There is no supported path from "a machine with nothing on it" to "a running
local memQL cluster I am signed into." Today it takes a git checkout, a working
knowledge of `make up`, a hand-configured wildcard domain with DNS pointed at
`127.0.0.1`, an mkcert CA in the trust store, a master key, a genesis envelope,
and an AI provider key exported into the environment. Every one of those is
documented; none of them is discoverable.

This design specifies a **wizard** that performs that install, verifies each
step, and — equally important — can **completely reverse it**.

Scope is **local clusters only**. Remote clusters are added with the existing
`memql.clusters.add` flow, which this design leaves untouched.

---

## 2. Constraints discovered during design

These are findings from the current tree, not assumptions. Each one closed off
a design direction, so they are recorded with their evidence.

### 2.1 The portal cannot host the wizard

`clients/portal/src/cluster/endpoint.ts` documents a deliberate decision
(memql#3315): the portal has **no cluster registry at all**. Its cluster is the
origin that served the page. Browser-local storage was considered and rejected;
a server-side registry was deferred with a named seam.

The portal is served *by the bff* (`component/portal`, mounted at `/portal/`,
baked into the bff image). Therefore "no local cluster exists" and "the portal
is reachable" are mutually exclusive states, and `if (!cluster) showWizard()` is
unreachable code. A browser tab also cannot install Docker.

**Consequence:** the wizard lives where there is a filesystem and a process
runner — the VS Code extension and the Cockpit. The portal is unchanged. It is
already local-only, which is the behaviour desired; no work is required there.

### 2.2 `scripts/k3d/up.sh` is repo-shaped, not product-shaped

It reads `deploy/k8s/overlays/local`, points the ArgoCD Application at
`github.com/znasllc-io/memql.git` **at the operator's current branch**, and
seeds TLS from `docker/nginx/certs/dev.{crt,key}`. A user who installs only the
extension has none of that.

**Consequence:** the wizard must acquire the stack. See Decision D3.

### 2.3 Docker is the only dependency that cannot be installed unattended

k3d, kubectl, and mkcert are single binaries that can be placed in
`~/.memql/bin` with no elevation. Docker needs a package manager, a privileged
daemon, a group change, and on macOS a `.dmg` plus a first-run GUI.

**Consequence:** a card promising "fully automatic" cannot keep that promise.
See Decision D4.

### 2.4 The engine has no local-model support, and the platform depends on structured output

`dsl/providers/providers.memql` declares exactly two base providers,
`@type("Anthropic")` and `@type("OpenAI")`, both with hardcoded vendor
endpoints. There is no base-URL override anywhere in the tree and zero Ollama
references. Meanwhile the platform leans hard on structured output and tool
calling: the `respondToUser` envelope, the conductor's single structured call,
and `CallChatStructured` in `component/safety/llm` and
`component/healing/repair_loop.go`.

**Consequence:** a local-model install would plausibly produce a green cluster
whose agents fail on their first turn. Deferred — see §9.

### 2.5 A local cluster has no working mail

`integrations/email` falls back to `LogSender` when neither Graph nor SMTP
credentials are configured. Identity's primary login is magic-link. So "check
your email" is a dead end on exactly the install being built.

**Consequence:** see Decision D7.

### 2.6 The dog-fooding substrate already exists

This was the largest finding. The mechanism of "run once with an LLM, record
it, replay deterministically" is already designed and partly shipped:

- `dsl/actions/concepts.memql` (epic #1734): an LLM step's capability sequence
  is captured as a `candidate` trace, minted into an `action` keyed by
  `inputFingerprint`, replayed **token-free** on identical input, with
  `reliability` reinforced on verified replay and LLM fallback on mismatch.
- `dsl/deployment/actions.memql` already authors deploys as DSL actions whose
  bodies are a single `capability script(...)` call.
- `component/automations/steps/capability_script.go` carries an allowlist that
  **already contains `"k3d.up": "scripts/k3d/up.sh"`** and
  `"k3d.importImage"`.
- The I14 capability-script contract (#2221) exists precisely so a script runs
  identically whether an automation executor or a human invokes it, with
  `component/deploycontrol.ParseCapabilityResult` as the Go effect seam and
  `scripts/lib/capability_contract_test.go` enforcing it.

**Consequence:** authoring the install as DSL is a short step from where the
tree already is — for anything that runs *after* a cluster exists. See D2.

---

## 3. Decisions

### D1 — The wizard lives in the VS Code extension and the Cockpit

The portal is excluded (§2.1) and stays as-is. Both included surfaces already
read and write `~/.memql/clusters.yaml`, so a cluster installed from either
appears in the other.

### D2 — One step graph in DSL; two executors

Install and uninstall are authored **once**, as MemQL constructs under
`dsl/install/`. A build step compiles the graph to JSON.

- **Pre-cluster executor (host).** Walks the compiled JSON, dispatches
  capability scripts, journals to the receipt. No Postgres, no engine.
- **Post-cluster executor (engine).** The existing action executor runs the
  identical definitions against the graph.

**Why not execute the first install inside memQL.** The DSL action executor
lives in the engine; the engine's storage is Postgres + TimescaleDB; Postgres
runs in Docker; Docker is what is being installed. Breaking that cycle requires
an engine that executes DSL with no Postgres, and the storage layer is deeply
Bun + Timescale (hypertables, `(partition, id, createdAt)` PK). That is its own
epic, not a step in this one.

Assessed viability at design time: **~85%** for post-cluster operations
(uninstall, repair, upgrade, re-seed) as genuine in-engine DSL; **~15%** for the
first install without a new bootstrap engine mode. The dual-executor design
captures most of the value of the former without paying for the latter.

### D3 — Acquire the stack by cloning at a tag; published bundles later

The wizard clones memQL into `~/.memql/src` **pinned to a release tag**, not to
the operator's current branch (which is what `up.sh` defaults to today, correct
for repo-based development and wrong for an install).

A versioned, published install bundle is the better long-term artifact but
requires release plumbing, version pinning, and an ArgoCD source that is not a
GitHub branch — a separate project. The wizard fetches the stack through a
single function so that swap touches one place.

### D4 — Two cards: Automatic and Guided. AI provider is a step inside both

The top-level choice is **who executes each step**, not whether AI is involved.

- **Automatic (recommended).** Everything that can be done unattended is done.
  Where elevation is required — Docker, `/etc/hosts`, the mkcert CA — the
  wizard **pauses at a designed checkpoint**, shows the exact command, and polls
  until satisfied, then continues on its own.
- **Guided.** Nothing is executed. Each step shows its command; you run it; the
  wizard verifies and advances.

Same step list, same progress UI, same verification. Guided is also available
**per step**, as the escape hatch when an Automatic step fails.

The AI provider (cloud key) is a collection step shared by both paths, not a
fork.

### D5 — The domain is not optional; it is made free

The cluster's front door is traefik on 443 serving `cockpit.<domain>`,
`identity.<domain>`, `bff.<domain>`. CLAUDE.md's environment-parity rule
explicitly rejects port-forward-as-connection, so a no-domain mode would be a
second connection shape existing only locally — precisely what that rule
forbids.

- **Default:** `*.memql.localhost` via explicit `/etc/hosts` entries for the
  known hostnames. One elevation prompt, shown before it runs, recorded in the
  receipt so uninstall removes exactly those lines. mkcert issues and trusts the
  cert. No domain ownership, no DNS, no third party.
- **Advanced:** bring your own domain. A wildcard A record to `127.0.0.1`,
  verified to resolve before the run proceeds. `local.znas.io` continues to work
  through this path and must remain supported.
- **Rejected:** magic wildcard DNS (`nip.io` / `sslip.io`). Consumer routers'
  DNS-rebinding protection blocks public names resolving to `127.0.0.1`, so it
  fails intermittently and confusingly.

Both paths end in a **verify step**: each hostname resolves to `127.0.0.1`, and
the TLS certificate chains to a trusted root.

### D6 — Cloud AI provider only in v1

Two choices, Anthropic or OpenAI. Paste key → **one live verification call** →
seed as `MEMQL_AI_ANTHROPIC_API_KEY` / `MEMQL_AI_OPENAI_API_KEY`.

**No model picker.** `dsl/providers/providers.memql` already pins models; asking
the user to choose one is a question they cannot answer well.

A third card, **"Local model — coming soon,"** is rendered visible and disabled,
naming what is missing. Honest, and it reserves the slot.

### D7 — The wizard is the setup; first sign-in comes from the pod logs

`component/identity/config.go`'s `BootstrapConfig` already supports unattended
bootstrap: when `MEMQL_IDENTITY_BOOTSTRAP_DOMAIN`, `_OWNER_EMAIL`,
`_OWNER_FIRST_NAME`, `_OWNER_LAST_NAME`, and `_REGISTRATION_MODE` are all set,
the identity service bootstraps on first start with **no `/setup` visit**.

The wizard collects owner first/last name and email alongside the domain and
seeds those variables.

For first sign-in (§2.5), the wizard **reads the magic link from the identity
pod's logs and renders it as a "Sign in as owner" button.** Local-only and
explicitly labelled as such. It is what an operator does by hand today,
automated.

Email configuration stays optional and post-install. Requiring Graph or SMTP
credentials to finish a local install would be a bad trade.

### D8 — Receipt-based uninstall

The wizard writes `~/.memql/install-receipt.json`, **appended per step as each
step succeeds** — not written at the end. That is the property that makes a
*half-finished* install fully reversible, which is the case that matters:
complete installs rarely need uninstalling and broken ones always do.

The receipt records what **this wizard** did: cluster name, images imported,
binaries placed in `~/.memql/bin`, whether the mkcert CA was generated or found
pre-existing, hosts lines added, the cloned stack path, the owner email.

**Uninstall reverses exactly the receipt and nothing more.** It reads the
receipt, not the graph, so it works **even when the cluster is dead**.

It opens with a **dry-run preview itemized from the receipt**: what will be
removed, and what will be left because it pre-existed. Then two tiers:

| Tier | Contents |
|---|---|
| **Default** | k3d cluster, its volumes, memQL images, the cloned stack, binaries we installed, hosts lines we added |
| **Opt-in, off by default** | genesis envelope + master key (irrecoverable), the local entry in `clusters.yaml` (never the file — it is shared with the Cockpit and holds staging/prod entries and their PATs), the mkcert CA **if we created it** |

**Never touched:** Docker, anything that pre-existed the install, other
clusters' configuration.

### D9 — The receipt is also the bridge between executors

On first healthy boot, the receipt is replayed into the graph as rows. The
install becomes a first-class graph record retroactively, and post-cluster
operations (uninstall, repair, upgrade) can then run in-engine as genuine DSL
reading what the install actually did.

### D10 — No LLM on the happy path

The install step graph is finite and knowable, so reasoning buys nothing and
costs a key that may not have been collected yet. Reasoning belongs on the
**repair** path — a step failed in a way the script did not anticipate — which
is what `component/healing/repair_loop.go` is.

Pre-cluster there is no engine and possibly no key, so pre-cluster failures get
precise error messages and the Guided fallback. Post-cluster, the healing loop
can attach later (Phase 5).

### D11 — Style A illustrations

The two cards use **technical line art**: monochrome strokes with a single
accent colour. Automatic shows layers descending into a machine; Guided shows a
terminal with a command being typed. Reads as engineering documentation rather
than marketing.

The **progress screen** adopts the schematic step-flow treatment (filled step
dots, progress bar) — where that visual is literal rather than decorative.

---

## 4. Architecture

Five components.

### 4.1 The step graph — `dsl/install/`

| File | Contents |
|---|---|
| `actions.memql` | One external capability call each, following the pattern in `dsl/deployment/actions.memql` |
| `logic.memql` | Branching: OS detection, domain mode, automatic vs guided, uninstall tier selection |
| `concepts.memql` | The install record replayed into the graph (D9) |
| `automations.memql` | Post-cluster triggers (Phase 5) |

A build step compiles the graph to JSON so a host executor reads it without a
MemQL parser in TypeScript.

### 4.2 Capability scripts — `scripts/install/*.sh`

Authored under the I14 contract (`cap_init`, `cap_param`, `cap_ok`/`cap_fail`,
structured params in, one JSON envelope on stdout, human logs on stderr, honest
exit codes) and registered in `capabilityScriptAllowlist`. Each supports
`--dry-run`.

New scripts cover: dependency detection, binary installation, hosts entries,
mkcert CA and certificate issuance, provider-key verification, magic-link
retrieval, and the uninstall reversals. `k3d.up` and `k3d.importImage` already
exist and are already allowlisted.

### 4.3 Two executors

Per D2. The host executor is TypeScript in the extension; the Cockpit gets a Go
equivalent in Phase 4. Both speak the same compiled-JSON graph and the same
capability-script envelope.

### 4.4 The receipt — `~/.memql/install-receipt.json`

Per D8/D9. Journal, uninstall source of truth, and executor bridge.

### 4.5 The wizard UI

Two front ends: the **VS Code webview** (React, Style A) and the **Cockpit TUI**
(Go). They share the graph, the executor protocol, and the receipt; each renders
natively. A shared React package buys nothing across a webview and a TUI.

**The load-bearing property: no front end decides anything.** Step order,
dependencies, what is parallelizable, what requires elevation, and what
uninstall touches all live in DSL. The UIs render state and collect input. This
mirrors the split already enforced in the extension by
`cmd/memql-lsp/vscodeimportrule_test.go`.

---

## 5. The run

> **Terminology.** This section numbers the **stages of a single install run**.
> §8 numbers the **phases of the implementation**. They are unrelated
> sequences; "Stage" always means runtime, "Phase" always means delivery.

**Entry.** The extension detects no local cluster. "Add cluster" offers three
choices: *Install locally — Automatic*, *Install locally — Guided*, and *Connect
to a remote cluster* (the existing flow, unchanged).

**Every step declares three things:** its dependencies, its capability call, and
**how to know it is done** (a verify predicate). **Verify is what advances the
wizard** — not a zero exit code. That is what makes Automatic and Guided the
same graph: Automatic runs the capability then verifies; Guided renders the
command, waits, and polls the same verify.

### Stage 1 — Collect (before any work)

OS detection (Linux/macOS; Windows receives an honest refusal rather than a
broken run), domain choice (D5), owner first/last name and email (D7), AI
provider and key with one live verification call (D6).

Everything is collected up front so the long part runs unattended. A wizard that
stops to ask a question nine minutes in is a wizard people abandon.

### Stage 2 — Preflight (fully parallel, read-only)

Docker present and daemon running; k3d / kubectl / git / mkcert presence and
versions; disk space; ports 80 and 443 free; domain resolution when a custom
domain was supplied. Produces the full dependency checklist with each item's
state before anything is touched.

### Stage 3 — Provision

- **Parallel where safe:** k3d, kubectl, mkcert binaries into `~/.memql/bin`.
- **Serial and elevation-gated:** Docker (the designed checkpoint), `/etc/hosts`
  entries, mkcert CA trust.
- Clone the stack at a **tag** into `~/.memql/src` (D3).

### Stage 4 — Bring-up (strictly serial)

Generate master key and genesis envelope → `k3d.up` → seed secrets (bootstrap
variables, AI key, TLS certificate) → wait healthy.

### Stage 5 — Verify and hand off

Hostnames resolve, TLS chains to a trusted root, gRPC answers. Write the
`clusters.yaml` entry with `local: true`. Retrieve the magic link and render
**Sign in as owner** (D7). Replay the receipt into the graph (D9).

---

## 6. Failure, resume, and uninstall

**Idempotent by construction.** Every step runs its verify first and skips when
already satisfied — the same `changed=false` behaviour `up.sh` has today. So
re-running the wizard **resumes** rather than restarts.

**On failure,** the dependent subtree halts while independent branches finish.
The failure is presented using the capability contract's existing exit-code
taxonomy:

| Exit | Meaning | Presentation |
|---|---|---|
| 2 | bad param | internal error; report with context |
| 3 | refused | explain the refusal |
| 4 | prerequisite missing | render as a guided instruction |
| 5 | operation failed | show stderr, offer retry |

Two actions are always offered: **Retry**, and **Switch this step to Guided**.
The script's stderr is shown verbatim in a disclosure. Each step carries a
timeout so nothing hangs indefinitely.

**Cancel** leaves a valid receipt, so a cancelled install is still fully
uninstallable.

**Uninstall** per D8: reverses the receipt backwards, each reversal with its own
verify, works against a dead cluster, opens with an itemized dry-run preview,
and applies the two-tier model.

---

## 7. Testing

**The scripts inherit their gate.** `scripts/lib/capability_contract_test.go`
already enforces the I14 contract on every script sourcing the library. New
install scripts are covered automatically. `--dry-run` lets contract tests
exercise the envelope without touching a machine.

**The graph is pure data, so it is tested as data.** Over the compiled JSON:

- no dependency cycles; the topological order is satisfiable
- **every step has a verify predicate**
- every capability id resolves in `capabilityScriptAllowlist`
- **every step that writes to the receipt has a corresponding reversal in the
  uninstall graph**

The last assertion mechanically prevents uninstall from drifting behind install,
which is how uninstallers rot. It is only writable because install and uninstall
are in scope together (see §8).

**Verify predicates are tested independently of the actions they verify.** They
are the advance mechanism; a verify that always returns true would make the
whole wizard lie while appearing green.

**The host executor** is unit-tested against a fake script runner, matching the
existing `DeployControlPort` fake pattern under bare `node --test`.

**The UI is deliberately thin.** Wizard state lives in `src/state/` and is
tested bare; the webview is adapter wiring. That split is already enforced by
`cmd/memql-lsp/vscodeimportrule_test.go`.

**Clean-runner end-to-end.** A CI job on a fresh Linux runner runs Automatic to
completion, signs in as owner, then runs uninstall and **asserts the machine is
back to baseline** — binaries, `/etc/hosts`, Docker volumes and images,
`~/.memql`. A snapshot diff, not an inspection. macOS on the same lane once a
runner is available. Nothing else meaningfully tests a receipt.

---

## 8. Phasing and epic decomposition

### Phases

| Phase | Contents | Deliverable |
|---|---|---|
| **1a** | `scripts/install/*.sh`, `dsl/install/` graph + JSON compiler, host executor, receipt, CLI harness | `install` runs end to end on Linux from a terminal |
| **1b** | VS Code wizard: Style A cards, collect screens, parallel progress, elevation checkpoint, sign-in-as-owner | Zero to signed-in cluster from VS Code on Linux |
| **2** | Uninstall graph, dry-run preview, two tiers, full Guided mode + per-step escape hatch, clean-runner E2E | Provably reversible |
| **3** | macOS parity: Homebrew/dmg paths, TCC prompts, macOS CI lane | Both supported platforms |
| **4** | Cockpit TUI front end over the same graph | Parity across both surfaces |
| **5** | Receipt replays into the graph; uninstall / repair / upgrade run as in-engine DSL automations; `component/healing` attaches | The dog-fooding loop closes |

Building 1a before any UI means the graph is proven when the wizard arrives, and
the wizard is then what it should be — a renderer.

### Epics

**Three epics. Install and uninstall belong in the same epic.**

- **Epic 1 — Install/uninstall substrate (Linux, no UI).** All of Phase 1a, plus
  the parts of Phase 2 that are graph-level and not UI-level: the uninstall
  graph, the two tiers, the dry-run preview as structured data, and the
  clean-runner E2E.
- **Epic 2 — The wizard.** All of Phase 1b, plus the parts of Phase 2 that are
  UI-level: full Guided mode, the per-step Guided escape hatch, and the rendered
  dry-run preview. Depends on Epic 1's graph being real; once it is, this
  parallelizes well as front-end work.

  Phase 2 is therefore the one phase that spans two epics. The split is on the
  same seam as everything else in this design: the graph and its reversals are
  substrate, the modes and previews are rendering.
- **Epic 3 — Reach.** Phases 3, 4, and 5 as independent tracks. Phase 5
  (in-engine execution) may warrant promotion to its own epic once sized.

**Why install and uninstall stay paired:** the "every receipt-writing step has a
reversal" test can only exist if both are in scope at once. Split across epics,
that test cannot be written, uninstall becomes a follow-up, and follow-ups slip.

---

## 9. Out of scope

Each of these is a separate project, named here so the boundary is explicit.

- **Local-model support.** Requires `@type("OpenAICompatible")` plus a
  configurable base URL in the engine, and a structured-output conformance check
  before any model is allowed to back an agent (§2.4). Ollama remains the right
  runtime when this is taken up. The wizard renders a disabled "coming soon"
  card meanwhile (D6).
- **Published install bundle.** Versioned artifacts replacing the git clone
  (D3). Touches the build server, registry, version pinning, and the ArgoCD
  source.
- **Anything portal-side.** The portal is already local-only and correct as-is
  (§2.1).
- **Windows support.** Detected and refused with a clear message.
- **Email configuration.** Optional and post-install (D7).

---

## 10. References

**Code**

- `clients/portal/src/cluster/endpoint.ts` — the no-registry decision (#3315)
- `editors/vscode/src/clusters/{model,file}.ts` — `~/.memql/clusters.yaml`,
  shared with the Cockpit
- `scripts/k3d/up.sh`, `scripts/k3d/seed-secrets.sh` — existing capability
  scripts
- `scripts/lib/capability.sh`, `scripts/lib/capability_contract_test.go` — the
  I14 contract and its gate
- `component/automations/steps/capability_script.go` — the allowlist, already
  containing `k3d.up`
- `component/deploycontrol/capability_result.go` — the Go effect seam
- `dsl/deployment/actions.memql` — the action-authoring pattern to follow
- `dsl/actions/concepts.memql` — the action library (#1734): capture, mint,
  replay, reinforce
- `dsl/capabilities/capabilities.memql` — the capability vocabulary
- `component/identity/config.go` — `BootstrapConfig` and the unattended
  bootstrap path
- `component/healing/repair_loop.go` — the LLM repair path (Phase 5)
- `dsl/providers/providers.memql` — the two supported provider vendors

**Docs**

- `docs/internal/design/capability-script-contract.md` (#2221)
- `docs/public/operate/environment-parity.md`
- `docs/public/operate/reproduce-staging-locally.md`

**Design session artifacts**

- `.superpowers/brainstorm/*/content/install-cards.html` — the three
  illustration styles; Style A selected

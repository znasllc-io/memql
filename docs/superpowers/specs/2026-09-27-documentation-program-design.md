# Documentation -- one current set on memql.io, every fact generated or gated, seven epics with the publish fix first

- **Date:** 2026-09-27
- **Status:** agreed with the owner in the 2026-09-27 brainstorm. Two
  decisions bound it from the start: memql.io serves the current release only
  (D1), and documentation follows the engine / template / instance split (D2).
  Every other fork was put to the owner and answered as recorded in section 3;
  the two decided against the recommendation are marked (D16, D17). Issues
  were filed with the record (section 8). The record is product-neutral; the
  owner's own commercial draft and the full brainstorm belong in the instance
  repository's private design folder, not here (`docs/design/` in `memql-znas`).
- **What it is:** documentation as a product of the tree -- one public set
  rebuilt per engine release, facts generated or gated, diagrams from one
  platform graph that also feeds the Cluster app, public surfaces held neutral
  by a gate, conventions inherited from the template -- plus the repairs that
  make memql.io current this week.
- **Why it is pivotal:** an outside evaluator arrives within days and reads
  memql.io and the README first. Publishing has failed silently on every
  release since 2026-09-04, the site is 22 releases behind, every relative
  link on it 404s, and "Get started" downloads an asset that no longer exists.
- **Repositories:** `memql` (`docs/`, `cmd/docs-gen`, `scripts/docs`,
  `.github/workflows`, `component/node`, `dsl/cluster`, `clients/os`, new
  `core/positioning` and `cmd/platformgraph`); `memql-project` (the template);
  `memql-cockpit`; `memql-znas`, the instance repository hosting the public
  site.
- **Measurements this record rests on:** taken 2026-09-27 at engine `main` @
  ee3e87791 (VERSION 0.23.5), `memql-project` @ 704765e, `memql-cockpit` @
  3e2b846 (v0.16.0), the instance repository, the live site and the Actions
  run history; reproduced in section 1, line numbers at those commits.

## 1. Problem

The publish failure is one line. The `verify bundle` step of
`.github/workflows/publish-docs-bundle.yml` (line 140) pipes `tar -tzf` into
`grep -qx './manifest.json'` under `set -euo pipefail`: grep exits on the
match, tar gets EPIPE, pipefail fails the step, and the `||` branch reports
the manifest missing after the build wrote it. All 52 release runs from
v0.20.22 (run 33922058668) through v0.23.5 (run 36240504345) failed; the last
asset is `docs-0.20.21.tgz`. The lane is not required and no issue tracks it;
#5663 (gitleaks red since 2026-09-06) is the same class. Every later hop is
dead too: the instance's weekly `docs-sync.yml` ran once, built nine versions
(+450,589 lines), pushed an orphan branch, was refused a PR, and its guard has
exited 0 since; the local sync scripts point at a departed contributor's home
directory. `VERSION` read 0.15.0 at every tag from v0.16.x to v0.21.25 and
lagged again at v0.22.10 to v0.22.12; nothing compares it with the tag.

| What the reader meets on memql.io | Evidence |
|---|---|
| Bundle 0.21.25, labelled "engineVersion 0.15.0"; fourteen current pages absent, among them the docs home and the tutorial the README links | the site's `versions.json`; `README.md:19,63,116` |
| Every relative link 404s: 647 to 718 in-tree `.md` links, 42 into `docs/internal` and `docs/superpowers`, 34 leaving the docs tree | `_bundle.py` copies bytes; `docs_relative_links_test.go:84` checks only that files exist |
| Two self-canonical duplicate trees, 116 `/docs/latest/*` URLs, descriptions cut at a hard wrap, no `/llms.txt` | the site's routing and nav code |
| "Get started" fetches `memql-cockpit-<os>-<arch>`, unpublished since cockpit 0.9.0 | `curl -sIL` on the asset returns 404 |
| A retired product: a voice node type, a hand-drawn five-node topology, absent vendors, the cockpit as a terminal IDE, a downstream product as "living proof" | node types are agent, bff, edge, identity, mcp, planner, workbench (`ls app/build_*.go`) |

The content is mostly sound and weakest where an evaluator lands.

| Area | Pages | Current | Drifted | Wrong | Misplaced |
|---|---|---|---|---|---|
| overview | 10 | 6 | 2 | 2 | 0 |
| concepts | 16 | 9 | 4 | 2 | 1 |
| language | 16 | 11 | 4 | 0 | 1 |
| ai, build | 7 | 5 | 1 | 0 | 1 |
| operate (with auth) | 72 | 53 | 5 | 2 | 12 |
| **Total** | **121** | **84** | **16** | **6** | **15** |

- `concepts/architecture.md`, where `README.md:119` sends evaluators, is a
  2026-03 compiler tour; positioning is told five ways; `events.md` documents
  `si.completion.*` topics the code does not emit, while `Kind.String()`
  returns `si_*` values (`event.go:146,155-160`); `env-vars.md` lists 119 of
  387 names.
- `deployment-console.md` and `memql-os.md` put deploy control in a cockpit
  view deleted on 2026-08-25, not the VS Code Deployments panel; a draft
  proving log ships as public; seven pages carry hand "Last Updated:" lines.
- The only full platform description is `CLAUDE.md:355-1958`; a quarter of
  overview, concepts and language (4,813 of 18,762 lines) is for contributors;
  `DOCS_STANDARD.md` names two readers, not the evaluator.

The engine repository is public, yet `GLOSSARY.md:10-11` calls `docs/internal`
"not published". Seven public pages name a customer (one through a string at
`component/language/annotations/registry.go:494`), design records carry a
customer's store domain and private repository names, and about ten carry
per-tenant cost figures. No credentials, keys, GUIDs or addresses were found.

The architecture model has no consumer. `topology.model.json` is 81 MB,
regenerated in 272 of 2,768 commits since 2026-08-27, 69% of the 399 MiB pack;
its call graph is 63.9 MB. Its one renderer, the cockpit's Topology navigator,
went on 2026-08-25; `component/architecture/CLAUDE.md` still names it.

The cockpit, now the fleet worker runtime and cluster CLI installed as
`memql`, keeps six current operator pages (2,750 lines) and a README install
section: seven unpublished sources with no front matter, while
`docs/public/cockpit/` holds only `.gitkeep`. Its printed strings name a
downstream product and the retired Portal.

The Cluster app has no Overview; `v1:cluster:node.capabilities` is `[]` on all
65,116 live rows and `v1:cluster:nodeType` has no writer; four sections carry
standing "Read again" buttons against `clients/os/DESIGN.md`. The template has
no docs standard, front matter or lane, and `cmd/docs-gen` never mounts
runtime domains, so an instance cannot generate its own reference.

## 2. What the tree already has

- **DOCS_STANDARD and its gates.** Six front-matter keys, `audience: public`
  the site gate; fifteen-plus root tests per markdown change: a closed area
  set, links, retired vocabulary, `memql` fences parsed by the real loader,
  make-target citations, proving claims, the deprecation window rendered by
  `deprecation_window_docs_test.go`, `TestNoDatabaseProductClaims`.
- **Generated pages.** Grammar and vocabulary (`make docs-grammar`), the
  attribute matrix (`make docs-matrix`), the proving scorecard;
  `cmd/docs-gen`'s one concept catalog is gitignored, in bundles only.
- **Sources of truth no page derives from.** The env-var manifest; topic
  constants and the proto `EventKind` enum; `dslspec`, the annotations
  registry, `reserved.json`, the functions catalog; 263 `@relationship`
  annotations; `component/memql/os_navigation.json`; `ENGINE_NODE_TYPES`,
  `app/build_<type>.go` and `ValidNodeTypes`; `component/node/routing.go`;
  seven Deployments under `deploy/k8s`; the front-door generators; four proto
  services; the identity mux literals.
- **The bundle and publish path.** `build-docs-bundle.sh` runs `cmd/docs-gen`,
  then `_bundle.py` copies bytes; `publish-docs-bundle.yml` uploads
  `docs-<version>.tgz` on `release: published`. No PR builds it.
- **The site as an instance client surface.** memql.io is
  `clients/memql-website` in `memql-znas`, a static deployable in its
  `memql-package.yaml`; a push to the instance's `main` reaches
  `POST /inbound/github`, and Deployables' auto-run is the expected,
  unverified path to a rebuilt site.
- **The pipelines program** (`2026-09-16-pipelines-program-design.md`). The CI
  bridge (#5476) closed on 2026-09-27; M0 (#5697) and epics #5477 to #5480 are
  open, with no docs stage and no `release` event.
- **The architecture model.** `make arch-model` regenerates it
  byte-identically; `component/architecture` is base tier and takes data,
  never an upward import.
- **The OS map kit and the automationGraph pattern.** `kit/Overview`,
  `usePanZoom`, `MapControls` and `MapHeading` serve Fleet, Deployables and
  Nexus under `clients/os/DESIGN.md`; the `automationGraph` executor
  (`component/memql/automation_graph_read.go`) projects virtual rows on a
  never-persisted `v1:platform:automationNode`. The deleted cockpit navigator
  (memql-cockpit `b0545ac^`) is the reference drill-down: a breadcrumb zoom
  stack, per-child badges, a `codeMetric` overlay.

## 3. Decisions

Issue bodies cite the owner's answers as "owner decision N", N always one of
the session's nineteen answers, named beside the decision that records it; the
two binding decisions are cited as D1 and D2.

### D1 -- memql.io serves the current release only

One self-canonical URL per page, `/docs/<area>/<slug>`, with no version
segment, list, dropdown or archive. The docs layout renders the manifest's
version in the chrome and as
`<meta name="memql-docs-version" content="<version>">` on every docs page; the
docs home is the bundle's `overview/index.md`. Old `/docs/latest/*` and
`/docs/0.21.25/*` URLs get one release cycle of `noindex` meta-refresh stubs
to the new URL, then nothing: nothing old is served or listed (owner decision
2). The bundle stays a GitHub Release asset per release, never listed by the
site. A cycle of ranking loss on 116 URLs is accepted.

### D2 -- Documentation follows the engine, template and instance split

The engine's `docs/public` is the single public tree, machine-side pages
included (D11). The template carries the conventions and the docs,
product-reference and site-sync lanes. Each instance keeps its runbooks, cost
facts, release records and generated reference as `exposure: instance`, never
bundled, and links to the current public page, never a version URL. The public
site is a client surface of the instance that hosts it. An engine
docs-convention change names the template as its second consumer, through a
`standardVersion` the template lane compares.

### D3 -- The spine of DOCS_STANDARD stands; areas change additively, then rename

Front matter is the gate and the site is generated per release. Phase 1 adds
`architecture/`, `reference/` and `contribute/` and renames nothing, so old
URLs map 1:1 before the visit. Phase 2, one release later, renames `overview/`
to `start/` and `concepts/` to `data-model/` and prunes the flat 72-page
`operate/`. `DOCS_STANDARD.md` sections 1 and 5 and `VERSIONING.md` are
rewritten to the current-only, cross-repository contract.

### D4 -- One positioning sentence and one license claim, rendered by a test

Two constants live in `core/positioning`; the sentence is owner decision 1:

> MemQL is an open-source AI platform built on a time-series memory graph:
> typed data, queries and mutations, model routing, durable agent work and
> event-driven automations, declared in one `.memql` language and run on a
> cluster you own.

The license claim says "open-source" until counsel decides. A test-as-renderer
in the `deprecation_window_docs_test.go` pattern writes both into `README.md`,
`what-is-memql.md`, line 1 of `CLAUDE.md`, the VS Code site, the bundle
manifest and the internal `timescaledb-license-compliance.md`; counsel's
answer is a one-line change plus a claims gate.

### D5 -- A documentable fact is generated or gated, never typed

Every public page gains `description` (the meta and `llms.txt` line), `kind`
(`generated`, byte-compared; `gated`, every fact a token a test resolves;
`narrative`) and `exposure` (`engine` or `instance`; public plus instance is
refused in the engine). A facts gate resolves `MEMQL_*` names, node-type
words, event topics, `app:` capabilities, make targets and OS app names
against registries; narrative pages may name closed-set tokens. Drafts, hand
dates and footers are refused under `docs/public`; design records must carry
`status`. The reference is committed under `docs/public/reference/` from
`cmd/docs-gen`, checked by a root test. One status YAML feeds both the
engine's app table and the instance's, which may name third-party products.

### D6 -- One diagram source: Mermaid from a platform graph the OS also reads

Diagrams are Mermaid text from `cmd/docs-gen`, committed under
`docs/public/reference/diagrams/` and included by reference, so GitHub, the
site and the OS share one file: deterministic, diffable, native to GitHub, no
renderer binary in CI, which neither PlantUML nor the D2 language offers.
Figures are capped at 60 nodes and 120 edges. The source is a
`platform.graph.json` embedded in `component/architecture`, written by
`cmd/platformgraph` in the root module from node roles (a new
`component/node/roles.go`, tested against `ValidNodeTypes` and
`ENGINE_NODE_TYPES`), rendered `deploy/k8s`, proto descriptors, the front
door, routing, `@relationship` edges, the automation graph and the OS
navigation. The Cluster app reads the same graph (D10).

### D7 -- The call graph leaves the tree

The model drops the call graph, 63.9 of 81.3 MB with no consumer (owner
decision 7). `make arch-model` commits the structural model;
`TestArchitectureModelIsNotStale` builds the call graph on demand in the
go-tests lane and skips it in the docs-only lane, cost recorded against #5697.

### D8 -- Public safety is a gate; the engine is neutral everywhere

`docs_public_safety_test.go` runs over every public page, `README.md`,
`CONTRIBUTING.md`, `SECURITY.md`, `GLOSSARY.md` and `clients/os/DESIGN.md`,
with an allowlist in the `.gitleaks.toml` convention, and again at release. It
refuses links leaving `docs/public`; operator hostnames and cloud resource
names without a placeholder, GUIDs, public addresses, ruleset ids, customer
and private-repository names, money outside owner-designated price sheets,
break-glass words; and public pages marked `exposure: instance`. Neutral
everywhere (owner decision 6): the customer rule also covers `docs/internal`
and `docs/superpowers`, and customer records move to their private
repositories with acme placeholders. MemQL Cloud is the ZNAS instance's
product (owner decision 5): its five pages and the cost text move there.

### D9 -- Five readers, and the evaluator's twenty minutes

Five readers: evaluator, product builder, operator, engine contributor, answer
engine. The evaluator's path, on Phase 1 URLs: the memql.io home (the
sentence, the L1 diagram, proof rows with D17's caveat); `what-is-memql`; the
new `architecture/` area; a new `overview/maturity` (releases, security
workflows, the license claim, what is preview); the proving scorecard; a new
`overview/how-memql-is-organized` (the repositories, what is open and what is
yours, acme examples), which hands the builder to `build/product-repo.md`.
Each first paragraph answers its title in under 60 words.

### D10 -- The Cluster app gets an Overview, a per-type map and a topology read

Overview becomes the first and default section: Figures from reads the
sections already make, never zero before they settle, the running engine
version among them, and the Delivery breakdown moved from Mesh. Below them, a
mesh map with a pure, tested layout: one card per node type (owner decision
12), front door and database as anchors, edges aggregated per type pair, the
static platform graph the frame and live rows the truth. A type opens a page
with Replicas, Wiring, Configuration (the per-type version pins, moved from
Settings > Cluster) and Packages. The read is `clusterTopology` in
`dsl/cluster/builtins.memql`, over a never-persisted `v1:cluster:topologyNode`
in the `automationGraph` pattern. The "Read again" buttons go (owner decision
17); a `RefreshButton` shows only when a read failed or a feed is behind.

### D11 -- Machine-side pages under operate/machines/, the wire contract under build/

The cockpit's pages become `docs/public/operate/machines/`, a subgroup like
`operate/auth/`, and its computer-use wire contract
`build/computer-use-protocol.md`; each machine page links its cluster-side
page both ways, and `memql --help` is the only command reference. The
`cockpit` area value goes in the commit that adds the pages. The access page
is rewritten as shipped (memql#5181 and #5165 closed on 2026-09-08). The
cockpit repository keeps a short README, Apache 2.0 its license of record, and
deletes its `docs/*.md` after the move. The site's `/cockpit` page becomes the
machine-runtime page (owner decision 8): `CockpitConsole.tsx` is replaced by
the L1 diagram's machine edge, the cockpit repository's GitHub description
changes the day the page ships, and voice is "Ask and voice in MemQL OS".

### D12 -- VERSION equals the tag; the trigger stays release: published

The release cut (`integrations/release/cut.go`) writes `VERSION` in the
release commit and refuses a mismatch; the bundle build refuses too, and
`engineVersion` leaves the manifest. The trigger stays `release: published`; a
second would be a second way to publish. `scripts/docs/current-check.sh`, a
capability script under the contract test, fails weekly when the newest tag
has no asset or the site's version meta differs (docs-current PR 2).

### D13 -- The sync job commits directly to the instance's main

The instance's `docs-sync.yml` runs on dispatch, hourly and by hand; replaces
the current set with the newest release's asset; runs typecheck and build in
the job, since a `GITHUB_TOKEN` push triggers no workflow; and commits to
`main` (owner decision 3), the ruleset confirmed before the PR opens and an
App-token PR the fallback only. A post-sync probe fails loudly if the live
version meta has not changed within ten minutes; a coherence check fails when
the version is more than a patch from `ENGINE_REF`. The `repository_dispatch`
step (docs-current PR 5) uses the GitHub App token if housekeeping finds it
covers dispatch; the hourly poll serves until then.

### D14 -- The docs epics run ahead of pipelines P17 to P20

The evaluator arrives before pipelines M1 can (owner decision 4), so the docs
epics run first, as P26 to P32. Their gates ride the root package; the cost is
recorded against #5697 and may not push a Go PR past M0's 7 minutes.

### D15 -- Docs is the fifth pipeline stage, filed against the open pipelines tasks

| Pipelines task (comment filed now; built when epics 2 and 3 land) | Amendment |
|---|---|
| #5487, #5488 | `release` joins the event-to-mode table |
| #5489, #5490, #5493, #5495 | the docs stage compiles like any stage; the bundle, manifest and `llms-full.txt` become Library files of the run |
| #5499, #5500, #5504, #5505 | the Runs tab shows the docs stop; the notification carries the docs URL; verify-rollout probes the version meta |
| #5506 (M1) | five stages: checks, tests, docs (generator, graph and model checks, the bundle; on push and release), deploy, notify |
| #5508, #5509 (M3, M4) | at M3 the deploy stage commits the docs set, and `publish-docs-bundle.yml` and the instance's `docs-sync.yml` retire before `ci.yml` |

### D16 -- The owner keeps the merge script; M2 stays unmeasured (against the recommendation)

The recommendation was the merge queue, to give M2 traffic. The owner keeps
`scripts/dev/merge-as-owner.sh` (owner decision 18); the last `merge_group`
run was 2026-09-17, so M2 stays unmeasured while merges bypass the queue. The
commit that adds this record says so in the M2 row of the pipelines record's
section 7, and #5507 carries the note, filed with the D15 comments.

### D17 -- The proving headline stays replay-only (against the recommendation)

The recommendation was a live `proving-live.yml` run. The owner chose replay
only (owner decision 10); every proof row states "replayed from recorded
responses in CI; live tier not yet run" verbatim, the scorecard the only
record. The home's proof rows carry it under the site's brand gate
(docs-current PR 6); `proving_claims_test.go` requires it of each proof claim
in `docs/public` and `README.md` (docs-readers PR 2).

### D18 -- The event kind strings become ai_*

`Kind.String()` returns `ai_event` and `ai_completion_*` (owner decision 9). A
pre-release wire change, no shim: `clients/os`, `sdk/ts`, `sdk/go` and
`editors/vscode` move in the same commit, whose body has the frontend note.

### D19 -- The token install is the documented path

Docs and the cockpit's strings lead with the one-liner MemQL OS composes in
Fleet > Machines > Add machine (owner decision 11). `memql worker pair` gets
one neutral sentence, a product's Settings card becomes "your product's
Settings", and a cockpit neutrality test keeps product names out.

### D20 -- The OS host is os.<domain>

MemQL OS is served at `os.<domain>` (owner decision 14); the template README
changes to say so in docs-template PR 1.

### D21 -- Forge is a public pack

`operate/forge.md` moves under `build/` (owner decision 15).

### D22 -- The threat model and the design records are published after review

`auth-threat-model.md` is published after the owner's redaction pass,
exploitable specifics moved to a private note in the instance repository
(owner decision 16). After the safety sweep docs-readers PR 4 promotes
platform consolidation, module taxonomy, the mesh delivery ADR and
forwarded-auth contract, account isolation, deployment v2, seven language ADRs
(construct invocation, core builtins, behavioral constructs, import model,
spec-shape binding, event-payload binding, operator standardization), inbound
and outbound delivery, the capability-script contract, merge queue, ruleset
baseline, CI design, TimescaleDB compliance, the DR runbook, and the
pipelines, work-spine, DSL v1 freeze and deployables records as design
records; `auto-generated-diagrams.md` becomes the architecture method page.

### D23 -- contribute/ is public on both surfaces

Workflow, testing, image builds, branch rules, release cutting, CI design and
the docs standard become `contribute/` (owner decision 13), promoted from
`CLAUDE.md`, which links to it; D8 keeps ruleset ids and bypass paths out.

### D24 -- The remaining defaults

Accepted as recommended (owner decision 19): `docs/internal` keeps its name;
`acrmemql.azurecr.io` stays in public manifests; pack means Go and DSL under
`packs/`, bundle means runtime-mounted product DSL, "carrier repo" is retired,
all gated; node roles are services in `arch.yaml`; Settings > Cluster keeps
domain, issuer, mail and IdP, the version facts moving to the Overview (D10),
which opens at the app door; no human proving log page, so the 2026-08-20 log
goes; dated visual-QA logs carry `status: record`; the template owns the body
of stamped `ONBOARDING.md` and `CLAUDE.md`, the product an appendix;
`engine-version-watch.yml` also rewrites the `?ref=` pin and imports the
release manifest; no offline OS-hosted docs yet.

## 4. The seven epics

### Epic 1 -- Docs current (priority P26, `epic:docs-current`)

Six PRs and housekeeping. PR 1 (memql): the verify step greps a captured
listing; a guard test beside `scripts/release/build_workflows_test.go` refuses
`grep -q` on a tar pipe in any workflow; the 0.23.5 asset backfilled; the bug
filed beside #5663; code-owner review for `/.github/`. PR 2 (memql): `VERSION`
equals the tag, and the current check (D12). PR 3 (memql-znas): the cut (D1),
the version meta checked in `out/docs/index.html`. PR 4 (memql-znas): the sync
job (D13). PR 5 (memql): bundle contract v2, a `cmd/docs-gen` subcommand
replacing `_bundle.py`: public, stable, `exposure: engine` pages (a missing
key reads as engine until docs-readers PR 2); slug links, a link leaving
`docs/public` failing the build; `llms.txt`, `llms-full.txt`, a sitemap
fragment; manifest fields `positioning`, `nodeTypes`, `providers`, `apps` and
`diagrams`, filled by docs-readers PR 2 and docs-generated PRs 1 to 3; a check
mode running the boundary gate, docs-safety PR 1 adding the safety rules; the
dispatch step (D13); `docs_public_boundary_test.go` and
`docs_bundle_build_test.go`; `DOCS_STANDARD.md` section 5 and `VERSIONING.md`
rewritten. PR 6 (memql-znas): site copy and `/cockpit` from
`what-is-memql.md`, a Mermaid renderer, manifest readers, `/llms.txt`, search,
a brand gate over D17's sentence; the home switches to the constant, the
arrays and the L1 diagram at the day-8 release. Housekeeping (no code PR): the
predecessor site archived; the cockpit description changed the day PR 6 merges
(D11); the App token checked for dispatch.

Exit: a release reaches memql.io with no hand step and no relative link 404s.

### Epic 2 -- Docs for their readers (priority P27, `epic:docs-readers`)

Five PRs. PR 1, the prose truth sweep: deploy-control text,
`why-memql-harness.md`, `events.md`, the proving log deleted (D24), hand
dates, Cockpit framing, dead variables, carrier-repo pointers, retired
vocabulary (among it `si.completion` and `SI_COMPLETION`). PR 2:
`core/positioning` (D4), the README led by the why, the page contract and
facts gate (D5), the proving tier check (D17). PR 3, the evaluator's path
(D9): the organization and maturity pages, the `architecture/` area,
`concepts/architecture.md` cut to a hub. PR 4: `contribute/` (D23),
`build/product-repo.md`, argument resolution, the actor envelope to
`language/`, internal-only pages demoted, the promotions of D22, Forge under
`build/` (D21), `GLOSSARY.md` regenerated. PR 5: the kind rename (D18).

Exit: the sentence renders from the constant, every public page passes the
page contract, and the evaluator's six pages render on both surfaces.

### Epic 3 -- Generated docs (priority P28, `epic:docs-generated`)

Three PRs. PR 1: `roles.go`; `cmd/platformgraph` with its eight passes and a
staleness gate copied from the model's, with make targets to regenerate and
check it; the call graph un-committed (D7); stale model claims rewritten. PR
2: Mermaid for L1 context, L2 topology, node-type responsibilities,
per-namespace ERDs, the automation graph and four bounded sequences. PR 3: the
committed reference -- concepts, env vars (the six `MEMQL_WS_*` registered
first), events (today's `Kind.String()` values, regenerated by docs-readers PR
5 under its check mode), identity routes, OS apps with status, node types,
gRPC messages, reserved names and functions, the status page, provider tables,
and the glossary; `GLOSSARY.md` is generated here only, so later wording
changes edit the generator's input; a `docs-gen` make target and its check.

Exit: `go test ./ -run 'TestDocs|TestPlatformGraph|TestReference'` is green on
`main` and every generated page regenerates byte-identical.

### Epic 4 -- Docs safety (priority P29, `epic:docs-safety`)

Three PRs. PR 1: the safety test and allowlist (D8), with the closed-set
checks extended to every tracked `CLAUDE.md`. PR 2, the removals, owns its
receiving side: on day 8 a memql-znas PR adds the five MemQL Cloud pages, the
cost text and `infrastructure.md` under `docs/operate/` as
`exposure: instance`, and the customer repositories receive their design
records; only then does the engine delete them, neutralise the registry
string, regenerate, and change the "not published" wording in the glossary
generator's header. A release is cut the same day. PR 3 (memql-cockpit):
printed strings, pairing wording and install scripts made neutral (D19), with
the pinning tests and a neutrality test; no other task touches them.

Exit: the gate passes with no customer, cost or instance entry allowlisted.

### Epic 5 -- The Cluster overview (priority P30, `epic:cluster-overview`)

Four PRs (D10). PR 1, rule fixes: the "Read again" buttons, report-age
captions and the Active fact removed; separate scrollers in Agents;
`LocalTabs` and one `ActionBar` in Data origins; dead selectors deleted; the
app's README rewritten. PR 2, the engine read: `clusterTopology`, its concept
and executor, `capabilities` in the heartbeat, with `make concept-snapshot`,
`make sdk-gen-check` and conformance in the same PR. PR 3: the Overview, the
per-type map and the node-type page, with D10's version facts; tests mirroring
Fleet's; a gated `operate/cluster-app.md`. PR 4, deferrable: Automations on
the map composition.

Exit: Overview is the default, with two-replica parity-cluster screenshots.

### Epic 6 -- The docs template (priority P31, `epic:docs-template`)

Three PRs. PR 1 (memql-project): `docs/README.md` with the conventions and
`standardVersion`; the operate, releases and generated-reference skeleton;
`scripts/docs/engine-tool.sh`, cloning the engine at `ENGINE_REF` because
`go run module@tag` cannot resolve its `replace` directives; a docs lane with
the EPIPE fix upstreamed, skipping `--dsl-root` and the `standardVersion`
comparison while the engine lacks them; an opt-in site-sync lane keyed by
`DOCS_SITE_PATH`, docs-current PR 4's workflow with the same behaviour (D13);
drift over `docs/` and stamped prose; the README's `os.<domain>` (D20). PR 2
(memql): `--dsl-root` and `--package` for `cmd/docs-gen`, mounting a product's
domains as the runtime does; `standardVersion` in `DOCS_STANDARD.md`. PR 3
(memql-znas, then the other instance repositories): the template merged,
runbooks into `docs/operate` as `exposure: instance`, release records
generated, the local sync workflow replaced by the template lane, front matter
added to the pages docs-safety PR 2 moved in if the lane requires it.

Exit: a fresh stamp's docs lane passes, and after the `ENGINE_REF` bump to the first release carrying `--dsl-root`, the instance generates its own reference.

### Epic 7 -- Machine-side docs (priority P32, `epic:docs-machines`)

Two PRs (D11). PR 1 (memql), after Epic 1's PR 5: `operate/machines/` (index,
install, computer use, local models, local apps, watched folders, macOS menu,
access) and the protocol page, front-mattered and cross-linked; the engine
deep-links repointed; `docs/CLAUDE.md`, `DOCS_STANDARD.md` and the glossary
input changed together; the `cockpit` area retired in the same commit. PR 2
(memql-cockpit), after PR 1 and docs-safety PR 3, which made the strings
neutral: the README reduced, its license fixed; `docs/*.md` deleted.

Exit: the next bundle has the machine pages; the cockpit README links them.

## 5. Failure modes

- **The publish lane fails silently again, or VERSION lags.** The guard test,
  the cut's refusal, the weekly check and the post-sync probe fail loudly.
- **A bundle change first runs at release.** `docs_bundle_build_test.go`
  builds it on every PR touching docs or the bundler.
- **A link leaves the public tree.** The boundary gate refuses it on the PR
  and at release; the temporary allowlist only shrinks.
- **The sync fails to build, or `main` refuses it.** A failed build leaves
  `main` untouched; the ruleset is confirmed first, an App-token PR the
  fallback.
- **Deployables' auto-run does not fire.** The first dispatch verifies it; if
  not, the deployable runs by hand and the gap is filed.
- **The site documents an engine the instance does not run.** The coherence
  check fails the sync.
- **The day-1 cut republishes customer names.** No bundle is hand-edited; the
  removals and a release land on day 8, a week in.
- **A moved page exists nowhere for a while.** docs-safety PR 2 merges the
  receiving PRs before the engine deletes.
- **The day-4 site reads fields no release carries yet.** Its copy comes from
  `what-is-memql.md`; the readers switch at the day-8 release.
- **A generated page, a diagram or the platform graph drifts.** Check modes
  regenerate and byte-compare; diagrams over the caps fail; the graph gate
  applies the model's drift ceiling.
- **The event kind rename strands a consumer.** All move in the same commit.
- **The mesh map is a hairball, or replicas disagree.** One card per type; the
  answering node is labelled.

## 6. Testing

- Each new root gate ships with a seeded fixture that fails it. The bundle is
  judged by `docs_bundle_build_test.go` on PRs and its check mode at release;
  each generator by its byte-comparing check under one root test; the platform
  graph by its staleness gate; the model by `make arch-model-check`.
- The site is judged in the sync job: typecheck, build, every Mermaid fence
  parsed, the brand gate, the probe.
- OS work is judged by rendered screenshots at desktop and narrow width, light
  and dark, empty and populated, per `clients/os/DESIGN.md`, on the
  two-replica parity cluster (`make up SERVERS=2`, then `make scale N=2`),
  never populated by minting nodes.
- The template is judged by its lane on a fresh stamp, the cockpit by its
  neutrality test, and the whole path on day 14 by a release cut to exercise
  it, ending with `curl -s https://memql.io/docs/` showing the tag.

## 7. Milestones

| Day | Date | Epic and PR | What lands |
|---|---|---|---|
| 1 | Mon 28 Sep | docs-current PR 1, 2, 3 | the verify fix and backfill; VERSION equals the tag; the site current-only |
| 2 | Tue 29 Sep | docs-current PR 4; docs-readers PR 1 | the sync job dispatched, 0.23.x live; the prose truth sweep |
| 3 | Wed 30 Sep | docs-current PR 5; housekeeping | bundle contract v2; the predecessor site archived |
| 4 | Thu 1 Oct | docs-current PR 6; housekeeping | site copy from `what-is-memql.md`, Mermaid, `llms.txt`, search; the cockpit repository's description changed |
| 5 | Fri 2 Oct | docs-readers PR 2 | the constants, the page contract, the facts gate |
| 6-7 | Sat 3 - Sun 4 Oct | docs-generated PR 1, 2; docs-readers PR 3; cluster-overview PR 1 | the platform graph and diagrams; the evaluator's pages; Cluster rule fixes |
| 8 | Mon 5 Oct | docs-generated PR 3; docs-safety PR 1, 2 | the committed reference; the safety gate; the moved pages received, then removed; a release, and the home switches to the manifest |
| 9 | Tue 6 Oct | cluster-overview PR 2 | `clusterTopology`; `capabilities` populated |
| 10-11 | Wed 7 - Thu 8 Oct | docs-readers PR 4; docs-machines PR 1 | `contribute/`, moves and promotions; `operate/machines/` |
| 12 | Fri 9 Oct | cluster-overview PR 3; docs-readers PR 5; docs-safety PR 3; docs-machines PR 2; docs-template PR 1 | the Overview and map; the rename; the cockpit repository; the template lanes |
| 13 | Sat 10 Oct | docs-template PR 2, 3 | the instances merged, the reference step skipped until a release carries `--dsl-root` |
| 14 | Sun 11 Oct | a release; cluster-overview PR 4 if time allows | asset to instance to memql.io end to end; the machine pages live; the instance's `ENGINE_REF` bumped to this release, after which it generates its own reference |

Phase 2 follows one release after the visit: D3's renames and prune, a second
cycle of stubs. Deferred: the per-package drill-down and its metrics (they
need a code-map projection); more sequence diagrams; offline OS-hosted docs;
the OS-app guide at `build/os-apps.md`; PlantUML and the D2 language (never).

"Documentation is current" holds when all five hold: the `memql-docs-version`
of `https://memql.io/docs/` equals the newest tag, as
`scripts/docs/current-check.sh` checks against
`gh release view --repo znasllc-io/memql --json tagName`;
`go test ./ -run 'TestDocs|TestPlatformGraph|TestReference'` is green on
`main`; the newest `publish-docs-bundle.yml` run is green; the coherence check
passes; and the evaluator's six pages and diagrams render on both surfaces.

## 8. Issues

Filed with this record, all `claude`-labeled, each epic carrying its
`epic:<slug>` label and its program priority. The D15 and D16 pipelines
comments went with them; the commit adding this record amends the M2 and M3 rows of the
pipelines record's section 7: M2 stays unmeasured while merges bypass the
queue (D16), and M3 retires `publish-docs-bundle.yml` and the instance's
`docs-sync.yml` (D15). Area labels live on
the issues, as the union of each epic's tasks.

| Epic | Issue | Tasks |
|---|---|---|
| 1, docs current | #5712 | #5713 verify step, guard test, backfill; #5714 VERSION equals the tag; #5715 site cut to current-only; #5716 sync job rewritten; #5717 bundle contract v2; #5718 site copy and search; #5719 housekeeping, no code PR |
| 2, docs for their readers | #5720 | #5721 prose truth sweep; #5722 positioning constants and page contract; #5723 the evaluator's path; #5724 audience moves and promotions; #5725 event kind rename |
| 3, generated docs | #5726 | #5727 node roles and the platform graph; #5728 Mermaid diagrams; #5729 the committed reference |
| 4, docs safety | #5730 | #5731 the safety gate; #5732 the removals; #5733 the cockpit's strings |
| 5, the Cluster overview | #5734 | #5735 rule fixes; #5736 the topology read; #5737 Overview and mesh map; #5738 Automations on the map |
| 6, the docs template | #5739 | #5740 template skeleton and lanes; #5741 docs generation for a product's DSL; #5742 instances merge the template |
| 7, machine-side docs | #5743 | #5744 `operate/machines/` and the protocol page; #5745 the cockpit repository reduced |

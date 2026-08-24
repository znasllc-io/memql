# Zero-key installation: no AI provider key to install, providers configured in the portal, federation as the recommended path

- **Date:** 2026-08-23
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project O** of the 2026-08-23 backlog brief (VS Code + install/release batch)
- **Owner ask:** "Why does the installer ask for a provider key -- is it really
  required to start the cluster, and why? Installation should always be free of
  LLM inference. AI provider configuration is not required to start the
  cluster; it should happen in the portal, where -- now that Anthropic
  federation is implemented -- federation is the recommended, default and
  preferred way. Figure out the env vars and configuration actually needed to
  install, start, uninstall, repair and upgrade the cluster; that minimal
  configuration is the standard, engine-required vars stay, everything else is
  configured in the portal."

## What is actually true today (verified 2026-08-23)

The engine does NOT need a key; the WIZARD demands one.

- **Engine:** a provider whose `env()` placeholder resolves to nothing loads as
  **unavailable** -- registered, skipped at selection, with a message naming how
  to seed it (`component/memql/ai_anthropic_federation.go:388-400`,
  `ai_providers.go:399`). Nothing refuses boot over a missing key. The ONE
  refusal is deliberate and stays: a PARTIAL Anthropic federation config (some
  of the four ids) refuses boot rather than falling back (epic memql#4333) --
  zero config is legitimate, half config is a mistake.
- **Auth resolution is already runtime-capable:** provider auth resolves
  `globalSecret -> globalVariable -> OS env`
  (`ai_providers.go:917-960`), including the federation fields
  (`ai_anthropic_federation.go:42`), with the seal-floor name aliasing
  (`MEMQL_AI_OPENAI_API_KEY` <-> `MEMQL_OPENAI_API_KEY`, memql#4338). And
  `ReloadAIProviders` (`component/memql/engine_bootstrap.go:652`) exists to
  re-resolve after seeding -- built for dev-refresh, documented as callable
  post-rotation.
- **Wizard:** `requiredFields` (`editors/vscode/src/state/addCluster.ts:320-370`)
  lists `provider` + `providerKeyFile` as REQUIRED for install AND repair. The
  install graph runs `providerKey` (`scripts/install/graph/install.json`:
  `install.verifyProviderKey`) -- an authenticated `GET /v1/models` against the
  vendor (spends no tokens, but is a vendor call and demands a key) -- and
  `seedBootstrap` writes the key into the cluster.
- **`seed-bootstrap.sh` already tolerates absence:** `stage_provider_key`
  returns cleanly when neither `--provider` nor `--provider-key-file` is given
  (`scripts/install/seed-bootstrap.sh:241-244`). The hard requirement is
  wizard-side + graph-side only.
- **Portal:** no AI-provider configuration surface exists. `setGlobalSecret` /
  `setGlobalVariable` mutations exist (`dsl/platform/mutations.memql:30,63`)
  with backend encryption; `component/memql/provider_auth_check.go` implements
  the per-provider auth diagnosis the `memql provider-auth check` CLI uses.

So the epic is: drop the wizard/graph requirement, make keyless boot
first-class and quiet, build the portal page that owns provider config, make
seeding take effect without a restart, and write the minimal-envelope contract.

## Decisions

### D1 -- The wizard's AI fields become an optional, collapsed section

- `requiredFields` drops `provider` + `providerKeyFile` for `install`,
  `installGuided` AND `repair`. The collect screen moves both fields into a
  collapsed disclosure: "AI provider (optional -- configure later in the
  portal)". Supplied -> exactly today's behavior (verify, then seed). Empty ->
  nothing is verified, nothing is seeded.
- The graph's `providerKey` step is SKIPPED when no key was supplied, reported
  as `skipped` with reason "no key supplied -- configure AI providers in the
  portal", and `seedBootstrap` receives no provider args (which it already
  handles). The step is not deleted: an operator who DID supply a key keeps the
  fail-fast verification exactly where it is.
- The done screen gains a third hand-off line when nothing was seeded:
  "Configure AI providers" opening `https://portal.<domain>/settings/providers`
  (the D4 page). Repair keeps prefilling from the receipt when a key WAS
  recorded (memql#3544's editable-box reasoning is untouched); it simply stops
  demanding one that never existed.
- Prose sweep: `install.json`'s seedBootstrap description ("...and the AI
  provider key...") and `addCluster.ts` field comments rewritten to the
  optional model.

### D2 -- The guarantee, stated and gated: lifecycle verbs are vendor-silent

Install, start, repair, upgrade and uninstall make ZERO calls to any AI vendor
when no key/federation is configured; when a key IS supplied at install, the
one vendor call is the existing authenticated models-list probe (no tokens
spent). Nothing in boot, migrations, seeding or first-boot automations
requires a provider: providers load unavailable, prompts/policies degrade per
the `@disabled`/fallback semantics already documented, and anything
LLM-shaped that fires later fails soft per llm-cost-control's layers.

Gates: (a) the install-e2e lane runs KEYLESS BY DEFAULT (a keyed variant
remains for the verify path), so a future step that quietly demands a key
breaks CI, not an operator; (b) a graph test pins `providerKey.skippable`;
(c) a boot test brings the engine up with no `MEMQL_AI_*`/`MEMQL_*_API_KEY`
vars and asserts healthy + providers-unavailable + no WARN-level spam (D3).

### D3 -- Keyless boot is quiet: one INFO line, not a warning per provider

Today each unresolved provider logs its own registered-as-unavailable warning.
With keyless as a first-class state, boot emits ONE summary line at INFO --
"AI providers not configured (N unavailable); configure in the portal:
Settings -> AI providers" -- and the per-provider detail moves to DEBUG plus
the `provider-auth check` surface. A PARTIAL config (some resolve, some do
not) keeps per-provider WARNs: half-configured is the state worth shouting
about. The federation all-or-none boot refusal is unchanged.

### D4 -- The portal owns provider configuration: Settings -> AI providers (owner-only)

New portal page, owner-gated (`actor.isClusterOwner`), backed by gRPC-over-WS
constructs only:

- **Status:** a new builtin `providerAuthStatus` (virtual projection, the
  `dataOrigins` pattern -- never persisted) returning per configured provider:
  name, vendor, model, available/unavailable, auth source
  (`federation | globalSecret | globalVariable | env | unresolved`), and the
  reason string -- the `provider_auth_check.go` machinery exposed to the
  console instead of only to a kubectl exec.
- **Anthropic, recommended path = federation:** the page leads with workload
  identity federation (the four ids + token-file path seeded as
  `globalVariable` rows -- the resolver chain already covers them), links the
  runbook (`docs/public/operate/auth/anthropic-federation.md`), and states the
  all-four-or-none rule inline. API key entry exists beneath it as the
  local/dev alternative.
- **API keys:** entered once, write-only -- sealed via the existing
  `setGlobalSecret` path under the exact names the resolver tries
  (`MEMQL_AI_ANTHROPIC_API_KEY`, `MEMQL_AI_OPENAI_API_KEY`); the page never
  reads a key back, it shows only the fingerprint/status. A "Verify" action
  runs the server-side probe (Go reuse of the models-list check inside
  `integrations/`/`provider_auth_check`, NOT the install shell script) and
  reports pass/fail before the operator leans on it.
- **Apply:** a "Reload providers" action invoking D5. The page shows each
  node's post-reload availability via `providerAuthStatus`.

### D5 -- Seeding takes effect without a restart, on every node

`ReloadAIProviders` is promoted from dev-refresh tool to a production seam:

- **Make the swap safe under traffic first:** build the new registry fully,
  then swap it atomically under the registry lock (its own comment names the
  non-atomic swap as the caveat; that caveat is retired here, not inherited).
  In-flight calls finish on the old registry snapshot.
- **Cross-node by explicit plumbing** (the multi-node rule: nothing travels
  implicitly): an owner-gated builtin `providersReload` publishes a
  `providers.reload` event carrying a broadcast routing rule
  (`node.RegisterRoutingRule`, the `component/node/routing.go` precedent the
  fleet rows use); every node's subscriber runs the safe reload and logs its
  outcome. The portal's Apply calls this builtin. No automatic reload on every
  globalSecret write -- an explicit Apply keeps rotation a decision with an
  audit line (`v1:identity:auditEvent` `providers_reloaded`, requestedBy).
- The cluster-e2e harness gets the hop test: seed a key on one node's write
  path, reload, assert a DIFFERENT node's `providerAuthStatus` flips --
  the test that fails against single-node-assuming code.

### D6 -- The minimal envelope is a written, tested contract

- `docs/public/operate/env-vars.md` gains "The minimal install envelope":
  exactly what install/start/uninstall/repair/upgrade require (domain +
  the operator/master key pair + DSNs/identity URLs the overlay derives --
  enumerated from `component/envregistry/manifest.yaml` + `bootvalidate.go`
  as the source of truth, not restated numbers), with everything AI-related
  listed as portal-configured. The manifest entries for every `MEMQL_AI_*` /
  federation var are audited to `optional` for every node type (voice's
  OpenAI requirement stays what it is today: the voice lane is already gated
  to zero replicas without ITS credentials, and that gating is LiveKit's, not
  this epic's).
- `bootvalidate` gets a test asserting a bff/agent node boots with exactly the
  minimal set. CI's keyless install-e2e (D2) is the end-to-end proof.

## Testing

Named per decision above; the load-bearing ones: keyless install-e2e green
with `providerKey` skipped-with-reason; engine boots keyless with one INFO
line; non-owner refused on `providersReload` and on the page's constructs
(real engine actor, per the fake-engine-has-no-gates memory); the cross-node
reload hop test; seal-floor alias resolution covered for the exact names the
portal writes; federation partial-config still refuses boot.

## Out of scope

- OpenAI federation (their side does not offer it yet; the page's structure
  leaves the slot, the copy says "hopefully soon").
- Per-partition/tenant BYOK (`partitionSecret` exists; this page is
  instance-global on purpose).
- Removing `verify-provider-key.sh` (still the fail-fast for the
  key-supplied install path).
- Any change to the LLM cost-control layers.

## Risks

- A reload seam under traffic is new surface: bounded by build-then-swap, the
  drain semantics, owner-only reach, and the audit line. Rollback is "don't
  call it" -- boot-time resolution is untouched.
- Quieting keyless boot could hide a MISCONFIGURED provider; D3's rule keeps
  per-provider WARNs whenever ANY provider resolved (the half-configured
  case), so silence only ever means "none configured", which is the state the
  portal page now owns.
- Operators who scripted around the wizard's required fields lose nothing:
  supplying the fields still works identically (pre-release product, no
  compat shims needed regardless).

## Task breakdown (preview; tasks carry the acceptance criteria)

1. Wizard + graph: optional fields, skipped `providerKey`, done-screen
   deep-link, repair un-requirement, prose sweep.
2. Keyless boot quieting (one INFO line; partial config keeps WARNs) + boot
   test.
3. `providerAuthStatus` builtin + safe `ReloadAIProviders` swap +
   `providersReload` broadcast + routing rule + audit line + hop test.
4. Portal Settings -> AI providers page (status, federation-first config,
   sealed key entry + verify, Apply/reload).
5. Minimal-envelope doc + manifest audit + bootvalidate test + keyless
   install-e2e lane flip.

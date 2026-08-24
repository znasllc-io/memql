# Cluster onboarding: a sorted version picker with Latest preselected, main that builds from source, and a domain-first Add Existing Cluster

- **Date:** 2026-08-23
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project M** of the 2026-08-23 backlog brief (VS Code + install/release batch)
- **Owner ask (two bullets, one wizard):**
  1. "The install form's versions are not sorted correctly; there should be a
     latest option that also displays the version number, selected by default.
     The 'main' option today says node images are not built from main -- I don't
     want this; main is so developers with memql repo access can check out main
     and build the local instance from the latest main to test locally."
  2. "Add an existing cluster should ask a display name and a domain (e.g.
     memql.example.com -- the owner's example used their own real domain, which
     the vendor-name gate keeps out of this tree; memql.localhost must not be
     allowed, because this option is for non-local clusters); the rest
     auto-populates from the domain with defaults; an Advanced option reveals a
     full form for manual population."

## Current state (verified 2026-08-23)

**Versions.** `src/install/tags.ts` lists release tags via `git ls-remote`
against `DEFAULT_STACK_REPO`, parses `vX.Y.Z` only, and sorts NUMERICALLY
newest-first (`compareSemverDesc` -- the sorting itself is correct). The wizard
then breaks the order: `dedupeKeepingDefault`
(`src/webview/installScreens.ts:504-510`) hoists the CURRENT value -- the pin
`DEFAULT_STACK_TAG = "v0.19.1"` (`src/install/stackPin.ts:142`) -- to the TOP of
the list, so the picker reads `v0.19.1, v0.20.x, v0.19.2, ...`. That hoist is
the "not sorted correctly" the owner sees. There is no "Latest" labeling, and
`tags.ts`'s header states the old doctrine outright: "IT NEVER AUTO-SELECTS THE
NEWEST, and neither does anything downstream."

**main.** `MAIN_BRANCH_CHOICE` (memql#3901) is a deliberate SKEW: main's
checkout (manifests + scripts) with the newest RELEASE's node images, because
`build-engine-images.yml` publishes only on release dispatch and no `main`
image exists in GHCR. `imageTagForVersion` (`stackPin.ts:186-191`) maps main ->
latest release images and the picker label says so
(`installScreens.ts:535-552`). Meanwhile the from-source machinery already
exists on another verb: `memql.deployments.rebuildFromCheckout` (memql#4246)
builds node images from a local checkout via the `k3d.dev` capability, imports
them into k3d, and `state/imageLane.ts` narrates the released-vs-checkout lane
crossing. The install graph is `scripts/install/graph/install.json`
(stackCheckout -> clusterUp -> seedBootstrap -> ...), with `rebuild.json`
beside it carrying the build-from-checkout step shape.

**Add existing cluster.** The `connect` screen (memql#3475,
`src/state/addCluster.ts:189-291,754-911`) asks name, domain (OPTIONAL),
endpoint (derivable via `composeEndpointFromDomain(domain)` = `api.<domain>:443`),
token (optional, discouraged in favor of "MemQL: Sign In"). Nothing refuses a
localhost-family domain; the endpoint box is a first-class field rather than an
advanced override.

## Part 1 -- the version picker

### D1 -- Strictly newest-first; the hoist is retired

`dedupeKeepingDefault` is removed. The picker renders the listing exactly as
`compareSemverDesc` orders it. A current value the listing does not carry (a
pin newer than a partial listing, a typed tag) is INSERTED IN ITS SORTED
POSITION -- the guarantee-present property survives (a `<select>` silently drops
a value that is not among its options), the queue-jumping does not.

### D2 -- "Latest -- vX.Y.Z (recommended)" is the first entry and the default

- When the listing succeeded, the first entry is
  `Latest -- v0.20.3 (recommended)` whose VALUE is that newest tag (no
  sentinel: what is submitted is a real tag, so `installPlan`, `imageTagFor`
  and the receipt all see an ordinary version). The same tag does not appear
  twice (the labeled entry replaces the bare newest row).
- `DEFAULT_INPUTS.version` becomes empty-meaning-latest: the collect screen
  seeds the field from the listing when it arrives (`values.version` <- newest)
  unless the operator has already touched the field. Offline / empty listing:
  the existing degrade-to-typing path stays, prefilled with
  `DEFAULT_STACK_TAG`, whose role NARROWS to "the offline fallback the
  extension was built against".
- **The prose is swept with the decision** (house rule: never leave the
  argument standing over a reversed decision): `tags.ts`'s "never auto-selects"
  header, `stackPin.ts`'s "WHY A PINNED CONSTANT RATHER THAN THE NEWEST TAG"
  section, and `addCluster.ts`'s Inputs.version comment are rewritten to state
  the new split: the INSTALL form recommends latest (a fresh install wants the
  newest release, whose manifests and images ship together at that tag -- the
  stale-pin postmortems in stackPin.ts are all failures of NOT installing the
  newest); the pin remains as offline fallback; and the deployment page's
  MOVE-a-cluster tag picker KEEPS its no-preselection doctrine -- moving an
  existing cluster is the surface where a silent choice is dangerous, and that
  boundary is stated where both pickers can read it.

### D3 -- main builds from source, or it is not offered

`main` stops meaning the skew and starts meaning what the owner wants: a
developer lane that runs main's ENGINE, built locally.

- Label: `main -- build from source (for MemQL developers)`. The skew label and
  `imageTagForVersion`'s main -> latest-release-images mapping are DELETED
  (sweep memql#3901's prose in `stackPin.ts` and `installScreens.ts`).
- Selecting main changes the install plan: `stackCheckout` checks out `main`
  (`--branch`, as today), and a new `buildImages` step -- the `k3d.dev`
  capability `rebuild.json` already drives -- builds the node images FROM THAT
  CHECKOUT and imports them into k3d, ordered after `clusterUp` exactly where
  the rebuild flow runs it, so the cluster comes up on `memql-<node>:local`
  images built at main's commit. No GHCR release image is pulled for engine
  nodes on this lane.
- Preflight says what it costs: the collect screen's main hint states that this
  clones and BUILDS (docker required, several minutes); the `detect` step's
  docker check is already in the graph and `dockerAccess` already gates it.
- The Deployments row then narrates the lane truthfully for free:
  `state/imageLane.ts` already renders checkout mode with the commit and dirty
  count, and a main install IS checkout mode from birth.
- main is offered exactly as today only when a listing exists (an offline
  operator typing "main" gets the existing sentinel handling), and never as the
  default.

## Part 2 -- Add Existing Cluster, domain-first

### D4 -- Two primary fields: display name + domain; everything else derived

The connect screen's primary form becomes NAME + DOMAIN, both required. On
save, the entry derives: endpoint = `composeEndpointFromDomain(domain)`
(`api.<domain>:443`, the function that already exists and that
`identityBaseUrlFor` reads back), and identity/portal URLs continue to derive
from `domain` downstream exactly as the connection layer already does. The
domain field's hint shows the derivation live ("will connect to
api.memql.example.com:443").

### D5 -- Advanced is a disclosure, not a second screen

A collapsed "Advanced" section holds the ENDPOINT and TOKEN fields (prefilled
with the derived endpoint; the token keeps its existing PAT-refusal and
whitespace validation, `addCluster.ts:836-849`). Opening Advanced and editing
the endpoint wins over the derivation -- for the rare non-standard front door.
The existing `ClusterRegistration` shape and the omit-empty-optional-fields
rule (`addCluster.ts:241-246` -- never write `local:`) are unchanged.

### D6 -- The localhost family is refused HERE, by name

`validateConnect` refuses a domain whose apex or suffix is the local-install
family: `localhost`, `*.localhost` (RFC 6761), `127.0.0.1`/`::1`/IP literals in
the loopback ranges. Message: "That is a local install's domain -- use
'Install a local cluster' instead. This form registers a cluster reachable
over the network." The refusal lives in the validator (pure, `node --test`),
not in prose. `memql.localhost` remaining valid for the INSTALL form is
untouched -- the two flows deliberately diverge here and each says why.

### D7 -- Probe on save: warn, never block

After validation, the panel host probes the derived (or overridden) front door
-- `GET https://identity.<domain>/.well-known/jwks.json` and the endpoint's TLS
reachability -- and renders the result on the form. A failed probe WARNS
("could not reach api.<domain>:443: <reason>") with "Save anyway": an operator
may legitimately register a cluster that is currently down, and the localhost
family being banned removes the mkcert-leaf false-negative trap (Node's fetch
cannot verify the local mkcert leaf; public domains carry public chains). The
probe is host-side (needs network), behind an injected function so the state
module stays pure.

## Testing

- `node --test`: picker ordering (no hoist; sorted insert of unlisted current;
  Latest labeling + default selection; offline fallback prefilled with the
  pin); main plan shape (checkout at main + buildImages step present, no
  release image tag anywhere in the plan); connect validation (localhost
  family refused with the pointing message; name+domain required; derived
  endpoint; advanced override wins; probe-failure still saves with the flag).
- Graph tests (`scripts/install/graph/*_test.go`): install-from-main graph
  variant round-trips; step ordering buildImages-after-clusterUp pinned.
- `installDomain.test.ts` counterpart for the connect flow's refusal list.
- Manual: fresh install picking Latest lands the newest tag end to end (the
  receipt names it); a main install's pods run `:local` images at main's
  commit (`kubectl get pods -o jsonpath` in the checklist); add-existing with
  a real domain connects with only two answers typed.

## Out of scope

- Publishing `main`-tagged engine images from CI (rejected in memql#3901 and
  still rejected -- the from-source lane makes it unnecessary and a mutable
  image tag is its own decision).
- Changing the deployment page's move-a-cluster picker preselection (kept, by
  D2's stated boundary).
- Multi-remote listings (the repo asked is `DEFAULT_STACK_REPO`, unchanged).
- The release-cut automation (sub-project N) -- it produces the versions this
  picker lists, and the two land independently.

## Risks

- Latest-by-default re-opens the extension-scripts-vs-newer-checkout pairing
  question the pin existed to close. Mitigation, stated in the swept prose:
  the install runs the RELEASE's own scripts from its checkout for everything
  cluster-side; the extension's staged scripts are the bootstrap shim, and a
  skew there surfaces in `version/skewHint.ts` territory. The offline fallback
  keeps the reviewed pairing for machines that cannot list.
- A from-source main install takes minutes and can fail in a build; the wizard
  already renders per-step failure with remedy (`state/addCluster.ts` step
  model), and the buildImages step reports through the same envelope
  (`rebuiltMessage` reads it today).

## Task breakdown (preview; tasks carry the acceptance criteria)

1. Picker: retire the hoist, Latest-first + default, offline fallback, prose
   sweep in tags.ts/stackPin.ts/addCluster.ts, boundary note on the move
   picker.
2. main-from-source: plan change + buildImages graph step + label/prose sweep
   + preflight hint.
3. Connect form: name+domain primary, Advanced disclosure, localhost refusal,
   derivation hint.
4. Probe-on-save (warn, never block) + wiring + tests.
5. Docs: extension README onboarding section reflecting both flows.

# Local Models on the Fleet (Epic 3) -- Design

- **Date:** 2026-08-26
- **Status:** approved (brainstorm session with the owner)
- **Scope:** engine + protocol in this repo (`component/grpc` worker
  protocol, provider registry + AI guard in `component/memql`, fleet router
  in `integrations/agent/worker/`, `dsl/providers` + `dsl/policies` +
  `dsl/router`, planner park path, portal surfaces); discovery/runtime half
  in the `memql-cockpit` repo; install-time detection in
  `scripts/install/` + the VS Code extension repo. Same split as local apps
  (epic memql#4358): **this repo fixes the protocol and the engine side.**
- **Siblings:** Epic 1 (Synapse -- consumes the same cost-visibility
  values), Epic 2 (portal surfaces render arrangement-native).

## Why

Owner's brief: if a user's machines can run local models, there is no reason
to pay for API keys except when a task genuinely needs a stronger model. The
MVP is local models running the platform's operations -- planning,
conductor/routing, suggestions, embeddings -- by sending model calls to
fleet machines over the already-open cockpit stream, with cloud providers
(Anthropic/OpenAI) reserved for explicitly policy-routed needs. The DSL's
provider/policy layer already expresses per-purpose selection; this epic
gives it local targets, a transport, honest accounting, and surfaces.

Second half of the brief (added mid-session): MemQL is an intelligent
system, so **using it requires inference** -- but starting it does not. The
portal gates first use on having inference configured (local by default);
the engine, the installer, and the VS Code plugin's install / uninstall /
repair / update flows must all work with no model and no key anywhere.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | V1 runtimes | Ollama discovered natively; any OpenAI-compatible endpoint declared in the machine's `policy.yaml` (covers LM Studio, vLLM, llamafile) |
| D2 | Availability gap | PARK, never silently fall back to a paid API: cloud runs only via an explicitly authored policy fallback or per-request consent |
| D3 | Quality tiers | Authored per purpose in DSL policies, exactly like cloud providers today |
| D4 | Accounting | New billing class `local`: dollar cost zero, excluded from the dollar ceiling, INCLUDED in loop caps; `plan.tokenSpentLocal` mirrors the subscription counter |
| D5 | Workbenches | No models on workbenches -- machines only |
| D6 | Install-time | Local installs detect Ollama and offer wiring + install/update/repair assist; automations stay deterministic-first, reasoning only on failure |
| D7 | Inference requirement | USING the portal requires configured inference: a local model (default), an Anthropic or OpenAI API key, or the Anthropic workload-identity federation (memql#4333) |
| D8 | What never requires inference | Engine boot, install, uninstall, repair, update (installer + VS Code plugin) -- all fully functional with no model and no key |
| D9 | Portal first-run gate | In order: passkey enrolled, then inference configured; only then the console |
| D10 | Local hardware floor | Apple Silicon on macOS; discrete GPU on Linux (table in G) |

## A. Machine side (cockpit repo; contract fixed here)

- **Discovery:** Ollama probed at its default endpoint (installed models,
  context length, parallelism); additional runtimes declared in
  `policy.yaml` as OpenAI-compatible base URLs with a model list. No other
  per-vendor integrations in v1.
- **Advertisement:** models ride the existing registration/heartbeat
  capability mechanism (the `app:<id>` precedent): `model:<modelId>`,
  `runtime:<name>`, per-model attributes (context window, structured-output
  capable, embeddings capable, max concurrent). Refreshed on heartbeat;
  stale machines fall out via the existing 30s online window.
- **Owner allowlist:** `policy.yaml models.allow` gates what a machine
  offers, exactly like `apps.allow`. Not allowed or not signed-in => not
  selectable, mirroring the app-session rule.
- **Serving:** the cockpit executes a model call against the local runtime
  and streams deltas back; it enforces its own per-call concurrency cap and
  reports usage (prompt/completion tokens) when the runtime provides it.

## B. Provider type `fleet`

- **DSL:** one base provider (`@base @type("Fleet") provider fleet {}`) in
  `dsl/providers/providers.memql`. No static per-model children: fleet
  models are DYNAMIC.
- **Catalog:** the live model list is a virtual projection (the
  `v1:router:modelCatalog` pattern -- never persisted): every model at least
  one online, allowed, signed-in machine advertises, with its attributes
  and the machines behind it.
- **Policy reference:** a policy names a fleet model as
  `@primary("fleet:llama3.1:8b")` (or `@fallback`), resolved against the
  catalog at selection time. Selection treats "no online machine offers
  this model" and "model lacks a required capability (e.g. structured
  output) for this prompt" identically: the provider is UNAVAILABLE (see D).
- **Capability gating:** structured-output prompts (conductor, planner,
  suggest domains) select only fleet models advertising structured-output;
  embeddings route only to embeddings-capable entries. A capability miss is
  an availability miss, not an error the user has to interpret.

## C. Transport -- the worker stream

- **Envelope:** new `ModelCall` family on `WorkerService.Stream`:
  `ModelCallStart` (model, messages/prompt payload, params, request id),
  `ModelCallDelta` (streamed tokens; monotonic `seq`, out-of-order/duplicate
  dropped -- the AppSession chunk rule), `ModelCallEnd` (finish reason,
  usage), `ModelCallCancel`. Timeout + keepalive semantics defined on the
  envelope, not left to callers.
- **Selection:** the fleet router (`integrations/agent/worker/router.go`)
  asked for `model:<id>`, with the existing strategies; `leastLoaded` uses
  the advertised per-model concurrency as the cap. Machines on a sibling
  replica are reached via the existing `WorkerForward*` mechanics
  (memql#4352); `refused_before_start` stays the only re-pick predicate.
- **Ownership boundary (security):** a model call carries the acting user's
  prompts, so it routes ONLY to machines owned by that user. Calls with no
  acting user (system automations, cluster maintenance) route only to
  machines whose OWNER opted them in via an operator label
  (`sharedInference=true` on `operatorLabels` -- the no-reconnect-overwrite
  map). No cross-user routing exists otherwise.

## D. Selection, parking, and consent -- no silent cloud spend

- A call whose policy primary is a fleet model, with no eligible machine
  online and no authored fallback, returns the typed refusal
  `no_local_model_available` naming what was considered (the
  `no_worker_available` reporting pattern).
- **Plans park** on it: `awaitingFeedback` with
  `feedbackReason=no_local_model_available` (the budget-park precedent).
  The card offers: wake a machine, or approve cloud for this plan (only
  when a cloud provider is configured).
- **Interactive surfaces** (suggest, Synapse, compose) surface the refusal
  with a one-shot "use cloud this once" consent affordance -- never an
  automatic switch.
- **Authored fallback stays authored:** a policy that names
  `@fallback("streamClaudeSonnet")` gets exactly that, because the operator
  wrote it down. Seeded default policies for planner/conductor/suggest/
  embeddings are LOCAL-FIRST WITH NO CLOUD FALLBACK; cloud-quality policies
  are present but opt-in per purpose.
- **Guards wrap the fleet path too:** the process-wide rate ceiling and
  identical-request breaker currently live at the provider HTTP chokepoint
  (`component/memql/ai_guard.go`); they move to (or are duplicated at) the
  provider-registry call seam so fleet calls pass the same gates. A runaway
  loop on a free model is still a runaway loop; per-plan token/call budgets
  count local calls (D4).
- **Out of scope:** realtime voice stays on its cloud path (OpenAI
  realtime); no local STT/TTS in v1.

## E. Accounting

- `v1:router:call.billing` gains `local` beside the epic-4358 classes;
  `executionSurface` names the machine. Silence still falls to `unknown` --
  never inferred.
- `plan.tokenSpentLocal` alongside `tokenSpentSubscription`: excluded from
  the dollar ceiling, included in loop caps; absent-not-zero where the
  runtime reports no usage.
- Synapse's token float (Epic 1) renders local-billed calls in the accent
  tone rather than red -- spent, but not money.

## F. Portal surfaces (arrangement-native per Epic 2)

- **Cluster -> Providers:** the live catalog -- each fleet model, its
  machines and online state, capability chips; cloud providers listed as
  today. The page states plainly when the cluster is running fully local.
- **Policy editor:** per purpose, the chain in words: "planner: local
  llama3.1 -> park" / "vision: Anthropic". Editing writes the same policy
  records DSL authors write.
- **Fleet machine cards:** model chips (runtime, models, busy state) --
  already in the Epic-1 visual direction.
- **Plan cards:** the `no_local_model_available` park renders with the
  wake/approve-cloud actions.

## G. The inference requirement, onboarding, and the hardware floor

**Where the requirement lives.** The ENGINE does not change: it boots,
serves, and migrates with no provider configured (D8), and features that
need inference already refuse or park typed (D). The requirement is
enforced where a person USES the system -- the portal:

- **First-run gate**, after sign-in, in order:
  1. **Passkey.** If `passkeysForSelf` is empty, the portal walks the user
     through enrolment (the existing WebAuthn ceremony on the identity
     origin) before anything else.
  2. **Inference.** If the cluster has no eligible inference source, the
     portal presents the three doors, local first: **Run a local model**
     (pair a machine via the existing add-machine flow + Ollama guidance,
     or detect models on already-paired machines), **Use the Anthropic
     federation** (status read from the memql#4333 config -- if it is
     already set up, this check passes silently), **Enter an API key**
     (Anthropic or OpenAI, stored the way provider auth is stored today).
  3. The console. The gate re-checks live: if the only local machine goes
     offline later, the portal does not eject the user -- inference-needing
     features park per D, and a rail-level notice says why.
- **Eligibility** = at least one of: an online fleet machine advertising a
  model that meets the platform's minimum capability profile (structured
  output + the default context floor), a configured cloud provider key, or
  a complete federation config. The check reads the same catalog and
  provider registry the router reads -- no second implementation.
- **Auth-disabled clusters** (`MEMQL_IDENTITY_ENABLED=false`,
  troubleshooting only): the passkey step does not apply (no identity);
  the inference gate still renders but is skippable, matching that mode's
  existing "you are not really signed in" posture.

**Hardware floor for the local default** (published in docs and checked by
the cockpit's model discovery -- a machine below the floor is not offered
as an inference machine, with the reason named):

| Platform | Minimum (supported) | Recommended |
|---|---|---|
| macOS | Apple Silicon (M1+), 16 GB unified memory, macOS 13+ | M2 Pro+ / 32 GB for 8B-class at comfortable latency |
| Linux | x86_64 + discrete GPU with >= 8 GB VRAM (CUDA/ROCm) | 12-16 GB VRAM |
| CPU-only / Intel Mac | Not supported as an inference machine (still a full worker for everything else) | -- |

**Model floor:** the default operational class is a 7-8B instruct model
with structured output (llama3.1:8b / qwen2.5:7b class) plus a small
embeddings model where embeddings route locally. The catalog's capability
gating (B) is what enforces this per call; the floor is what the
onboarding flow recommends to install.

## H. Install-time (LOCAL installations only) -- inference-optional, always

- `scripts/install/install-{mac,linux}.sh` + the VS Code extension's
  install/update/repair flows probe for Ollama; when present, offer:
  register this machine's models (cockpit wiring) and seed the local-first
  default policies. Decline leaves cloud-key seeding as today.
- **Install, uninstall, repair, and update NEVER require inference**
  (D8): every step of those flows is deterministic and completes with no
  model and no key. When a local model happens to be wired, the assist
  lane those flows already have may use it -- so a failed migration or
  repair can reason locally at zero spend -- but its absence changes
  nothing about whether the flow completes.
- Automations remain deterministic-first: reasoning enters only on failure
  paths, as the automation philosophy already requires; this epic adds no
  ambient AI to automations.

## Testing

- Protocol: in-process hop test for `ModelCall` forward
  (`forward_hop_test.go` precedent -- real router wired to real handler);
  delta ordering/duplicate-drop; cancel + timeout.
- Router: `model:<id>` label selection, ownership boundary (user-owned
  only; `sharedInference` opt-in for system calls), capability gating,
  leastLoaded with per-model caps.
- Catalog: projection from registrations (online window, allowlist,
  attribute mapping); park refusal naming considered machines.
- Guards: rate ceiling + breaker exercised over a stubbed fleet provider;
  budget counting of local calls; billing class + `tokenSpentLocal`
  bookkeeping (absent-not-zero).
- Portal: catalog rendering, park card actions, policy chain display; the
  first-run gate (passkey step ordering, the three inference doors,
  eligibility reading the real catalog/registry, skippability only in
  auth-disabled mode, live re-check turning into a notice rather than an
  eviction).
- Install flows: an assertion test that install/repair/update paths make
  no inference call and gate on none (D8).
- Cockpit-side runtime tests live in `memql-cockpit`.

## Rollout

Cross-repo sequencing: protocol + engine land first (a cockpit that does
not advertise models changes nothing), then cockpit discovery/serving, then
installer/VS Code wiring. In this repo, 2 PRs:

1. **Engine:** `ModelCall` envelope + router labels + ownership boundary +
   catalog + `fleet` provider type + guard relocation + billing/accounting
   + park path.
2. **Surfaces + seeds:** portal Providers/policy/park surfaces + the
   first-run onboarding gate (G) + seeded local-first policies + installer
   script probe.

The `memql-cockpit` and VS Code extension work is filed in their repos,
referencing this spec.

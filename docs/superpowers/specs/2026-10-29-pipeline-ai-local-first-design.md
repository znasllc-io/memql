---
title: Pipeline AI on local models first, then apps, then a paid vendor
audience: internal
status: draft
area: operate
sinceVersion: 0.31.8
owner: znas
---

# Pipeline AI: local models first

Owner brief, 2026-10-28. The AI in MemQL pipelines -- the independent PR
review, the diagnosis of a failed run, and the repair change a diagnosis
requests -- should run on the fleet's local models first, then on a
subscription app, with a paid vendor as the balance. On the production
installation that did not happen.

## What happened (production, 2026-10-28)

A release run failed and pipeline maintenance opened its diagnosis goal. The
diagnosis prompt is a structured call at `@level("reasoning")`, so the locked
rule `reasoningParks` chose `federationStrongest`, and Auto re-ordered that
chain for the call. The walk:

1. `app:*` -- refused `execution_destination_requires_permission`.
2. `fleet:<27B model>` on a machine running Ollama 0.33.3 -- `400 Failed to
   initialize samplers: failed to parse grammar`.
3. `fleet:<4B model>` on a second machine running Ollama 0.33.3 -- the same
   400.
4. `fleet:<embedding model>` -- selected as a chat model; every machine
   refused it ("answers embedding calls only").
5. The paid door -- nothing to call. The installation has no vendor
   configured (`providerAuthStatus`: no provider available).

The run parked on `inferenceUnavailable`. Its decision record called the two
local failures `other`; the Ollama text was readable only on the work run's
observations. The PR review an hour later went to the app door and passed.

## Why it fails today

1. **Local models.** Ollama 0.33.3 could not turn the call's JSON response
   schema into a sampling grammar. The same schemas (the diagnosis and the PR
   review) compile and run in the llama.cpp that Ollama 0.40.6 bundles, through
   both its `json_schema` and chat paths, thinking on and off. Three MemQL
   defects turned one runtime's limitation into a parked goal:
   - The Cockpit falls back to asking for the JSON in the prompt only when the
     runtime refuses `format` outright (501). A runtime that cannot compile the
     schema (400) fails the call.
   - That failure arrives as a failure after start, so the dispatcher never
     tried the same model on the other two machines that offer it, one of
     them on Ollama 0.40.6.
   - `component/router` names an untyped error `other`, so the decision record
     hid the cause.
2. **Embedding model as a chat candidate.** Fleet eligibility reads the
   catalog's model-level capability flags, which are a union over every
   registration behind the model, online or not. One online machine's Cockpit
   predates memql-cockpit#551 and still advertises `tools=1,structured=1` for
   the embedder, and so do about fifteen old offline registrations. The
   embedding exclusion (memql#6238) therefore never fired at the catalog. The
   dispatcher checks each machine's own labels, which is why every machine then
   refused it.
3. **The app refusal.** The diagnosis runs inside a work goal that pipeline
   maintenance opened, with no route choice, so its execution destination is
   Auto. The app gate (`integrations/agent/worker/app_gate.go`) treats any app
   session inside such a run as moving the work onto the owner's computer,
   which needs the owner's explicit Apps route for that goal or a per-run
   approval. Nobody is present to give either. The PR review passed because it
   runs outside any work run. Yet a chat-door app turn with no images and no
   inputs is already **inference-only**: Claude Code runs with `--tools ""` and
   an empty strict MCP config, Codex app-server with shell, patch, multi-agent
   and web search off, and the delegated tool scope is empty. It cannot act on
   the computer, exactly like a local model, which the gate never asks about.
4. **The order.** `federationStrongest` is one of the three chains Auto ranks
   per call, and at `reasoning` Auto's fixed quality puts an app (0.8) above a
   local model (0.6). Pipeline prompts therefore try the app first.
5. **The paid balance.** There is no vendor on this installation. Not a code
   defect; see the owner actions.

## Decisions

Approved by the owner on 2026-10-29, with two choices made explicitly: an
embedding model is never a chat candidate, even when an old Cockpit claims it
can chat (D3); and a tool-free app turn skips the destination check in Auto
goals only, so an explicit Workbench goal keeps today's refusal (D4).

| # | Change | Where |
|---|---|---|
| D1 | A structured call whose schema the runtime cannot compile into a grammar falls back to the existing schema-in-prompt path: one retry on the same machine, answer returned only when it validates against the full schema, otherwise the call fails as today. "Cannot compile" means `/api/chat` answered 400 to a request carrying `format` and no tools, with "grammar" in the error, which Ollama answers before generating anything. Any other 400 fails as today. | Cockpit `modelcall` |
| D2 | A machine's failure keeps its Cockpit code (`runtime_error`, `timeout`, ...) as a typed error, so the decision record reads `failed when called: runtime_error: the machine's model runtime failed the call` instead of `other`. The Ollama text stays on `errorMessage` and the run's observations, never in `considered`. | `integrations/agent/worker`, `component/router/failure_reason.go` |
| D3 | Fleet eligibility is decided per online machine: a model is a candidate only through a machine that is online, allows the purpose in its model setup, and itself advertises every capability and the context window the call needs -- the test the dispatcher already applies. The union stays for display. An embedding model is never a candidate for a call that is not an embedding, whatever else an old Cockpit claims for it. Both the catalog check and the dispatcher's `ModelAttributes.Satisfies` change together. | `component/memql/fleet_provider.go`, `component/worker/fleetcatalog`, `fleetcatalog.Project` |
| D4 | An inference-only app turn in a run whose destination is Auto is a model call, not execution on the computer: the destination check is skipped for it, and only machines whose Cockpit declares that it runs that app without tools for such a turn are eligible (new `AppDescriptor.inference_only`, set for the `claude-headless` and `codex-app-server` harnesses; `codex-mcp` cannot honour it). Everything else in the gate is unchanged: the pin, the person, the kill switch, the owner's own machine, allowed and signed in. Turns with images or inputs, session-door steps and an explicit Workbench destination keep today's refusal. | `integrations/agent/worker/app_gate.go`, `app_inference.go`, `component/worker/apps.go`, `component/grpc/worker.proto`, Cockpit app descriptors |
| D5 | Work a pipeline started routes local first, in order. New call tag `pipeline`; new locked rule `pipelineLocalFirst` (`@when(level="reasoning", tag="pipeline")`, precedence 105, `@onUnavailable("park")`) naming a new policy `localFirstStrongest` = `fleet:strongest`, `app:*`, `federation:strongest`, which Auto does not rank. The tag reaches a call two ways: the independent review stamps it on its own call, and every model call of a work run whose goal arrived through a pipeline (`requestedVia: "pipeline"`: maintenance diagnosis, repository changes) carries it, read off the goal row by the replica executing the run. A goal that cannot be read refuses nothing: the run routes by the ordinary rules and the dispatcher logs why. | `dsl/rules`, `dsl/policies`, `core/airoute`, `component/memql/ai_attribution.go`, `core/common.RunContext`, `app/integrations_work_dispatch.go`, `app/pipeline_independent_review.go` |

### Context needs (D5)

The independent review renders at most 1 MiB. The context floor is the rendered
bytes over four plus a 4,096-token completion budget, so a full-size review
asks for 266,240 tokens. The largest local window in this fleet is 262,144
tokens, so such a review skips the local door by the existing floor and goes to
the app. Smaller reviews are tried locally when a machine holds the window; a
machine without the memory for it refuses at admission, and a machine with a
saved model setup caps the window at 32,768 tokens. A long review on a local
27B model can take minutes; that is the cost of local first, accepted here.

### Cross-node

- D4: the forwarded app call already carries the run id, images and inputs, so
  the holding replica re-runs the gate and reads its own worker's descriptor.
  Hop test in `integrations/agent/worker/app_forward_hop_test.go`.
- D5: the tag is derived on the replica that executes the run, from the goal
  row, never carried in memory from the replica that opened the goal. Test
  with a dispatcher that has none of the opener's context.
- D3 and D2 read only the catalog and the call's own result.

## Owner actions (not code)

1. Upgrade Ollama on the two machines running 0.33.3; the third (0.34.2) is
   untested. This fixes the observed failure before any release ships.
2. Update every fleet machine's Cockpit to the release carrying D1 and D4 once
   one is published from this repository. The machine on Cockpit 0.18.3 is the
   one whose labels mislead the catalog.
3. Configure a paid vendor (Anthropic or OpenAI workload identity federation,
   `docs/public/operate/auth/`), or accept that pipeline AI parks when local
   and apps cannot serve it.

## Not changed / known limits

- Not verified on Ollama 0.33.3 itself: reproducing needs that build. D1 does
  not depend on which schema feature it rejects.
- D4 helps only machines on a Cockpit that declares inference-only turns; until
  then the app door stays refused for pipeline goals, as today.
- The parked diagnosis from 2026-10-28 was not re-tried by the
  `inferenceUnavailable` sweep after its `resumeAt`; reported as a follow-up,
  not fixed here.
- Pipeline step placement, retries and failover are another session's work and
  are untouched.

## Tests

Each decision has a failing test first: the Cockpit grammar fallback (D1); the
decision line for a machine failure (D2); per-machine eligibility with a stale
online machine and offline registrations, and parity between the catalog and
the dispatcher (D3); the app gate for an inference-only turn on machines with
and without the descriptor, with images, on the session door and with an
explicit Workbench destination, plus the forwarded hop (D4); rule matching, the
unranked walk, both tag sources and the 1 MiB floor (D5). Then `make test` and
the db-gated trees the touched packages belong to.

## Docs

`docs/public/operate/ai-routing.md` (the rule and the tag),
`docs/internal/ops/harness-decisions.md` (decision map),
`cockpit/docs/local-models.md` (the grammar fallback),
`cockpit/docs/local-apps.md` and `docs/public/operate/local-apps.md`
(inference-only turns and the destination), and the comments beside each
changed branch.

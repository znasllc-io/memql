# Fleet inference completion, and subscription apps as an inference door -- Design

- **Date:** 2026-09-06
- **Status:** approved in the 2026-09-06 brainstorm as epic 3 of the inference and setup
  program (`2026-09-06-inference-and-setup-program.md`). The direction (D1-D4) was put to
  the owner as selectable options and answered; D5-D8 follow from it.
- **Repositories:** the engine half in `memql` (this record, `component/memql`,
  `component/router`, `component/worker`, `integrations/agent/worker`, `component/mcp`,
  `dsl/providers`, `dsl/policies`, `component/grpc/worker.proto`); the cockpit half in
  `memql-cockpit` (the same record, copied; `internal/worker/appsession`,
  `internal/worker/apps`, `internal/worker/models`, `internal/worker/modelcall`). The two
  halves share one proto change and land in this order: proto first in `memql`, then the
  cockpit, then the engine's use of the new fields.

---

## 1. Problem

The local-models epic (2026-08-26) built the engine half of fleet inference and left four
gaps, and the local-apps epic built Claude Code and Codex as task-execution surfaces only.
The owner wants three things:

1. **The fleet finishes.** A fleet model cannot serve a turn with tools, embeddings never
   route to the fleet (the seeded local embedding policy has no consumer), a structured
   call with a fleet default probably falls through to a paid provider (the one thing
   park-not-fallback forbids), and no call site uses any of the four local-first policies.
2. **A signed-in Claude Code or Codex on a fleet machine is an inference door**, selectable
   by a policy or a labelled request, with the whole loop running through MemQL so the
   person never leaves MemQL OS. Today the apps are reached only by the planner's
   delegation, as one task at a time, through an argv runner with stdin closed.
3. **The default routing order is local-strongest, then a fleet app, then federation**,
   automatically, under the cost ceilings; every door stays selectable; a person tunes
   routing rules in a later epic.

## 2. What the tree already has

### 2.1 Engine

- `@type("Fleet") provider fleet {}` with models named `fleet:<modelId>` in policies,
  resolved at selection time against a live catalog (`fleetEntry`, `EntryForUser`).
- `fleetProvider` implements chat, structured chat, streaming chat and embeddings, and NOT
  the tool-calling surface; the router's `resolveChain` skips a provider that lacks the
  modality it needs.
- `isNonStreamingType` in `ai_providers.go` knows `openai` and `anthropic` and not `Fleet`,
  so `InvokeAIStructured` with a fleet default provider falls through to the registry's
  cloud structured provider (unverified; the plan's first task is the test that proves or
  disproves it).
- `EmbeddingProvider` reads the registry by name and never resolves a `fleet:` name.
- `resolveChain` walks the policy chain; the only branch that substitutes a paid provider is
  `consentedCloudFallback` under `req.CloudConsent`; a fleet miss parks the work
  (`no_local_model_available`, `component/router/fleet_refusal.go`) with the cloud-approval
  affordance (`cloudApproved`) offered only when `HasCloudProviderConfigured` is true.
- Machine selection (`PlanModel`, `PlanSharedModel`) narrows by capability
  (`ModelAttributes.Satisfies`) and takes the first fit; no size or quality ranking exists
  because the fleet does not advertise size.
- `inferenceStatus` has three doors, `local`, `federation`, `apiKey`; no app door.
- App sessions: `AppSessionStart / Chunk / Control / End` on `WorkerService.Stream`;
  `CockpitAppExecutor.Run` mints a per-run `app_session` credential, selects a machine
  holding the app on this replica, and returns the transcript and artifacts; the delegation
  policy (`v1:worker:delegationPolicy`) decides when a task goes to an app; the runnable app
  set is closed in `component/worker/apps.go`.
- The MCP node serves tools, resources and prompts over streamable HTTP, resolves the actor
  from the app-session bearer, and is never an MCP client.

### 2.2 Cockpit

- The runner launches `claude -p <prompt> --output-format stream-json --verbose` or
  `codex exec <prompt>` with stdin closed; the only controls are cancel and renew; chunks
  are raw lines classified by "parses as JSON"; the end carries exit code, usage, the
  app's session ref and artifact ids, no final text; a Codex run yields no usage; the MCP
  config is `.mcp.json` (Claude Code, streamable HTTP with a bearer) or a `config.toml`
  under a private `CODEX_HOME` (marked unverified in the source); Claude Code reads its MCP
  config at startup so a mid-run credential renewal never reaches the process.
- Models: Ollama and declared OpenAI-compatible endpoints are discovered, floored, gated by
  `models.allow`, advertised as `model:<id>=ctx=…,structured=1,max=N` labels, and served
  through the `ModelCall` envelope.

### 2.3 What was verified on 2026-09-06

- MCP 2026-07-28: a server may make a request of a client only while that client is
  calling it; sampling and elicitation ride inside a call as a multi-round-trip result;
  standing bidirectional streams are gone. Claude Code does not implement sampling.
- Codex ships `codex mcp-server` (a `codex` tool that runs a session, `codex-reply` that
  continues one by thread id) and `codex app-server`, a JSON-RPC protocol over stdio,
  WebSocket or a socket with bearer auth, built for orchestration layers.
- Claude Code offers `claude -p` with `--resume`, `--output-format stream-json`,
  `--json-schema` for a structured final answer, `--mcp-config`; `claude mcp serve` exposes
  its file and shell tools, not "run a task"; automated use of a Claude subscription draws
  from the plan's limits and is permitted.

## 3. Decisions

### D1 -- The worker stream carries requests; MCP carries the app's calls back

Chosen over an MCP-only loop where a long-running app polls MemQL for work. Nothing can
dial a laptop; the cockpit's outbound stream already carries a session with chunks, a
control channel and a per-run credential; and an MCP-only loop would still need something
on the machine to start and babysit the process, would burn subscription tokens polling,
and would lose the live transcript and cancel the session envelope gives the OS. The
`nextTask` and `submit` tool pair exists as a NON-DEFAULT option for an app the person
started themselves and pointed at our MCP server.

### D2 -- The cockpit drives each app through its own harness protocol

Codex through `codex app-server` (thread continuation, structured events, usage) with
`codex mcp-server`'s two tools as the fallback where the app-server is not available;
Claude Code through resumable headless sessions (`claude -p --resume <ref>
--output-format stream-json --json-schema <schema>`) with one process per turn and the
session ref carried across turns. Parsing terminal output goes away. This is what gives the
engine a "send a follow-up into the same session" control and a structured final answer.

### D3 -- A `SubscriptionApp` provider type makes it an inference door

`@type("SubscriptionApp") provider subscriptionApp {}` in `dsl/providers`, models named
`app:claude-code` and `app:codex`, resolved at selection time like `fleet:` names against
the machines whose registration says the app is runnable (allowed and signed in). It
serves chat and structured calls (the app answers a prompt; the schema is the app's own
structured-output option) and NOT MemQL's own tool-calling turns: the app is the agent
there and MemQL is its tool provider over MCP, which is the inversion that makes the loop
close. Billing class `subscription`; the dollar ceiling excludes it; the loop caps include
it. `inferenceStatus` gains the door `app` once the type exists.

### D4 -- Default order: local-strongest, then app, then federation, under the ceilings

The shipped policies change from local-first-with-no-fallback to a three-step chain:
`@primary("fleet:*")` (any eligible local model, strongest first), `@fallback("app:*")`
(any runnable app), then the federated vendor entries. The federation hop is governed by
the existing dollar ceiling and loop caps; work parks only when every door is shut. The
park path in `component/router` (`fleet_refusal.go`, the `cloudApproved` affordance) parks
only when every door is shut and its approval affordance comes to mean "raise the ceiling
for this run"; `consentedCloudFallback` stays for policies that pin a fleet model with no
fallback. This replaces D2 of the local-models record as the DEFAULT; an authored policy
may still pin any order.

### D5 -- Strongest means parameters, then context window, overridable

The cockpit advertises `params=<count>` and `quant=<level>` on each model label from
Ollama's `details` (`parameter_size`, `quantization_level`, returned by `/api/tags` and
`/api/show` and parsed nowhere today); the engine's `ModelAttributes` gains both, as
ordering signals rather than capability gates (`Satisfies` is unchanged). A policy may
name `fleet:*`, which is new: the resolver picks the strongest eligible model in the
caller's catalog (parameters descending, then context window descending, then model id),
then plans machines for that model through the owner's existing routing strategy
(`orderCandidates`: first fit, round robin, least loaded, label match). A routing policy
field `modelPreference` (an explicit ordered list of model ids) overrides the default when
present. Missing attributes sort last, never first: a model that does not say how big it
is does not win by silence. The new attributes change every machine's label fingerprint
once, which costs one reconnect per machine on rollout.

### D6 -- The four gaps close, each with the test that proves it

Tool-calling on the fleet: `fleetProvider` implements the tool-calling surface by passing
the tool schemas to the runtime (`/api/chat` `tools` on Ollama, the OpenAI-compatible
shape elsewhere) and surfacing tool calls in the envelope; a machine whose runtime does not
support tools advertises `tools=0` and is skipped for a tool turn. Embeddings: the router
gains an embedding modality and `EmbeddingProvider` resolves `fleet:` names through the
same user-scoped path as chat. Structured: `isNonStreamingType` knows `Fleet` and
`SubscriptionApp`, and a test with a fleet default and no cloud provider proves no cloud
call is made. The local-first policies are wired to their purposes (planner, conductor,
suggest, embeddings) or deleted; none is left seeded and unused.

### D7 -- A `submit` tool and a follow-up control, and the session end stays the fallback

The MCP node gains `submit` (the app hands back a structured result for the session named
by its credential) and, for the non-default MCP-only participant, `nextTask`. The worker
protocol gains `AppSessionControl.action = "message"` carrying a follow-up prompt, and
`AppSessionEnd` gains `result` (the structured final answer, when the harness produced one).
A session that ends without a `submit` still yields its transcript and artifacts, as today.

### D9 -- The park re-homes onto the work spine

The planner's plan-driven loop and the `v1:planner:plan` rows it parked were retired on
2026-09-06 (the planner-retirement epics), so the park this record inherits from the
local-models design has no home. A run whose step needs inference with every door shut
parks as a `v1:work:approval` of kind `inferenceUnavailable` on its run, naming the doors
considered and the reason each is shut; it resumes when a door opens (the readiness feed
of epic 1 is the trigger) or when a person raises the ceiling through the approval, which
is what the old cloud-approval affordance becomes. `FeedbackReasonNoLocalModel` and
`CloudApprovedMetricKey` are retired with the rows that carried them. The executing
session reads the work-spine record's section D before touching this.

### D10 -- `app:` is one vocabulary in two places, on purpose

`app:<id>` already names a machine's routing label (`app:claude-code=<major.minor>`).
The provider reference `app:claude-code` uses the same word deliberately: a policy that
names it and a machine that advertises it are talking about one thing. The two never meet
in code (a provider reference resolves through the registry's `IsAppReference`; a label
lives on a registration), and the plan adds a test that the id set behind both is the
closed set in `component/worker/apps.go`.

### D11 -- Tool calling over the fleet is a wire change, on both sides

`ModelCallStart` carries no tools, `ModelCallMessage` is role and content only, and
neither `ModelCallDelta` nor `ModelCallEnd` can carry a tool call; the cockpit's runtimes
decode none. The gap closes with `tools` on `ModelCallStart`, `tool_call_id`, `name` and
`arguments` on the message, a tool-call list on the end and incremental arguments on the
delta, the matching Go envelopes in `component/worker` and the cockpit's `modelcall`,
`tool_calls` decoding for Ollama's `/api/chat` and the OpenAI-compatible stream, and then
`fleetProvider.CallChatWithTools`. A runtime that cannot do tools advertises `tools=0` and
is skipped for a tool turn. This is the largest single piece of the epic and the reason
the engine's PR 1 lands the proto before anything else.

### D8 -- The closed app set stays closed; its shape grows

`component/worker/apps.go` keeps the two ids; each gains a harness descriptor (which
protocol the cockpit drives it through, whether it supports a structured final answer and
follow-ups), reported by the cockpit in `Register` so a newer cockpit never makes the
engine attempt a protocol the machine lacks.

## 4. The change, by half

### 4.1 Engine (`memql`)

1. The structured fallthrough test, then the `isNonStreamingType` fix (D6).
2. `fleetProvider` tool-calling surface; `ModelAttributes.Tools`; label `tools=1`.
3. The embedding modality in the router; `EmbeddingProvider` through `EntryForUser`.
4. `ModelAttributes.Params`, `.Quant`; the label parser; `PlanModel` ordering;
   `routingPolicy.modelPreference`.
5. `SubscriptionApp` provider type, `app:` names, selection against runnable apps on
   machines this replica holds (the cross-replica forward for app sessions is a listed
   follow-up, not this epic), billing `subscription`, the `app` door in `inferenceStatus`.
6. The default policy chain (D4), the park rewrite, the `approve_cloud` action's new
   meaning ("raise the ceiling"), the cost-control doc updated.
7. Proto: `AppSessionControl.action="message"` with `prompt`; `AppSessionEnd.result`;
   `Register` app descriptors and model attributes; `AppSessionStart.responseSchema`.
8. MCP: `submit` and `nextTask` tools, gated to app-session bearers, session id from the
   credential's label.

### 4.2 Cockpit (`memql-cockpit`)

1. The Codex harness: `codex app-server` client (stdio; thread create, turn, events,
   usage, structured final answer), `codex mcp-server` fallback; the MCP config for Codex
   verified against a real Codex, and the `unverified` note removed or made true.
2. The Claude Code harness: one `claude -p` process per turn with `--resume`,
   `--output-format stream-json`, `--json-schema`, `--mcp-config` pointing at the written
   `.mcp.json`; stream events mapped to typed chunks; usage from the `result` event.
3. The runner: a session is a sequence of turns; `message` control starts the next turn;
   `AppSessionEnd.result` from the harness's structured answer; chunks typed
   (`event`, `text`, `tool`, `stdout`, `stderr`).
4. Models: `params` and `quant` from `/api/show`; `tools` capability from the runtime's
   declared support; label rendering and the parser tests.
5. `Register`: the app descriptors of D8.

## 5. Failure modes

- A machine asleep: the app door is not runnable, the chain falls to federation under the
  ceiling; nothing parks unless federation is shut too.
- A turn with tools and only fleet models without tool support: the fleet is skipped for
  that turn, not the whole plan.
- A harness process that dies mid-turn: the session ends `failed` with the exit code and
  the transcript so far; the engine's retry policy is the plan's, unchanged.
- A credential that expires mid-session: Claude Code's turn-per-process design makes the
  renewed `.mcp.json` effective on the NEXT turn, which is the honest form of what
  renewal could never do for a single long process.

## 6. Testing

- The negative control first: a structured call with a fleet default and no cloud provider
  makes zero cloud requests (an `httptest` cloud that fails the test if hit).
- Fold-style unit tests for ordering: parameters, then context, then registration order;
  missing attributes last; `modelPreference` overriding.
- Router tests for the three-step chain with each door open and shut; the ceiling refusal
  on the federation hop; the park only when all are shut.
- Proto round-trip tests for the new fields in both repos; the cockpit's harness clients
  against recorded app-server and stream-json fixtures; a cockpit-side fake Codex
  app-server for the turn sequence.
- The MCP `submit` tool: session id from the credential; a bearer of another class refused.
- `inferenceStatus` with an app runnable and nothing else: `doorsOpen == ["app"]`.

## 7. Delivery

Engine: two PRs. PR 1: the proto (tool calling on the model-call family, `message` and
`result` on the app-session family, app descriptors on `Register`), the structured
fallthrough fix, embeddings through the fleet, model attributes and `fleet:*` ordering,
`fleetProvider` tool calling against the new envelope. PR 2: the `SubscriptionApp` type and
door, the default chain and the work-spine park, the MCP tools, the engine side of
`message` and `result`. Cockpit: two PRs. PR 1: the pin bump to engine PR 1's `main` sha,
model attributes and the `tools` capability, `tool_calls` decoding in both runtimes,
`Register` descriptors. PR 2: the two harnesses and the turn-based runner. Order: engine PR
1, cockpit PR 1, cockpit PR 2, engine PR 2. The cockpit reaches the engine's proto through
`.github/memql-pin` plus the `replace` to the sibling checkout; the pin bump, any new
`require`/`replace` pair and `go mod tidy` land in ONE commit, and `memql-pin-guard` fails
any workflow that checks the engine out directly.

## 8. Out of scope

- Cross-replica forwarding of app-session envelopes (a machine on a sibling replica stays
  skipped, as today).
- Person-tunable routing rules in the OS (a later epic; `modelPreference` is the one field
  this epic adds).
- Local transcription or speech on a fleet machine.
- Counting a subscription's remaining quota; billing stays `unknown` when the app is silent.

## 9. Facts to re-verify before starting

Codex app-server's method names and event shapes; Claude Code's stream-json event types
and `--json-schema` behaviour; Ollama `/api/show` detail fields; whether `codex
mcp-server`'s `codex-reply` accepts a thread id from a previous process; the MCP node's
current protocol revision. All were true on 2026-09-06.

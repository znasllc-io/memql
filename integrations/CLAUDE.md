# Integrations Directory

**Purpose:** External service integrations (Go code)
**Language:** Go
**Type:** Protocol adapters that bridge external services into the MemQL ecosystem

---

## Integration Contract

MemQL has **exactly three** extension words -- component / integration / pack;
intake "plugin" means pack. See
[Component vs integration vs pack](../docs/public/concepts/component-integration-pack.md).

Integrations are **protocol adapters**. They bridge external services (OpenAI voice, LiveKit, avatar vendors, etc.) into the MemQL ecosystem using Go code for protocol-level concerns that cannot be expressed in the MemQL DSL.

### What Integrations DO (Go code)
- Handle external protocols (WebSocket, gRPC, HTTP webhooks, binary audio)
- Emit events into the system when external things happen
- Expose **capabilities** -- typed functions callable from the MemQL DSL
- Manage real-time state (streaming, presence, caching)

### What Integrations DO NOT DO (belongs in MemQL DSL)
- Query orchestration (fetching data, assembling context)
- AI invocation (calling prompts, tool-calling loops)
- Business logic (deciding who responds, building conversation history)
- Data mutations (inserting records, updating nodes)

### A capability that reads rows DIRECTLY must gate them itself

A hand-rolled `bun.NewSelect()` against `"MemoryNodes"` passes through neither
the parser nor the filter path, so **nothing the engine injects reaches it**.
Two `PluginContext` callbacks are the whole of the enforcement on that path, and
a factory that needs either must **refuse to construct** when it is nil rather
than default:

| Callback | Question | Default that must never be taken |
|---|---|---|
| `ConceptDataIsStaged` | Is this concept's data visible to anyone yet? (epic memql#3974) | "nothing is staged" |
| `AdmitSourceRow` | May **this caller** see **this row**? (memql#4029) | "yes" |

**Apply `AdmitSourceRow` to the rows AS FETCHED — before folding, summarizing
or repacking them.** Both of the engine's row-authz mechanisms resolve the tier
from a *concept*: filter injection from `plan.BoundConcept`, the row gate from
the row's own concept. So a capability that reads real rows and returns a
**synthetic** node stamped with its own concept defeats both by construction —
the gate is asked about the made-up concept, finds no tier declared, and admits.
It fails silently and in the admitting direction. `integration.chat.recentChat`
did exactly this with `v1:cognition:utterance` text.

Repairing it after the repack is not possible: the summary carries no top-level
owner field for an `owned` tier to read, no per-row identity for `granted`, a
synthesized id, and often rows from more than one concept. The fetched rows are
the last point on the path where the question is answerable.

**If your reader dedups versions per id, gate INSIDE the fold, after the id is
marked seen** — gating the slice first lets a denied *latest* version fall
through to an admitted *older* one, which hands the caller a stale row instead
of no row. See `foldActiveParticipants` in `integrations/chat/recent_chat.go`.

Note this makes a read *correct*, not *scoped*: the gate filters after the
fetch and adds no caller predicate to the SQL. Routing the read through the
authorized path is the wider inventory (memql#3984).

### IntegrationProvider Interface

All integrations that embed the base `Integration` struct should implement `IntegrationProvider`:

```go
type IntegrationProvider interface {
    IntegrationName() string
    Capabilities() []IntegrationCapability
}
```

Capabilities are registered with the engine -- either through the plug-in path (`memql.RegisterPlugin` at `init()`; preferred and used by core integrations like database, auth, identity, email, embedding, files, azureblob) or through explicit `app/` wiring for complex node-type-scoped ones (cognition, agent, stt). Either way they become callable as builtin functions from the DSL. Return `nil` from `Capabilities()` for integrations with no DSL-callable functions.

### Channel-Based Dispatch

When the engine's bus wiring is configured, integration capability calls are dispatched
via the `IntegrationRequests` channel using `IntegrationDispatchRequest` protobuf messages.
The engine's integration dispatcher goroutine reads from the channel, looks up the handler
by FQN, executes it, and sends the response via `ReplyTo`. The `IntegrationProvider`
interface itself does not change -- only the dispatch mechanism.

### IntegrationEngineAccess

Integrations should receive `IntegrationEngineAccess` (narrow interface) instead of the full `MemQLEngine`. This interface provides:
- `RegisterIntegration()` -- capability registration
- Streaming provider access (for protocol-level streaming)
- Tool definitions and execution (for tool-calling streams)

It explicitly does NOT provide `Execute()`, `InvokeAI()`, or `RenderPrompt()` -- those belong in MemQL automations and functions.

### Pattern: Events Inward, Capabilities Outward

```
External Service --> Integration (Go) --> [emits event] --> Automation (MemQL)
                                                                |
                                                    [calls integration capability]
                                                                |
                     Integration (Go) <-- [capability handler] <--
                         |
External Service <-- [protocol response]
```

---

## Directory Structure

```
integrations/
├── CLAUDE.md          # This file
├── arch.md            # Architecture documentation
├── integration.go     # Base Integration struct + the IntegrationProvider contract
├── integrations.json  # Integration inventory metadata
├── actionsearch/      # searchActions -- pgvector cosine search over the action library's intent embeddings
├── agent/             # AI tool-loop + chat replier + AiSuggest dispatcher (agent build only)
├── agentdef/          # One modality-independent projection of an agent's generation definition (text + voice read the same fields)
├── agents/            # Specialist dispatch -- askSpecialist, invoke, produceArtifact, requestUserFeedback
├── auth/              # resolveUser, checkPermission
├── avatardirect/      # Direct/Guide avatar -- startSession, stopSession, engageVendor
├── avatarvendor/      # CGO-free Anam/Simli avatar-vendor REST + dispatch core (shared by voice-agent + direct/Guide avatar)
├── azureblob/         # storage.upload (Azure Blob) -- registers as plug-in "storage"
├── chat/              # recentChat -- the row-gated recent-utterance repack
├── cognition/         # Routing+conductor, Polyphon scoring, client-tool relay -- scoreUtterance, trackPresence
├── dailyspace/        # Per-user daily-space lifecycle -- ensureForCaller, ensureForUser, rolloverAllUsers
├── database/          # healthCheck, stats
├── deployversion/     # suggestNextVersion -- read-only version arithmetic the DSL grammar cannot express
├── email/             # Microsoft Graph / SMTP / Log senders -- sendEmail, status
├── embedding/         # Vendor-neutral text embedding -- embed, store, findSimilar (pgvector)
├── fileprocessor/     # extractText (PDF / DOCX / images via VisionAIProvider)
├── harnessrecall/     # recall -- the harness memory-recall operator
├── harnesstrace/      # trace -- harness trace writes
├── identity/          # Delegation lifecycle -- createDelegation, resolveDelegation, revokeDelegation, validateScope
├── knowledge/         # Corpus seed + augment-domain pipeline -- ingest, seedStandardDomains, augmentDomainAnalyze/Generate, ensureKnowledgeBridge
├── library/           # Library document version history: the user / assistant / restore appends (memql#1228-1231)
├── liveknowledge/     # query -- Live Knowledge dispatch to a registered connector
├── openai/            # Polyphon ASR/TTS via OpenAI (Realtime transcription + /v1/audio/speech) -- synthesize
├── openairealtime/    # Mints OpenAI Realtime ephemeral client secrets for the direct browser<->OpenAI WebRTC path
├── planner/           # Planner Agent loop, task fan-out, refresh cron, action substitution (planner build)
├── rbac/              # Relational governance rank arithmetic -- canCreatePrincipal, governPrincipal
├── router/            # BYOK credential + budget admin -- setApiKey, listModels, listPolicies
├── shopify/           # THE CONNECTOR: a complete generated mirror of a store,
│                   # the push channel, and the compliance jobs. The reference
│                   # implementation of the data-origins contract
├── similarity/        # pgvector similarTo() builtin
├── stt/               # Speech-to-text -- transcribe (batch capability + streaming session)
├── telephony/         # PSTN edge -- number provisioning, call control, E911, consent, DTMF
├── timeutil/          # Stateless time helpers; always-on core plug-in, every node type
├── voice/             # Canonical voice catalog builtins -- pickForGender, resolve
└── workbench/         # Per-Plan sandboxed workspace -- dispatchHost, teardownDirectory

# The coding agent is a SEAM, not an integration here: component/planner's
# RegisterContainerExecutor takes a backend (reserved name "nemoclaw") and
# nothing in this repo registers one -- see the root CLAUDE.md (memql#4120).
#
# training/ (the per-agent "Train" pipeline) is a PACK: it lives in the
# product repo as a thin Go module self-registering via
# memql.RegisterPlugin("training"). It is the transitional product-Go
# exception under memql#2472, not yet absorbed into the engine.
```

---

## Key Integrations

### cognition/ -- routing + conductor + relay

Owns the cognition pipeline on the cognition node. The registered capability
surface is just two: `cognitionScore` and `cognitionTrackPresence` (the DSL
builtin names in `dsl/common/builtins.memql`, backed by
`integration.cognition.scoreUtterance` / `.trackPresence` in
`capabilities.go`). The cross-node `ClientToolCall` round trip rides the graph
event bus, not a capability.

Routing behaviour -- the single-LLM-brain text path, capability-aware routing,
conversational continuity, greet-on-join pacing and its cross-replica advisory-
lock gate -- is described in the root CLAUDE.md's Cognition section. Two things
that live only here:

- **Affirmation guard exempts corrections.** The
  "affirmation/follow-up/farewell -- skip dispatch" guard exempts
  correction-shaped utterances ("sorry I meant X", "actually no", "wait that's
  wrong") so a user correcting the agent's previous action lands as actionable
  rather than conversational.
- **Older docs describing a two-brain (router + conductor) text path are wrong
  about the current code.** The standalone router LLM call in `ai_router.go`
  fires for voice utterances only.

Key files: `cognition_handler.go` (event handler + dispatch flow),
`conductor_consult.go` (conductor LLM call + lastResponder + plan->outcome
adapter), `greet_on_join.go`, `ai_router.go`, `client_tool_relay.go`,
`agent_forward.go`, `space_context_engine.go`, `prompt_context_cache.go`,
`participant_presence.go`.

### `core/audio/` -- audio processing (NOT under `integrations/`)

Not a DSL-callable integration -- a shared Go utility package, so it lives
under `core/audio/`. PCM16 resampling (16kHz <-> 24kHz for Polyphon <-> OpenAI)
with a persistent-filter streaming resampler (`resample.go`), WAV header
generation + PCM chunking (`wav.go`), MP3 helpers (`mp3.go`).

### openai/ -- OpenAI ASR for streaming transcription

Streaming ASR via the Realtime API in transcription-only mode (WebSocket), with
automatic 16kHz <-> 24kHz resampling. Implements `audio.ASRProvider`; the only
constructor is `app/integrations_stt.go`. Files: `openai.go` (config),
`asr.go`. Env: `MEMQL_OPENAI_REALTIME_MODEL`, falling back to
`MEMQL_POLYPHON_OPENAI_ASR_MODEL` and then to `whisper-1`; plus
`MEMQL_POLYPHON_OPENAI_VAD_SILENCE_MS` (600) for the server-VAD
end-of-utterance window.

The package also carried a TTS client against `/v1/audio/speech`, for the
Polyphon voice transport. Epic memql#4988 retired that transport with the voice
and cognition node types and nothing constructed the client, so it and its
`..._TTS_MODEL` / `..._TTS_VOICE` knobs are gone. **The engine's own
text-to-speech is unaffected**: `AiSpeechMsg` is served by the AI provider
registry in `component/memql/ai_providers.go`, which has always called OpenAI
directly and never went through this package.

### stt/ -- speech-to-text

Real-time streaming transcription over `MemqlService.Stream`
(`AiTranscribeStreamStart` / `Chunk` / `End` -> `Delta` / `Complete`); the
**agent** node owns the provider session and the BFF proxies through
`AiForwardRouter.ForwardContinuation`. Single-shot batch via `AiTranscribeMsg`
is still supported, plus the DSL capability `integration.stt.transcribe`.

Providers: **OpenAI Realtime** (default, true streaming WebSocket) or **OpenAI
Whisper** (batch only; `whisper-1` transcribes verbatim and must be enabled for
your OpenAI project).

Env:
- `MEMQL_STT_PROVIDER` -- `openai-realtime` (default) or `openai-whisper`
- `MEMQL_STT_LANGUAGE` -- hard-pinned streaming language (default `en`).
  Overrides the client `language_hint`; prevents wrong/mixed-language drift and
  short-word hallucination on noisy audio.
- `MEMQL_STT_MIN_CONFIDENCE` -- low-confidence FINAL cutoff (default `0.6`),
  also gating a no-speech silence-hallucination denylist; `0` disables both.
  Filtering runs at the provider-agnostic `pumpDeltas` chokepoint in
  `component/grpc/ai_transcribe_stream.go`.
- `MEMQL_WHISPER_MODEL` -- defaults to `whisper-1`

### The rest, in one line each

| Package | DSL capabilities |
|---|---|
| `auth/` | `resolveUser`, `checkPermission`. JWT verification itself stays in `component/identity/verifier/` + `component/auth/` |
| `database/` | `healthCheck`, `stats`. The connection is a core component; this exposes management ops |
| `fileprocessor/` | `files.extractText` -- PDF / DOCX / images (via VisionAIProvider) / text |
| `azureblob/` | `storage.upload` -- returns the blob URL (registers under the name `storage`) |
| `shopify/` | **The connector** (memql#4389) -- see the section below. `ensureSubscriptions`, `runComplianceJobs`, `fetchProduct`, `shopifyql`, `storeHealth`, and the four `commerce*` analytics tools; the five contract verbs over 65 generated concepts. The plug-in always registers so the capabilities exist; each no-ops with no store row rather than failing |

---

---

## Agent integration (`agent/`)

Runs on the agent build only. Holds the streaming AI tool-loop
(`streaming.go`), the chat replier (`replier.go`), the
respondToUser-envelope schema + parser (`envelope.go`), and the prompt
context builder (`prompt_data.go`). (AiSuggest is no longer handled here --
it dispatches via the suggest-domain registry; see below.)

The unified `MEMQL_TOOL_LOOP_MAX_ITERATIONS` cap (default 120, clamped
to a max of 200) gates both streaming and non-streaming tool loops.

**Reply envelope.** The chat path delivers every user-facing reply
via a sentinel `respondToUser` tool call whose args are
`{response, citations[]}`. The streaming loop intercepts the call by
name (no engine executor exists), parses the args as
`agent.Envelope`, and uses that as the turn's final text + citations
list. See `envelope.go` -- the schema, tool definition, and parser
all live there.

**AiSuggest domains (memql#1959).** `AiSuggestMsg` dispatch is generic in
core (`component/grpc/ai_handlers.go` + the `memql.RegisterSuggestDomain`
registry in `component/memql/suggest_registry.go`). `knowledge` registers
from core; the product domains (spaces / spaceTitle / agents /
groups / groupDescription / agentCardSummary / spaceCardSummary /
groupCardSummary) register from the product's own suggest pack (a thin Go
module in the product repo). Under the consolidated platform (memql#2472) the
engine ships product-agnostic and product DSL rides in at runtime via
`MEMQL_DSL_PATH`; these Go-backed suggest handlers are a transitional
product-repo pack, pending engine-generic absorption or bundle delivery.
`spaceTitle` / `groupDescription` are lightweight single-field generations
(create-modal blur handlers); the richer `spaces` / `agents` / `groups`
return full payloads; the `*CardSummary` domains generate canvas-card bodies;
`knowledge` powers the KnowledgeModal domain picker.

---


## shopify/ -- the connector, filled

`integrations/shopify` is the J1 fill (memql#4389) of the surface J0 left
declared. Read it before writing a second connector; most of what follows
generalises.

**The model is generated, not written.** `cmd/shopifyschema` reads Shopify's
Admin GraphQL schema at a pinned version and emits 65 concepts, their default
projections, their two reads, the fetch and bulk documents, and the Go routing
table (`integrations/shopify/generated/`). Nothing under `generated/` is
edited by hand -- a drift gate fails the build, and a hand edit would be lost
at the next regeneration anyway. The connector holds NO per-type code: it
reads a table that says which topic routes to which concept and which document
fetches it.

**No mirror WRITE is generated**, and that is the contract rather than an
omission: a connector returns MirrorWrites and the runtime performs the write.
Emitting a mutation per concept would put a second write path beside it -- 65
of them -- and the two would have to agree about stamping, about what a
cleared field means and about which fields a retirement sets.

**Six decisions worth copying into the next connector:**

1. **A webhook is a trigger, not a payload.** Nothing in `apply.go` reads a
   business field out of a delivery. Payloads lose fields, truncate and arrive
   out of order; the API does not.
2. **Reconciliation is a requirement.** Shopify does not guarantee delivery,
   so every domain is re-checked on a cadence and what a pass heals is counted
   as DRIFT -- the only evidence anyone gets that live delivery is working.
3. **Credentials are references.** The store row carries the NAME of a
   `globalSecret`, never a token, because the row is read by the portal.
4. **A vendor validation error is `sync.Permanent`.** It arrives inside a 200
   and will fail identically forever; the drain dead-letters it immediately
   rather than spending its attempt budget.
5. **Every write is idempotent on a derived row id.** `MirrorRowID(store, gid)`
   is a digest, so two tenants sharing an origin id stay separate rows.
6. **A multi-tenant connector owns its inbound sources.** `InboundSource` is an
   OPTIONAL interface (`sync.InboundSourceProvider`) rather than a contract
   verb: only a connector with tenants added at runtime needs one.

Runbook: [docs/public/operate/shopify-connector.md](../docs/public/operate/shopify-connector.md).

## Adding New Integrations

MemQL's integration system has two registration paths:

1. **Self-registration** (`memql.RegisterPlugin`; preferred) -- the
   integration registers itself at `init()` with a narrow
   `PluginContext`. Good for integrations whose dependencies fit
   the common surface (Logger, Engine, BunDB, VisionProvider,
   EmbeddingProviderByName, partition/variable resolvers). Use this
   for any product-specific integration (registered from its pack repo)
   that doesn't need deps outside `PluginContext`.
2. **Explicit `app/` wiring** -- reserved for first-party integrations
   that need deps outside `PluginContext` (cognition, agent, stt).
   Stays in `app/integrations_*.go` with build-tag gating.

### Self-registration path (most cases)

**1. Create the package**
```bash
mkdir -p integrations/<name>
```

**2. Capability / provider** -- implement `memql.IntegrationProvider`
```go
// integrations/<name>/capabilities.go
package <name>

import (
    "context"
    "encoding/json"
    memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
    "github.com/znasllc-io/memql/component/memql"
)

type Integration struct{ /* ...deps... */ }

func New(/* deps */) *Integration { return &Integration{} }

func (i *Integration) IntegrationName() string { return "<name>" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
    return []memql.IntegrationCapability{
        {
            Name:        "doThing",
            Description: "...",
            Handler:     i.handleDoThing,
        },
    }
}

func (i *Integration) handleDoThing(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
    // ...
}
```

**3. Self-registration** (`memql.RegisterPlugin`) -- register at `init()`
```go
// integrations/<name>/plugin.go
package <name>

import "github.com/znasllc-io/memql/component/memql"

func init() {
    memql.RegisterPlugin("<name>", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
        return New(/* pluck deps from pctx */), nil
    })
}
```

Return `(nil, nil)` from the factory to opt out at runtime (e.g., a
feature that needs configuration the environment didn't supply).

**4. Anchor the package** so its `init()` runs. Add a blank import
to `app/plugins_core.go` for core integrations; a product-specific
integration anchors from its pack repo instead.
Build-tag-gate the anchor file if the integration should only run on
certain node types.

**5. Expose it from the DSL via a builtin**
```memql
// in dsl/common/builtins.memql
@enabled
@description("What the capability does, in one line.")
@executor("integration.<name>.doThing")
builtin <name>DoThing {
  someId  string  @required
  limit   integer
}
```
The body IS the builtin's input schema — one field per line, taking the
same annotations a concept property does (`@required`, `@enum`,
`@description`). The `func (Builtin) …` receiver form this step used to
show is retired and rejected at parse time (memql#303).

### Explicit `app/` path (complex first-party only)

When a PluginContext isn't enough, add wiring under
`app/integrations_<nodeType>.go` with the matching build tag and
call `a.engine.RegisterIntegration(yourInstance)` directly.

### Test It
```bash
make dev NODE=bff
kubectl logs -n memql deploy/bff -f | grep -iE "plug-in|<name>"
```

---

## Connectors -- the recipe (epic memql#4378)

A **connector** is an integration that sits on the other side of a data
boundary: it fills a MIRROR from the system that owns it, or pushes a
MemQL-**origin** concept's changes out to a system that mirrors it. It is
not a fourth extension word -- a connector is an integration, and what
makes it one is that a concept names it in `@origin` or `@mirroredTo` and
the connector registry can find it under that name.

Read [data origins](../docs/public/concepts/data-origins.md) first; the
three states are the vocabulary everything below assumes.

**1. Declare the name from an `init()`.**

```go
const ConnectorName = "shopify"

func init() { memqlsync.Declare(ConnectorName) }
```

Declaration is separate from binding, and not by preference: the engine's
boot check resolves `@origin("shopify")` inside `MemQLEngine.Init`, which
runs BEFORE integrations are wired (`app/build_*.go`: config → database →
engine → integrations). A registry holding only live instances is empty
exactly when the check reads it. `Declare` in an `init()` is available
before all of that.

**2. Implement the contract** (`component/memql/sync`): `Name`,
`Domains`, `Apply`, `Backfill`, `Reconcile`, `Propagate`,
`EnsureSubscriptions`. Return `memqlsync.NotImplemented(name, what)` for
anything you do not serve yet -- the runtime distinguishes that from a
delivery failure, so an unserved capability is reported rather than
retried and dead-lettered.

**3. Bind the implementation** from your `memql.RegisterPlugin` factory,
once configuration is in hand: `memqlsync.Bind(i.Connector())`. `Bind`
refuses a name nothing declared, which is what keeps the two halves from
naming different sets.

**4. Declare the domains in DSL.** `@origin("<name>")` on each concept
you mirror; `@mirroredTo("<name>")` on each MemQL-origin concept you
drain. A name nothing declared **refuses boot**.

**5. Stamp the actor on every write.** The mirror guard admits exactly
one identity, and it is not "the engine":

```go
ctx = auth.ContextWithConnectorActor(ctx, ConnectorName)
ctx = auth.ContextWithInternalOrigin(ctx)   // only if your mutations are @serverOnly
```

### What the runtime does for you

Version-guarded apply (a late webhook cannot regress a mirror), the
inbound dispatcher, the backfill and reconciliation runners, the durable
outbox and its per-connector drain worker with idempotency, backoff,
dead-lettering and audit, and the health row the Data origins page reads.
A connector delivers once and reports what happened; it does not own
ordering, retry or scheduling.

### What the actor may touch

Row admission admits `ConnectorActor(name)` to the concepts whose
`@origin` or `@mirroredTo` names it, **regardless of tier, and to nothing
else** -- including concepts that declare no tier at all, which admit
every other caller. Your connector cannot read a campaign, an identity,
or another origin's mirror. That is deliberate: the concept's declaration
is what grants reach, so a connector gains it only where an author wrote
its name.

Worked example: `integrations/shopify/connector.go`.

---

## Debugging Integrations

```bash
kubectl logs -n memql deploy/bff | grep "integration.*started"
kubectl logs -n memql deploy/cognition -f | grep -E "cognition|turn state|ai.*response"
```

Common causes, in order: dependencies not up (engine, event bus); event
subscription missing; API keys absent; cache hit rate (visible in the logs).

## Environment Variables

### AI Providers (Cognition Integration)
```bash
MEMQL_AI_OPENAI_API_KEY=sk-...         # OpenAI provider
MEMQL_AI_ANTHROPIC_API_KEY=sk-ant-...  # Anthropic provider
MEMQL_SI_CACHE_DEFAULT_ENABLED=true
MEMQL_SI_CACHE_MAX_SECONDS=120
```

`MEMQL_SI_OPENAI_API_KEY` / `MEMQL_SI_ANTHROPIC_API_KEY` are legacy aliases
only (`component/envregistry/legacyalias.go`, entries at :88-89, the map is
NEW -> LEGACY); use the `MEMQL_AI_*` forms above.

### Feature Flags

None. The `<COMPONENT>_CAPABILITIES_LOGGING_LOG_LEVEL` spelling belongs to
`core/component`'s logger convention (see `core/component/component.go`),
and an integration is an `IntegrationProvider`, not a
`core/component.Component` -- so no integration reads one. An integration
logs through the logger its plug-in factory is handed.

---

## See Also

- [Architecture](arch.md) - integration system architecture
- [Audio Streaming](../docs/public/build/audio-streaming.md) - the streaming-transcription gRPC path
- [Authoring rules](../docs/public/language/authoring-rules.md) - MemQL gotchas (read before extending the DSL surface)

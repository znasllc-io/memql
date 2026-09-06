# Removing cognition, spaces and voice -- Design

- **Date:** 2026-09-06
- **Status:** approved. Epic memql#4988 states the goal ("cognition and spaces
  are the conversational substrate of the previous product... the concepts,
  constructs, code and documentation that describe them are to be removed, not
  adapted") and leaves five things open. Each is recorded below as a decision
  with the evidence that settled it and the alternative it rejected.
- **Program:** sub-project F of the program in memql#4961, after B. It is the
  last of the removals: A replaced the planner spine with the work spine, B
  rebuilt Nexus on MemQL OS, E removed the portal.
- **Scope:** `dsl/cognition/`, `dsl/telephony/`, `integrations/{cognition,
  voice,dailyspace,chat,avatardirect,avatarvendor,openairealtime,telephony}/`,
  `component/polyphon/`, `component/server/{polyphonws,audiows}/`, the
  cognition and voice node types, the guest-invite flow, the space attachment
  route, the client-tool relay, the agent concept's conversational half, and
  the documentation for all of it.

---

## 0. What this removes, in one paragraph

MemQL's previous product was a conversational one: people and AI agents in a
shared **space**, taking turns under a **conductor**, speaking over **LiveKit**
with **Polyphon** deciding who talks next and an avatar lip-syncing the reply.
Everything in that sentence is gone. What is left is the platform the product
was built on -- the graph, the DSL, the work spine, the Library, the fleet, the
workbench, campaigns, and MemQL OS -- plus one piece of the voice stack that
was never conversational and is load-bearing today.

**307 files and roughly 74,000 lines are deleted.** The engine's node types go
from nine to seven.

---

## 1. The decisions

### D1 -- Streaming transcription STAYS, re-homed from the voice node to the agent node

The epic's "Voice" bullet reads as though the whole speech stack goes. It does
not, and this is the decision most likely to have been made wrong by a careful
reading of the issue alone.

**MemQL OS's Ask surface has hold-to-talk dictation** -- `clients/os/src/ask/`,
shipped 2026-09-01 as epic memql#4747, five days before this work. It is not
the conversational product: there is no room, no participant and no
conversation, just a microphone and a transcript that lands in the goal field.
It rides `AiTranscribeStreamStart / Chunk / End -> Delta / Complete`, which the
BFF proxied to **the voice node**.

So the family stays and `nodeTargetForTranscribe()` now returns
`NodeTypeAgent`. That is a re-point rather than new plumbing: the agent node
already called `SetSTTProvider` (`app/transport_agent.go`), so it has been
capable of serving this the whole time. The `init()` guard that pinned the
target to the voice node is kept, pointed at the agent node, because the
failure it prevents is silent -- the browser holds the mic open and no
transcript ever arrives.

**What went with the voice node instead:** the one-shot `AiSpeechMsg` (TTS) and
`AiTranscribeMsg` (batch), which had no consumer but the SDK and the portal.

The ASR contract (`ASRProvider` / `ASRStream` / `ASRConfig` / `ASRResult`)
moved from `component/polyphon` to `core/audio`. It had to move somewhere:
`integrations/stt` imports `integrations/openai`, so the shared types cannot
live in either without making the edge circular, and `core` is the leaf both
already depend on. Nothing about those types was ever specific to Polyphon.

### D2 -- Telephony goes with voice

Telephony is not named in the epic. It goes anyway, and the evidence is its
own:

- `docs/public/operate/telephony.md` is titled "Telephony -- PSTN calling for
  MemQL **voice agents**".
- `integrations/telephony/room.go` says it in code: *"The voice-agent
  dispatcher serves these the same way it serves product rooms, so the existing
  realtime agent answers phone calls."*

Without the voice agent an inbound call rings into an empty LiveKit room.
Keeping it would mean keeping the entire LiveKit deployment -- server, SIP
bridge, external secret and the `LIVEKIT_*` quartet -- to serve a surface with
no answerer, which is the standing thing this epic exists to stop.

**Rejected:** keeping telephony as a stranded integration and flagging it. That
reading is defensible from the epic's literal word list, and it is the one this
work started with; the two independent pieces of evidence above are what
changed it.

### D3 -- The agent concept is REDUCED, not deleted

The epic says the agent concept's "fate is decided here". It survives.

The reason is that six surviving namespaces name `v1:agents:agent`
structurally -- `worker` (`workerCall.agentId`), `identity`
(`agentDelegation.agentId`), `platform` (`capabilityGap.requestedByAgentId`),
`library` (`producedByAgentId`), `planner` (`plan.ownerAgentId`) and `router`
(a budget scope) -- and `v1:agents:agentAuthorization` has four live readers,
of which three survive: the worker dispatch gate
(`integrations/agent/worker/store.go`), the authoring capability store, and the
planner's mint gate. Deleting the concept is a second epic, not this one.

**Worth recording, because the epic asserts otherwise:** nothing in `dsl/work`,
`component/work` or `integrations/work` reads `agentAuthorization` today. The
spine's design record states the intent prospectively; the code has not caught
up. The row survives because the WORKER and AUTHORING gates read it, not
because the spine does.

What was cut from the concept is its conversational half: the `avatar`,
`triggerBehavior` and `providerConfig.{voice,avatar}` blocks, the
`capabilities.{avatar,lipSync,vision,voiceToVoice,claw,clawWorkspace}` flags,
`gender`, `audioControl`, `videoControl`, `avatarPersonaId`, `avatarVendor`,
the whole `avatarPersona` concept and its Simli seed, and the `askSpecialist`
tool and builtin (the assistant-to-specialist bridge).

### D4 -- The guest half of the invitation goes; the invitation stays

`v1:identity:invitation` is a general identity primitive and MemQL OS's Users
app issues user invitations through it. What went is the GUEST half, which was
space-scoped by construction: the five guest gRPC messages, the
`Authorization: Guest <token>` interceptor and its `identity.guest` claim, the
`?guest_token=` WebSocket parameter, the four `@serverOnly` guest mutations,
`createGuestParticipant`, and `invitationByTokenHash` /
`invitationByPreviousTokenHash` (both filtered `kind=="guest"` and had no
surviving caller).

`kind` keeps its `guest` enum value for rows already written. Nothing produces
one.

`userInvitationByTokenHash` was deliberately NOT widened to compensate. A
credential lookup that returns any kind makes every present and future caller
responsible for a privilege boundary the filter used to hold.

### D5 -- The space attachment route goes; the Library's byte routes were already the only path

`POST|GET /spaces/{id}/attachments` had no caller left in this repo. Training
moved to the Library when it was re-keyed in A3 -- `clients/os/src/apps/
training/concepts.ts` says so in capitals -- and the only caller-shaped code
was an SDK helper nothing imported. The handler, its store, the SDK helper, the
`Connection.uploadAttachment` method and the edge's `/spaces` bff-root prefix
all go; `POST /artifacts` and its chunked-session family are untouched.

`v1:common:attachment` SURVIVES (minus its required `partitionId`) because
`dsl/forge` and the reviews example pack reference it. `v1:common:media` --
"assets referenced by utterances", space-scoped and read by nothing -- does
not.

### D6 -- `@clientExecution` is REFUSED at parse, not ignored at runtime

A client-executed tool declared its body in the connected browser and reached
it over the client-tool relay (`integrations/cognition/client_tool_relay.go`),
which went with the rest of cognition. The flag itself had four homes that
outlived the relay: the parser, the AST, the runtime `Tool`, and
`ToolDefinition.client_execution` on the wire.

The first shape of this fix left the flag in place and refused it at CALL time
on both paths. That is the wrong seam. The load gate's whole purpose is that
"a tool must carry a handler" -- and `@clientExecution` was its ONE exemption,
so a tool declaring the flag and no handler still loaded, was still advertised
to the model, and failed only on the one call that reached it. The exemption
is now gone and the annotation is an unknown one, which the parser already
refuses by name. An author is told at boot instead of a user being told
mid-turn.

The wire field is `reserved 4; reserved "client_execution";` on
`ToolDefinition`, and both SDKs lose their inbound halves --
`registerClientToolHandler` in `sdk/ts` and the `ClientToolCall` dispatch
documented in `sdk/go/client/tools.go`. **This is a wire-contract change the
frontend can see**: `ToolDefinition.clientExecution` no longer appears in a
`ListTools` reply. Nothing in this repo read it except the SDK mirrors.

Two terminal markers in the agent tool loops went with it, in both the
streaming and non-streaming loops: `CLIENT_TOOL_UNREACHABLE`, whose only
emitter was the deleted `component/grpc/client_tool_failfast.go`, and
`WHEEL_CONTESTED`, which its own comment sourced to "the frontend's
ClientToolRelayBridge + requestControl primitive" for "a space that doesn't own
the Control Session" -- a bridge, a primitive and a concept that are all gone.
Both were substring matches against arbitrary tool output, so neither would
ever have failed a test; they would simply never have fired again.

### D7 -- The `guest` WebSocket credential scheme goes, because nothing can mint one

D4 removed the guest invitation. The TRANSPORT that carried a guest token
outlived it in five places: the `guest` subprotocol scheme and its negotiation
in `component/auth/ws_subprotocol.go`, the `?guest_token=` query parameter and
the subprotocol branch in `component/server/memqlws/handler.go`, and the
`auth.guestToken` dial option in `sdk/ts`.

It failed CLOSED -- no interceptor validated `Authorization: Guest`, so such a
stream reached the identity verifier and was rejected -- which is precisely why
it survived a green build. What it cost was honesty: an SDK dial option and a
negotiated subprotocol advertise a credential the cluster stopped accepting,
and a browser offering an unnegotiated subprotocol aborts the handshake with no
usable error.

`v1:identity:invitation.kind` also moved its `@default` from `guest` to `user`.
The enum keeps both values so rows already written read back, but `guest` is
now a value nothing writes and no redeem path finds, and a default is a
statement about what the next row should be.

### D8 -- One DSL-visible `config.` key goes, and it is the only contract here a product bundle could hold

`config.voiceProvider` was in `PolicyExposableConfig`, so any `.memql` body
could read it. Every other removal in this epic is either engine-internal or a
wire message a client would have to have been sent; this one is a name a
product DSL bundle mounted at `MEMQL_DSL_PATH` could reference without this
repository ever seeing it, and the failure would land at that bundle's strict
boot rather than here.

It goes anyway. It answered "which voice provider serves `/memql/audio`", a
route and a decision that no longer exist, so keeping the key would mean
keeping a value nothing computes -- and a config read that resolves to a stale
constant is worse than one that refuses. `MEMQL_POLYPHON_VOICE_PROVIDER`, the
env var behind it, and `ConfigSnapshot.polyphon_voice_provider`, the bus field,
go with it (the field number and name `reserved`, following
`polyphon_bridge_agent_url` in the same message).

No other `config.` key is removed, and no reader exists anywhere in this repo,
`examples/` included.

---

## 2. What was deliberately left alone

- **The retired `partitionId` dimension.** Twelve concepts still carry an
  optional `partitionId`. Their `@description` strings named
  `v1:cognition:space` and are rewritten, but the FIELDS stay: the partition
  sweep is issue #56 phase 8, which is open, and conflating the two would make
  both harder to review. MemQL OS reads none of them.
- **`v1:planner:plan` / `task` / `taskState` and `component/planner`.** Retired
  by the work spine's section F, gated on A3 re-keying Training. That is issue
  #5000, still open.
- **`deploy/fleet`'s voice pricing.** ~~Left alone pending an owner ruling.~~
  **RULED AND DONE** (2026-09-06, memql#5031): remove it. `voiceMinutes`,
  `overageVoiceUsdPerMinute` and `voiceAddOn` are gone from the tierSpec and
  subscription concepts, from all five tier seeds, from three shapes and from
  the two mutations that wrote them; the tier table in
  `docs/public/operate/memql-cloud.md` loses its Voice minutes column and the
  overage sentence its voice clause.

  Two things were deliberately KEPT. `usageMeter.metric` keeps its
  `voice_minutes` enum value so periods already metered and invoiced still read
  back -- billing history is the one thing that may not become unreadable --
  while `openUsageMeter`'s own arg enum narrows to `message_credits`, so
  nothing can open a new one. Same shape as D4's `invitation.kind`: close the
  writer, keep the stored value legible. And `setSubscriptionAddOns` keeps its
  plural name; HA is the only add-on now, but the name is a category rather
  than a count, and this bundle is not embedded so a rename is a wire change
  against callers outside this repository.
- **`policy localConductor`.** A provider-selection policy in the local-first
  set from epic memql#4676. "Conductor" there names a workload shape rather
  than the cognition conductor, and renaming it would touch an unrelated
  epic's design record and test for no functional gain.

---

## 3. Two things that changed shape under the removal

### 3.1 The release images become distroless, and the drain hooks had to change

The Dockerfile's `voice-runtime` stage (debian-slim + libopus) was the LAST
stage, so `docker build` with no `--target` produced it. Every RELEASED image
was therefore debian-slim while every locally built one was distroless -- which
the Dockerfile's own comment says is the intent ("the default for every node
type except the workbench"). Deleting the voice stage makes the release match
the intent, and the release workflow now names `target: runtime` explicitly
rather than relying on stage order.

The consequence had to be handled rather than shipped: seven Deployments
carried `preStop: exec: ["/bin/sh","-c","sleep 5"]`, and distroless has no
shell, so every drain would have failed silently (a failed preStop is an event,
and the pod terminates anyway). They now use Kubernetes' native shell-free
`preStop.sleep` action, which is GA in 1.32 -- the version the local cluster
pins.

### 3.2 The coordination Redis had no consumer left

`deploy/k8s/base/redis.yaml` (`livekit-redis`) existed for LiveKit and its SIP
bridge. Both are gone, so it is deleted rather than left running a pointless
replica in both cloud overlays.

---

## 4. The documentation disposition

23 pages deleted, 74 rewritten, plus `GLOSSARY.md` and root `CLAUDE.md`.

**Deleted outright:** the eight `voice-4xx` design records, the extension-point
audit (whose subject was the cognition/voice/planner audit itself), the
telephony epic and its two runbooks, the LiveKit provisioning runbook, the
three voice runbooks, the voice-agent JWT page, the dedupe-peruser-seeds
migration, and -- under the standing cleanup the epic also carries -- four
orphans with no inbound link that described retired work, plus the spent A2
plan.

**`docs/public/build/audio-streaming.md` is a REWRITE, not a delete.** Its
three documented paths reduce to one, and that one -- gRPC streaming
transcription -- is the surviving path D1 keeps. Deleting the page would have
removed the only documentation of a live feature.

Two pages were deliberately not rewritten. `timescaledb-license-compliance.md`
quotes correspondence actually sent to Timescale saying "agents, automations,
and voice"; that is a historical quote and got a dated note rather than a
silent edit. `conn-exhaustion-53300-spike.md` is an incident record whose node
names are observed facts.

---

## 5. Where the gates moved

The removal broke twenty-odd repo gates, every one because a fixture named
something that no longer exists. They were re-pointed rather than weakened, and
three are worth recording because the replacement changes what they assert:

- `TestStagedReadSiteGateCatchesAStrippedVerdict` drove
  `integrations/chat/recent_chat.go`, which had five all-GATE read holders. The
  widest surviving all-GATE file is `component/memql/executor_filter.go` with
  two, so the floor moved from five to two. What the floor buys is "more than
  one holder, so a half-working detector is loud", not the number.
- `TestNestedBlockWritesAreDeclared` lost `utterance.source` and gained
  `user.preferences`. It immediately earned its keep: it caught the two agent
  seeds still writing `capabilities.{avatar,lipSync,vision,voiceToVoice}` after
  the concept dropped them.
- `TestThePerConceptGuardsStillFire` goes from eight per-concept write guards
  to six. The two removed are the cognition utterance-authorization and
  AI-participant guards.

`TestEmbeddedFileCountsAreStable` moved 438 -> 413, measured from the gate's
own output rather than derived.

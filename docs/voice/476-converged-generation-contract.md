# Converged generation contract (one brain) for voice + text

Spike deliverable for issue #476, the **architectural keystone** of epic #475
("Realtime voice v2: model-owns-turn + converge to one generation brain").
Phase 0 design, no dependencies; blocks the 1-on-1 native-generation work
(#478) and the text-convergence work (#480).

Status: design + a clear VERDICT. This document changes no runtime behavior --
it is a contract + migration design grounded in the tree as it stands after
the Go voice-agent cutover (epic #449). Every load-bearing claim cites a real
`path:line`. The headline is in section 0.

> **Implementation status.**
>
> - **Stage 0 + Stage 1 data (#480).** The converged contract lives in the
>   shared `integrations/agentdef` package -- `AgentGenerationContract`,
>   `BuildGenerationContract`, and the shared `RenderIdentityBlock`, all pure
>   and golden-tested. The cognition text path
>   (`integrations/agent/prompt_data.go`) projects its `assistant` block through
>   `BuildGenerationContract`, byte-identical to the prior inline mapping
>   (pinned by `prompt_data_test.go`).
> - **Stage 2 renderer convergence (#478).** The voice persona renderer
>   (`integrations/voice/agent/instructions.go` `BuildPersonaInstructions`) now
>   embeds the SAME `agentdef.RenderIdentityBlock` the text path embeds, via a
>   `personaContract(Persona)` adapter -- so both modalities describe the agent
>   through one renderer (cross-modality test in `instructions_test.go`).
> - **Remaining (#478).** Stage 2 persona *population* still needs the
>   `VoiceAgentSessionAck` proto extension (section 6.3) -- deferred because
>   regenerating the proto here downgrades the committed protoc version header
>   (local v3.21.12 vs committed v5.29.3), which the `sdk-gen-check` lane would
>   flag; it needs the pinned protoc toolchain. Stage 3 (native authorship:
>   `semantic_vad`, conductor off the 1-on-1 critical path, delete the `runTurn`
>   round-trip + `RealtimeInstructionsForReply`) is the headline turn-detection
>   rework, tracked as the rest of #478.

> **Framing note.** Epic #475 states the problem as "two authors": voice
> replies come from gpt-realtime, text replies from cognition. The spike's
> first job was to verify that against the code. The finding (section 1) is
> sharper and changes the migration shape: **today cognition is the single
> author for BOTH modalities.** The realtime executor does not let the model
> author -- it sends the human turn to cognition, gets the authored reply
> back, and instructs gpt-realtime to *speak* it
> (`RealtimeInstructionsForReply`, `integrations/voice/agent/instructions.go:114`).
> The model is a prosody-aware TTS behind the same slow path the cascade uses.
> So "two authors" is the v1 *target* the realtime executor was built toward
> but never reached; "one starved persona vs one rich persona" is the drift
> that exists *right now*. The converged contract has to fix both: give the
> model authorship (so #475's latency win is real) AND make that authorship
> read the same definition the text path reads (so the two can't drift).

---

## 0. VERDICT

**Same-definition, two-runtimes -- GO. Same-model-both-modalities --
GO for voice-enabled spaces, DEFER as the universal text engine.**

Recommendation, in one line: **make the `v1:agents:agent` row the single
versioned source of truth for an agent's generation, project it into a shared
`AgentGenerationContract` value that BOTH a text endpoint (cognition's existing
agentReply loop) and a voice endpoint (gpt-realtime session config) instantiate
from the same fields, and let each endpoint's native model author its own turn.
Use the multimodal gpt-realtime model as the author on the voice path; do NOT
route all text through a standing realtime websocket.**

Why this split, precisely:

1. **The source of truth already exists and is already versioned.** The agent
   definition (persona + role + model policy + tools/skills + voice) lives on
   one concept, `v1:agents:agent`
   (`dsl/agents/concepts.memql:14`), materialized per-user by the
   SeedMaterializer (`dsl/agents/assistant.memql`). It carries `personality`,
   `systemPrompt`, `role`, `providerConfig.llm.{provider,model,policyName}`,
   `providerConfig.voice.voiceId`, and `capabilities.skillIds`
   (`dsl/agents/concepts.memql:17-80`). The text path reads ALL of it via
   `ActingAgentIdentity` (`component/grpc/memql.proto:1146`). The voice path
   reads almost NONE of it -- `VoiceAgentSessionAck`
   (`memql.proto:1653`) stamps only voice/avatar/audio-mode, so
   `Persona` (`integrations/voice/agent/persona.go:41`) degrades persona, role,
   description and style to neutral defaults. The convergence is therefore not
   "build a new source of truth" -- it is "stop the voice path from reading a
   starved projection of the one we already have." That is a proto-plus-mapping
   change, not a re-architecture. **[OK]**

2. **gpt-realtime accepts text in and text out in-session, so it can author
   typed turns** -- the protocol supports a `response.create` with text-only
   output. So same-model-both-modalities is *feasible*. But making a standing
   realtime websocket the engine for every typed turn in every space is a poor
   lifecycle/cost trade: the realtime session is a warm, stateful, per-space
   socket with a token meter and idle/duration guardrails
   (`integrations/voice/agent/realtime_budget.go:66`), whereas the text path is
   a stateless, fan-in, fallback-chained request the SI Router load-balances
   across providers (`integrations/agent/replier.go:270-292`). Text-only and
   voice-less spaces -- the common case -- would pay a standing-socket cost for
   a request/response workload. **Same *definition*, not same *runtime*, is the
   convergence guarantee; same *model* is a per-modality implementation choice,
   correct for voice, wrong as a universal text engine.** **[OK] / [WARNING]**

3. **Cognition's role narrows but does not vanish.** It stops *authoring the
   words* on the voice path (the v1 realtime executor's `runTurn` round-trip,
   `realtime_executor.go:357`, is deleted under #478) and becomes
   policy + grounding + capture: WHEN (the conductor gate, #432/#477), WHAT
   CONTEXT (RAG retrieval + persona projection, `replier.go:361-471`), WHO MAY
   (privileged-tool authorization -- unchanged per #475 non-goals), and WHAT WAS
   SAID (the utterance capture path, `realtime_output.go`). On the *text* path,
   cognition keeps authoring -- but authoring against the converged contract,
   from the same fields, with the same gate, so it cannot drift from voice.
   **[OK]**

4. **The consistency guarantee is structural, not procedural.** Both endpoints
   build their instructions from ONE pure projector,
   `BuildGenerationContract(agent, directive, grounding)`, and a golden-file
   test asserts the voice session-instructions and the text system-prompt are
   character-identical in their shared region. Drift becomes a failing unit
   test in the default CI lane, not a production incident (section 7). **[OK]**

GO-WITH-CAVEATS would apply only if the agent definition were split across
multiple concepts (it is not -- it is one row) or if gpt-realtime could not
emit text-only responses (it can). NO-GO is not on the table. The one genuine
caveat is lifecycle (point 2): the recommendation explicitly does NOT converge
the *runtime*, only the *definition* -- and that is the correct reading of
#475's "one brain," which is about authorship drift, not about collapsing two
very differently-shaped runtimes into one socket.

Per-issue acceptance mapping is in section 8.

---

## 1. The problem, verified against the code

### 1.1 What epic #475 says

> "Today voice replies come from the realtime model while text-chat replies
> come from cognition -- two authors that can drift in tone/knowledge/policy."

### 1.2 What the tree actually does

The realtime executor was built behind the SAME gRPC seam as the cascade and
**never authors**. Trace it:

- A human final transcript drives `onCommittedTurn`
  (`realtime_executor.go:315`), which calls `runTurn`
  (`realtime_executor.go:357`).
- `runTurn` sends a `VoiceAgentTurnRequest`
  (`memql.proto:1734`) to cognition and consumes the streamed
  `VoiceAgentTurnComplete`. The comment is explicit
  (`realtime_executor.go:32-42`): "non-empty final text means the assistant
  should speak (engage), empty means the conductor/classifier suppressed the
  turn." The *content* is cognition's.
- That content is then handed to gpt-realtime not as "author a reply" but as
  "convey this": `RealtimeInstructionsForReply(reply)` renders
  *"Respond now in your persona's voice. Convey the following ... do not read
  it verbatim"* (`instructions.go:114-126`). gpt-realtime is a
  paraphrasing TTS.
- The voice-agent handler comment seals it: "the cascade routes assistant
  replies through VoiceAgentTurnRequest -- cognition runs the agent loop and
  inserts the SI utterance itself (insertSIResponse)"
  (`component/grpc/voice_agent_handlers.go:928-930`).

So **cognition is the author for voice and text alike, today.** The model does
not own the turn yet -- that is exactly what #478 ("1-on-1: model owns the
turn") is for, and #478 depends on this spike.

### 1.3 The drift that exists right now

Because the voice path was built to *speak cognition's words*, nobody noticed
that the voice persona is starved. `VoiceAgentSessionAck`
(`memql.proto:1653`) carries `ga_canonical_voice`,
`ga_avatar_persona_id`, `initial_audio_mode`, `initial_video_mode` -- and
nothing else. `Persona` (`persona.go:41-75`) therefore has
`DisplayName/Role/Description/Style` as fields it can never populate from the
ack; the struct doc says so directly: *"NOT on VoiceAgentSessionAck today; they
resolve to '' here and the instruction builder renders a neutral default
persona"* (`persona.go:64-74`). `BuildPersonaInstructions`
(`instructions.go:50`) then emits *"You are Assistant, the General
Assistant"* -- the neutral defaults from `instructions.go:25-26` --
for EVERY agent, regardless of who the user configured.

The drift today is masked only because cognition authors the words and the
model merely re-voices them. The instant #478 lets gpt-realtime author its own
turn, the starved persona stops being cosmetic: the model would author *as
"Assistant, General Assistant"* with no `personality`, no `systemPrompt`, no
`role`, no `domains`, no tool surface -- a different agent than the one the text
path renders from the full `ActingAgentIdentity`. **That is the divergence #476
must prevent before #478 can land.** The convergence contract is the
precondition for model-owns-turn, not an afterthought.

---

## 2. The single source of truth: `v1:agents:agent`

### 2.1 It already exists, and it is already the right shape

There is exactly one place an agent is defined:
`v1:agents:agent` (`dsl/agents/concepts.memql:14`). One row per
agent (per-user GA + specialists), materialized by the SeedMaterializer
(`dsl/agents/assistant.memql` header). The generation-relevant fields, all on
that one row:

| Generation input | Field on `v1:agents:agent` | `concepts.memql` line |
|---|---|---|
| Identity name | `name` | :17 |
| Persona / behavior | `personality` (system-prompt prose) | :19 |
| Extra instructions | `systemPrompt` | :20 |
| Role label | `role` (`specialist`\|`assistant`) | :23 |
| Specialty | `roleSlug` | :24 |
| Model: explicit provider | `providerConfig.llm.provider` | :50 |
| Model: explicit model id | `providerConfig.llm.model` | :51 |
| Model: router policy | `providerConfig.llm.policyName` | :52 |
| Sampling | `providerConfig.llm.{temperature,maxTokens}` | :53-54 |
| Voice id | `providerConfig.voice.voiceId` | :58 |
| Tools / knowledge | `capabilities.skillIds` (resolves to domains + toolSlugs) | :44 |
| Keywords | `capabilities.keywords` | :43 |
| Voice capability gate | `capabilities.voiceToVoice` | :40 |
| Avatar | `avatarPersonaId` / `avatarVendor` | :79-80 |
| Audio/video gate | `audioControl` / `videoControl` | :75-77 |

This is the whole generation definition: persona + grounding selectors
(`skillIds` -> domains) + tools (`skillIds` -> toolSlugs) + model
(`providerConfig.llm`) + instructions (`personality` + `systemPrompt`) + voice.
**The convergence does not need a new concept. It needs both modalities to read
the same fields off this one.**

### 2.2 How the two modalities read it today (the asymmetry)

- **Text:** cognition's forwarder reads the row and stamps the full
  `ActingAgentIdentity` (`memql.proto:1146-1158`) onto the
  `AgentGenerateTurnMsg`. `buildPromptData` projects every field into the
  `assistant` block (`integrations/agent/prompt_data.go:159-196`):
  `name`, `description`, `personality`, `systemPrompt`, `role`, `domains`,
  `keywords`, `tools`. The `cognitionReply.tmpl` renders identity + personality
  + tools from it (`dsl/cognition/prompts/cognitionReply.tmpl`, the
  `## YOUR IDENTITY` / `### Personality & Instructions` / `## YOUR TOOLS`
  blocks). Model selection runs the SI Router off
  `providerConfig.llm.*` with documented precedence
  (`replier.go:196-255`).

- **Voice:** the session bring-up sends `VoiceAgentSessionStart`
  (`memql.proto:1641`) and receives `VoiceAgentSessionAck`
  (`memql.proto:1653`) -- voice/avatar/audio only. `ResolvePersona`
  (`persona.go:115`) builds a `Persona` with neutral persona-prompt fields, and
  `BuildSessionPersona` (`instructions.go:94`) bakes the neutral instructions +
  a catalog voice into the realtime session config. Model is implicitly
  `gpt-realtime`; tools come from a separate low-risk MCP allowlist
  (`integrations/voice/agent/mcp_tool_bridge.go`), NOT from the agent's
  `skillIds`.

The two modalities read *different subsets* of the same row, through *different
wire messages*, and project them through *different builders*. That is the
drift surface. The contract closes it by introducing one projection both share.

### 2.3 Versioning

The row is already versioned three ways, which the contract inherits for free:

1. **Concept schema version.** `@version("1.0.0")` on the `agent` concept
   (`concepts.memql:10`). A breaking field change bumps this; the
   SeedMaterializer re-materializes against the new shape on a fresh DB
   (the `_agent.memql:24` "pre-prod, seed regen" note).
2. **Row mutation lineage.** Edits flow through `mutationUpdateAgent` (the
   skill-cap enforcement is server-side, `concepts.memql:45`), and the row
   carries `lineage` (`concepts.memql:89`) + the engine-stamped
   `createdBy`/`updatedAt` columns. A turn's author is pinned to the row state
   at dispatch time.
3. **Provider/model pin.** `providerConfig.llm.{provider,model,policyName}`
   (`concepts.memql:50-52`) is the explicit, user-editable model version. The
   converged contract stamps the *resolved* provider+model onto every captured
   utterance (section 6.3) so "which brain authored this" is auditable
   per-turn, on both modalities.

**No new versioning machinery.** The agent row IS the version; the contract is
a deterministic projection of a versioned row, so the contract version is the
row version. (If we later want a frozen, hash-pinned contract snapshot per
turn -- e.g. to reproduce an exact generation -- we add a `contractHash` field
to the captured utterance's `source` map, computed from the projected contract.
Out of scope for the spike; flagged in section 9.)

---

## 3. The converged contract: `AgentGenerationContract`

### 3.1 The shape

One pure Go value, projected once from the agent row, instantiated by both
endpoints. It is the union of what `ActingAgentIdentity` carries today and what
`Persona` *should* carry:

```go
// AgentGenerationContract is the single, modality-independent definition of
// how one agent authors a turn. Projected from a v1:agents:agent row by
// BuildGenerationContract; instantiated by the text endpoint (cognition's
// agentReply loop) and the voice endpoint (gpt-realtime session config)
// IDENTICALLY in its shared region. Pure/deterministic -> golden-file testable.
type AgentGenerationContract struct {
    AgentID     string
    Name        string
    Role        string   // "assistant" | "specialist"
    RoleSlug    string
    Description string
    Personality string   // the system-prompt prose
    SystemPrompt string  // extra instructions

    // Model selection -- resolved through the SAME SI Router policy chain
    // both paths already use (replier.go:196-255). For voice this resolves to
    // gpt-realtime; the contract still records the policy so the choice is
    // explicit and auditable, not implicit-by-runtime.
    Provider   string
    Model      string
    PolicyName string
    Temperature float64
    MaxTokens   int

    // Grounding selectors. skillIds resolve to domains + toolSlugs at read
    // time (the union the replier already computes, replier.go:381-419).
    Domains   []string
    ToolSlugs []string  // the privileged set; voice exposes only the low-risk subset (section 5)
    Keywords  []string

    // Voice projection. Empty/ignored on the text endpoint.
    CanonicalVoice  string
    AvatarPersonaID string
    AvatarVendor    string
    VoiceToVoice    bool
}
```

### 3.2 The one projector both endpoints call

```go
// BuildGenerationContract is the single projection from the agent row to the
// converged contract. Pure; no I/O. The forwarder (cognition) and the voice
// session bring-up both call it, so neither can read a field the other doesn't.
func BuildGenerationContract(agent AgentRow, resolved RouterPick) AgentGenerationContract
```

It lives where both callers can reach it without an import cycle -- the natural
home is a small shared package (e.g. `integrations/agentdef/`) that
`integrations/agent` (replier) and `integrations/voice/agent` (persona) both
depend on, mirroring how `prompt_data.go` and `persona.go` are siblings today
but currently duplicate logic.

### 3.3 The two renderers, one shared region

The contract is rendered into modality-specific instruction strings by two
renderers that **share a common middle**:

- **Text renderer.** Today `cognitionReply.tmpl`'s `## YOUR IDENTITY` +
  `### Personality & Instructions` + `## YOUR TOOLS` blocks. Refactored to
  render from `AgentGenerationContract` instead of the ad-hoc `assistant` map.
- **Voice renderer.** Today `BuildPersonaInstructions`
  (`instructions.go:50`). Refactored to render the SAME identity + personality
  region from the SAME contract fields.

The shared region is a single helper, `RenderIdentityBlock(contract)`, that
both renderers call for the identity + personality + domains lines. Modality
deltas (voice's spoken-register constraints, `instructions.go:66-73`; text's
tool-call choreography and emoji ban,
`cognitionReply.tmpl` `## RULES`) are layered AROUND the shared block, never
inside it. The golden test (section 7) pins `RenderIdentityBlock` output to be
byte-identical across both call sites.

### 3.4 Per-turn shaping is layered ON TOP, not baked in

The contract is the *static* per-agent definition. The conductor's per-turn
directive (mode / brevity / instruction -- `AgentParticipationDirective`,
`integrations/cognition/conductor.go:106`; `ConductorPlan`,
`conductor_consult.go:103`) is layered on top, identically on both paths:

- Text: the `## TURN OVERRIDE` block at the top of `cognitionReply.tmpl`
  (rendered from `buildDirectiveMap`, `prompt_data.go:260`).
- Voice: the per-response `instructions` on `response.create`
  (the #432 design, `docs/voice/432-conductor-response-gate.md:169-204`).

Both consume the same `AgentParticipationDirective`. Under the converged
contract the voice per-response instructions stop carrying *the authored reply*
(the v1 `RealtimeInstructionsForReply` "convey the following") and instead carry
*the turn directive* (mode/brevity/angle), exactly as text does -- because the
model is now the author, not the speaker. This is the precise behavioral change
#478 makes; #476 defines the contract it renders from.

---

## 4. Same-model vs same-definition: the feasibility analysis

This is the question #476 exists to answer. Two readings of "one brain":

### 4.1 Same-definition, two runtimes (RECOMMENDED)

Both modalities instantiate the **same `AgentGenerationContract`** but run it on
the runtime that fits the modality:

- **Voice runtime:** the gpt-realtime websocket session
  (`integrations/openai/realtime.go` per `docs/voice/453-gpt-realtime-go.md`),
  configured from the contract's persona/voice/tools, authoring natively per
  the #432 gate.
- **Text runtime:** cognition's `agentReply` streaming loop
  (`integrations/agent/replier.go:161` `handleStreaming`), authoring natively
  through the SI Router-picked provider, with the full tool loop, RAG, and
  citations it already has.

**Pros:** each runtime is already built and load-tested for its modality. The
text runtime is stateless, fan-in, fallback-chained, multi-provider
(`replier.go:270-292`) -- exactly right for a request/response chat workload.
The voice runtime is a warm, stateful, single-socket, prosody-aware
speech-to-speech engine -- exactly right for a live call. Convergence is the
*input* (the contract), which is cheap and testable to share.

**Cons:** two model families author (gpt-realtime for voice; whatever the SI
Router policy picks for text -- Sonnet-class `balancedChat`, `replier.go:253`).
Tone *could* differ if persona is under-specified. Mitigated because: (a) the
shared `RenderIdentityBlock` forces identical persona instructions; (b) the
captured utterance is the source of truth regardless of which model wrote it
(#475 "Utterance = verbatim truth"; section 6); (c) the golden test pins the
shared region.

### 4.2 Same-model, both modalities (the literal "one model")

gpt-realtime accepts text in and text out in-session: a `response.create` can
request `output_modalities: ["text"]` and the model authors a typed reply over
the same websocket
(`docs/voice/453-gpt-realtime-go.md:249-260` shows the audio variant;
the text variant is the same call with text modality). So the literal "one
model authors both" is **technically feasible**: a space's standing realtime
session could author typed turns too.

**Where it is right:** in a space that ALREADY has a warm realtime session up
(a live voice call), a typed message from a participant *should* be authored by
that same session so a person typing mid-call gets the same brain as the people
talking. This is a real convergence win and the contract supports it directly
-- the typed turn injects a `conversation.item.create` (user text) and a
`response.create` (text modality) on the existing socket, no new runtime.

**Where it is wrong:** as the **universal** text engine for every space.
Reasons, each grounded:

1. **Lifecycle mismatch.** The realtime session is warm-but-muted with a cost
   meter: idle teardown, max-duration, and a per-session audio-token budget
   (`realtime_budget.go:66-90`), with cost-guardrail teardown that *degrades to
   cascade* (`realtime_budget.go:52-58`). A text-only space has no audio, no
   room, no reason to hold a socket open. Standing up a realtime websocket per
   text-only space to author one typed message is pure standing cost for a
   request/response workload -- and ties directly into the #459 guardrails the
   issue flags.
2. **No fan-in / no fallback chain.** The text path's resilience is the SI
   Router's primary+fallback chain across providers
   (`replier.go:270`, `resolved.Chain`). A single realtime socket has no
   equivalent cascade; a provider blip drops the turn. The text path is
   *designed* to survive that.
3. **No streaming-tool loop parity (yet).** The text runtime runs a bounded
   multi-turn tool loop (`replier.go:157-160` `handleStreaming`) with the full
   privileged tool surface, RAG enrichment + citations
   (`replier.go:361-471`), and the operator/computer-use/workbench
   auto-domain injection (`replier.go:383-419`). gpt-realtime's tool surface is
   the low-risk MCP allowlist only (`mcp_tool_bridge.go`; #475 keeps privileged
   tools cognition-gated). Routing *every* text turn through realtime would
   either lose the privileged tool loop or require re-plumbing it through the
   socket -- a large regression risk for #480's "without regressing chat."
4. **Voice-less / text-only spaces are the common case.** Most spaces never
   open a voice session. Making realtime the universal text engine taxes the
   majority case to converge the minority case, when the cheaper convergence
   (shared *definition*) already removes the drift.

**Verdict on 4.2:** same-model authoring is the right call **on the voice
runtime, and opportunistically for typed turns in a space that already has a
warm voice session** -- but NOT as the standing engine for text-only spaces.
The contract makes the first true and the second optional, without forcing the
third.

### 4.3 The recommendation, stated as an invariant

> **One definition, authored natively on the runtime that fits the modality.**
> Voice turns and (mid-call) typed turns in a voice-enabled space are authored
> by gpt-realtime from the contract. Text turns in a voice-less space are
> authored by cognition's agentReply loop from the SAME contract. Neither can
> read a field the other can't, because both project from
> `BuildGenerationContract` and render the shared region through
> `RenderIdentityBlock`.

This satisfies #475's "one brain" (no authorship drift) without its overly
literal reading ("one socket for everything"), which the lifecycle analysis
rejects. It maps cleanly onto #475's own architecture line: *"1-on-1 and
multi-party share ONE core ... and differ in exactly one thing: the gate."* We
add: *and text and voice share ONE definition and differ in exactly one thing:
the authoring runtime.*

---

## 5. Cognition's new role: policy + grounding + capture, not a second author

Under the contract, cognition's responsibilities split cleanly into "stays" and
"moves to the model."

### 5.1 What STAYS in cognition (policy + grounding + capture + authz)

- **WHEN -- the conductor gate.** engage / defer / brevity. The conductor is
  the cheap gate (#432, #477), the same gate everywhere (#475). It decides
  whether a `response.create` fires and renders the per-turn directive, but it
  no longer pre-computes the words. `ConductorPlan.PrimaryAgentId() == ""` is
  still the silence gate (`conductor_consult.go:154`).
- **WHAT CONTEXT -- grounding.** RAG retrieval over the agent's domains, keyed
  on the user query (`replier.go:361-471`), with citation enrichment
  (`replier.go:439`). On voice this becomes the injected
  `conversation.item.create` grounding block
  (`integrations/voice/agent/grounding.go:111` `BuildGroundingItems`;
  the #432 design at `docs/voice/432-conductor-response-gate.md:201-204`).
  Same retrieval, two injection shapes -- one for a prompt, one for a session
  item.
- **WHO MAY -- privileged-tool authorization.** Role-locks,
  `agentAuthorization.computerUseScope` (`concepts.memql`), kill switch --
  **unchanged** (#475 non-goal: "No change to the privileged-tool authorization
  model"). gpt-realtime gets only the low-risk MCP read allowlist
  (`mcp_tool_bridge.go`); any privileged call the model wants still routes
  through cognition's gated, authorized path. The model authoring its own words
  does NOT mean the model authorizing its own privileged actions.
- **WHAT WAS SAID -- capture.** The utterance is the strong source of truth
  (#475, #482). Voice captures the model's final transcript via
  `RealtimeOutputForwarder` (`integrations/voice/agent/realtime_output.go`)
  into a `v1:cognition:utterance` byte-identical to a text reply; text captures
  via `insertSIResponse` (`integrations/cognition/si_responder.go`,
  referenced `voice_agent_handlers.go:929-930`). Both land the same row shape.

### 5.2 What MOVES to the model (authorship)

- **The words.** On voice, gpt-realtime authors the reply natively (#478),
  grounded by the injected context and shaped by the per-response directive.
  Cognition's `runTurn` round-trip (`realtime_executor.go:357`) and
  `RealtimeInstructionsForReply` "convey the following"
  (`instructions.go:114`) are **deleted** -- that is the v1 prosody-aware-TTS
  posture #475 is correcting.
- On text, authorship was already the model's (the SI Router-picked provider in
  `handleStreaming`, `replier.go:161`) -- but it now authors from the *contract*
  identity block, not an ad-hoc `assistant` map, so it cannot drift from voice.

### 5.3 The role statement

> Cognition is the **director and scribe**, not the **author**. It decides when
> the agent speaks, hands the agent its context and its turn-shaping directive,
> enforces what the agent is allowed to do, and writes down verbatim what the
> agent said. The agent (the model, instantiated from the contract) decides the
> words. One author per turn, the same author definition for both modalities.

---

## 6. Migration: converging the text path without regressing chat

The migration is deliberately staged so chat never breaks. It corresponds to
epic items #480 (text convergence) and #478 (voice authorship), both gated on
this contract.

### 6.1 Stage 0 -- land the contract (this spike's #476 -> a build PR)

Add `AgentGenerationContract` + `BuildGenerationContract` + `RenderIdentityBlock`
as **pure, unused additive code** in a shared package. No caller switches yet.
Golden test pins `RenderIdentityBlock`. Zero behavior change; CI-green proof
that the projection is sound.

### 6.2 Stage 1 -- text endpoint reads the contract (no behavior change)

Refactor `buildPromptData` (`prompt_data.go:159-196`) and the
`cognitionReply.tmpl` identity blocks to render from
`BuildGenerationContract(agent, routerPick)` instead of the ad-hoc `assistant`
map. The `ActingAgentIdentity` wire message is unchanged; only the projection
on the agent node changes. Assert (golden) that the rendered text prompt's
identity + personality region is byte-identical before and after. **This is the
"without regressing chat" guarantee: the text path's output is pinned across
the refactor.** This is the migration's heart -- it makes the existing
cognition-authored text path read the converged contract.

### 6.3 Stage 2 -- voice endpoint reads the contract (fixes the starved persona)

Extend `VoiceAgentSessionAck` (`memql.proto:1653`) to stamp the
contract's persona fields (`ga_display_name`, `ga_role`, `ga_description`,
`ga_personality`, `ga_system_prompt`, plus the resolved provider/model and the
low-risk tool allowlist) -- the exact follow-up `persona.go:70-74` and
`instructions.go:24` already anticipate ("TODO(#456-followup): populate from the
ack once the proto stamps ga_display_name / ga_role / ..."). `ResolvePersona`
(`persona.go:115`) populates the real fields; `BuildPersonaInstructions`
(`instructions.go:50`) renders the SAME `RenderIdentityBlock` the text path
uses. After this stage the voice session is configured with the user's real
agent, not "Assistant, General Assistant." Still no authorship change yet -- the
model still re-voices cognition's words -- so this stage is independently
shippable and *improves* voice fidelity even before #478.

### 6.4 Stage 3 -- voice model authors natively (#478)

Delete `runTurn`'s author round-trip (`realtime_executor.go:357`) and
`RealtimeInstructionsForReply` (`instructions.go:114`). The conductor gate now
fires `response.create` with the *directive* (not the authored reply); the model
authors. Cognition's role narrows to policy + grounding + capture (section 5).
The captured utterance (`realtime_output.go`) stamps the resolved
`gpt-realtime` provider/model in its `source` map so audit shows which brain
authored each turn. Per-turn grounding moves to the injected
`conversation.item.create` (already designed, `grounding.go:111`).

### 6.5 Stage 4 -- opportunistic same-model typed turns (optional, post-#478)

In a space with a warm realtime session, route a participant's *typed* message
to the existing socket (text-modality `response.create`) instead of the text
runtime, so mid-call typing gets the same authoring brain. Falls back to the
text runtime if no session is warm. This is the *only* place same-model-both-
modalities (4.2) is used; it is opt-in per-space-state, never the default for
text-only spaces.

### 6.6 Rollback posture

Every stage is independently revertable. Stages 0-2 are behavior-preserving by
golden test. Stage 3 sits behind the existing `MEMQL_VOICE_EXECUTOR` selection
seam (`executor_select.go:110`) and its clean cascade fallback
(`executor_select.go:117-124`) -- if native authorship misbehaves, the session
degrades to the cascade exactly as it does for any realtime build failure
today. The text path (stages 0-2) has no flag because it is proven
byte-identical.

---

## 7. Consistency guarantees + how we test that voice and text can't diverge

The whole point of the contract is that drift is impossible by construction, not
by discipline. Three layers of guarantee, each with a test in the default
(CGO-free) CI lane:

### 7.1 Structural: one projector, one shared renderer

There is exactly one `BuildGenerationContract` and one `RenderIdentityBlock`.
Neither endpoint hand-rolls an identity block. A reviewer rejects any PR that
renders persona/identity outside `RenderIdentityBlock` -- that is the "smell to
reject in review" analog of #475's single-core rule.

### 7.2 Golden: the shared region is byte-identical

A table test feeds N representative agent rows (GA, a specialist, an agent with
custom `personality` + `systemPrompt`, an agent with empty persona) through
`BuildGenerationContract`, then renders BOTH the text identity block and the
voice identity block, and asserts the shared region is character-for-character
equal. This is the analog of the existing pure-renderer tests already in the
tree (`instructions_test.go`, `grounding_test.go`, `persona_test.go`,
`prompt_data` coverage). A drift in either renderer fails this test.

### 7.3 Behavioral: capture parity (spoken == shown)

#482's "utterance single-source-of-truth" is the runtime guarantee that what is
*shown* equals what was *authored*. Both capture paths land the same
`v1:cognition:utterance` shape (`realtime_output.go` for voice,
`insertSIResponse` for text). A test asserts that a voice turn and a text turn
for the same agent produce utterance rows whose `source` map records the SAME
`agentId` and a resolved provider/model, so audit can prove which brain wrote
each turn and that both read the same definition. The verbatim transcript is the
truth; the contract guarantees the *author* was the same agent definition; the
capture guarantees the *record* is what was said.

### 7.4 What "cannot diverge" precisely means

The contract does NOT guarantee gpt-realtime and a Sonnet-class text model emit
identical prose (different models, different phrasings -- that is fine and
expected). It guarantees they author from the **same persona, role, domains,
tool policy, and instructions**, and that the **record of what each said is
verbatim**. Divergence in *tone* from under-specified persona is caught by the
golden test (the persona block is identical); divergence in *what was shown vs
said* is caught by capture parity. The two failure modes #475 names -- "drift in
tone/knowledge/policy" and "what's shown must equal what was said" -- each have a
test.

---

## 8. Acceptance criteria mapping

### 8.1 To issue #476

- [x] **"Single source of truth: where it lives, how it's versioned, how both
  modalities read it."** -> `v1:agents:agent`
  (`dsl/agents/concepts.memql:14`), versioned by concept `@version` + row
  lineage + the `providerConfig.llm` model pin (section 2.3), read by both
  through `BuildGenerationContract` -> `RenderIdentityBlock` (section 3).
- [x] **"Can/should gpt-realtime serve typed turns too; cost/lifecycle for
  text-only/voice-less spaces (ties to #459)."** -> Feasible (text-modality
  `response.create`); right for voice + mid-call typing, wrong as the universal
  text engine (section 4.2), because the warm-socket lifecycle + cost guardrails
  (`realtime_budget.go:66-90`) and the missing fan-in/fallback/privileged-tool
  loop make it a poor fit for the common voice-less case.
- [x] **"Cognition's new role: policy + grounding + capture, NOT a second
  author. What stays vs moves to the model."** -> Section 5: WHEN (gate) /
  CONTEXT (RAG) / WHO-MAY (authz, unchanged) / WHAT-WAS-SAID (capture) stay; the
  WORDS move to the model.
- [x] **"Migration: existing cognition-authored text path onto the converged
  contract without regressing chat."** -> Section 6, staged 0-4; stages 0-2 are
  byte-identical-by-golden-test, stage 3 sits behind the existing executor seam
  with cascade fallback.
- [x] **"Consistency guarantees + how we test that voice and text cannot
  diverge."** -> Section 7: structural (one renderer) + golden (shared region
  byte-identical) + behavioral (capture parity / spoken==shown).
- [x] **"VERDICT on same-model vs same-definition + recommendation, mapped to
  #475 acceptance."** -> Section 0 + section 4.3: same-definition two-runtimes,
  same-model for voice only. Mapped below.

### 8.2 To epic #475's acceptance (what this keystone unblocks)

- [~] **"Exactly one agent author: voice and text share one definition
  (persona/grounding/tools/model/instructions); no drift."** -> This spike
  DEFINES that one definition (the contract) and the test that pins it (section
  7). Landing it is #480 (text) + #478 (voice authorship); the spike is the
  precondition both depend on.
- [~] **"1-on-1 voice responds at near-native latency; no conductor round-trip
  in the generation path."** -> Enabled by section 6.4 deleting the
  `runTurn` author round-trip; the latency *measurement* is #484.
- [x] **"Privileged-tool authorization, grounding, citations, audit, and
  cascade fallback all preserved."** -> Section 5.1 keeps authz unchanged,
  grounding/citations in cognition, capture for audit; section 6.6 keeps the
  cascade fallback. No #475 non-goal is touched.

Legend: [x] satisfied / specified by this spike; [~] this spike is the
keystone the dependent build issues (#478/#480/#482/#484) consume -- the
contract + tests are designed here, the runtime switch and live measurement
land there.

---

## 9. Risks / open questions

Settled: the source of truth exists and is one row (section 2); the contract is
a pure projection (section 3); same-definition is unambiguously the right
convergence (section 4). The remaining unknowns are build-shaped and owned by
the dependent issues:

1. **Tone parity across model families.** gpt-realtime vs the text policy's
   Sonnet-class model author the same persona but in different prose. The
   contract pins the persona instructions; whether the *felt* voice is the same
   needs a human read across a few agents post-#478. Mitigation lever exists:
   pin the text path's `policyName` for a converged agent to a model family
   whose register matches gpt-realtime, via `providerConfig.llm.policyName`
   (`concepts.memql:52`). Not a blocker -- a tuning knob.
2. **Proto bump scope (stage 2).** Extending `VoiceAgentSessionAck`
   (`memql.proto:1653`) with persona/model/tool fields is additive and
   anticipated by the existing TODOs (`persona.go:70`), but it crosses the
   voice-agent process boundary in any out-of-process build. Single-binary
   builds collapse it to a struct fill; the proto stays valid for the
   out-of-process variant (same posture as #453's note,
   `docs/voice/453-gpt-realtime-go.md:436-441`).
3. **Frozen contract snapshot per turn.** Section 2.3 notes a future
   `contractHash` on the captured utterance's `source` map for exact-repro audit.
   Deferred; the row version + stamped resolved provider/model is enough for v1
   auditability.
4. **Mid-call typed-turn routing (stage 4).** Deciding *which* typed turns go to
   the warm socket vs the text runtime (e.g. a participant typing while others
   talk) is a small policy in cognition's dispatch; it must not starve the voice
   turn-taking on the same socket. Opt-in, post-#478, flagged here, owned by a
   follow-up.
5. **Skill -> domains/toolSlugs resolution timing.** The contract's `Domains` /
   `ToolSlugs` are the union of `capabilities.skillIds` bundles, resolved at
   read time (`concepts.memql:44`; `replier.go:381-419`). Voice and text must
   resolve them identically; the cleanest guarantee is to resolve once in
   `BuildGenerationContract` so both endpoints consume the same resolved set,
   not re-resolve per modality.

None of these gate the verdict. They are the surface #478/#480/#482/#484 build
against.

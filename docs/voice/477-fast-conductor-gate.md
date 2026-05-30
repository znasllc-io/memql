# Fast conductor gate (WHEN not WHAT) + latency budget

Spike deliverable for issue #477, part of epic #475 ("Realtime voice v2:
model-owns-turn + converge to one generation brain"). Phase 0 design. No
production code changes in this PR.

Status: design + feasibility verdict, grounded in the current Go code. The
live decision->first-audio measurement (a credentialed LiveKit room + OpenAI
Realtime + Deepgram) is the explicit follow-up #484 and is flagged throughout;
this spike specifies exactly which numbers only live infra can settle and
estimates the rest from the code as it stands today.

**VERDICT: GO.** A cheap engage/defer/brevity gate is viable and the budget
holds. See [section 8](#8-verdict).

> **Relationship to #432.** Issue #432's conductor-response-gate design has
> shipped: the realtime executor (`integrations/voice/agent/realtime_executor.go`)
> already runs `turn_detection:null` and drives exactly one `response.create`
> per conductor decision. But #432's gate is a *byproduct of authoring* -- the
> conductor still composes the reply text in cognition and the model re-voices
> it (`RealtimeInstructionsForReply`, `instructions.go:114`). That is the
> ~1-1.5s round-trip epic #475 is trying to delete. This spike redesigns the
> gate so the WHEN/brevity decision is decoupled from the WHAT (the reply),
> and validates that the WHEN decision alone can be cheap enough to stay off
> the generation critical path. #432 proved memQL *can* gate the model; #477
> proves the gate can be *fast*.

---

## 1. Problem restatement

Epic #475's architecture has one load-bearing assumption:

> **Conductor = WHEN (cheap gate).** engage / defer / brevity, heuristic-first,
> off the critical path for generation. **Model = WHAT (the reply).** Generated
> natively.

Today the conductor is a reply *author*, not a gate. Two paths show this:

1. **Text path.** A text utterance without a fast-path hit runs
   `consultConductor` (`cognition_handler.go:601`), a single structured-output
   LLM call that emits both the routing decision *and* the per-agent plan +
   per-turn instruction. The code comment at `cognition_handler.go:586-596`
   records why voice does **not** take this path: the conductor's "~1-1.5s
   sequential LLM call dominated every voice turn end-to-end" (tried in
   `f5e2af9`, reverted).

2. **Voice path (current realtime).** Even though #432 made the realtime model
   own prosody, the *content* still comes from cognition: the GA's reply lands
   over the `VoiceAgentTurnComplete` seam as fully-authored text, and
   `RealtimeInstructionsForReply` (`instructions.go:114`) tells the model to
   "convey the following ... as spoken dialogue." The model is a prosody-aware
   re-voicer behind the authoring round-trip. The gate ("did cognition produce
   non-empty text") is real (`realtime_executor.go:416`) but it is *derived
   from having already done the expensive thing.*

So the realtime model's ~170ms native first-audio latency (epic #475 "Why") is
buried under a ~1-1.5s authoring step in front of it. The fix epic #475 wants
is: let the model generate WHAT natively, and have the conductor decide only
WHEN (and how brief) -- a decision that must be cheap enough to overlap, or
fully precede, the model's own first-token latency rather than serializing a
second LLM call in front of it.

This spike answers the issue's questions:

- Which signals make the engage decision cheap + heuristic-first, and what does
  each cost (sub-50ms target, sub-10ms reach)?
- Where does the gate risk needing semantic understanding, and what is the
  bounded escalation that does not put a full LLM back on the critical path?
- How does the gate map to a per-turn directive (mode + brevity) injected into
  the model's response instructions?
- What is the decision->first-audio latency budget, gate vs the current
  ~1-1.5s authoring round-trip?
- How does one gate serve 1-on-1 (off) and multi-party (traffic-cop) -- the
  "one pipeline, one optional gate" invariant?

---

## 2. What "the gate" must produce (and what it must NOT)

The gate's output is small and bounded. For a given freshly-committed user
turn (a `VoiceAgentFinalTranscript`, `voice_agent_handlers.go:558`), the gate
emits one of:

| Decision  | Wire effect on the realtime model                                  |
|-----------|--------------------------------------------------------------------|
| **engage**  | one `response.create` with a per-response `instructions` directive |
| **defer**   | nothing -- the floor stays with the humans                         |
| (brevity)   | rides on engage: `short` \| `normal` \| `detailed`                 |

Plus, while the assistant is mid-response, the gate answers a second question:

| Decision  | Wire effect                                                          |
|-----------|---------------------------------------------------------------------|
| **interrupt** | `response.cancel` + `output_audio_buffer.clear` (barge-in)      |
| **hold**      | nothing -- the assistant keeps talking over side chatter        |

What the gate must **NOT** do:

- It must not author the reply. The reply is the model's job (epic #475's "one
  brain"). The gate never produces `response`-shaped text; it produces a
  *mode + brevity directive* the model conditions on.
- It must not add a serial LLM call in front of `response.create` on the common
  case. The whole point is that engage/defer is decidable from signals memQL
  already has by the time the final transcript lands.

This is the same engage/defer/brevity vocabulary the existing
`AgentParticipationDirective` already carries -- `DirectiveMode`
(`primary` / `chimein` / `brief_ack` / `defer`, `conductor.go:63-83`) and
`Brevity` (`short` / `normal` / `detailed`, `conductor.go:87-92`). The gate
reuses these types; it just produces them cheaply rather than as the output of
a reply-authoring LLM call.

---

## 3. Cheap, heuristic-first engage signals

The engage decision is "should the assistant take this turn?" In 1-on-1 it is
almost always yes (section 6). In multi-party it is the traffic-cop call. The
signals below are ordered cheapest-first; the gate evaluates them as a
short-circuiting ladder and returns the moment a high-confidence signal fires.

Every one of these signals already exists in the Go tree as a byproduct of the
Polyphon scorer (`component/polyphon/scoring.go`) and the message classifier
(`message_classifier.go`). The gate does not invent new signal extraction; it
*reorders* the existing signals so the cheap ones decide first and the
expensive ones (the LLM classifier) become the bounded escalation, not the
default.

### 3.1 Addressed-by-name (direct address) -- decisive, ~sub-1ms

`scoreDirectAddress` (`scoring.go:218`) already computes this. Two sub-cases:

- **Structured `@mention`** (`scoring.go:223-243`): when the SPA tagged the
  utterance with `Mentions[]`, an addressee mention scores `direct_address =
  1.0`. This is a map walk over a handful of mentions -- pointer-chasing, no
  allocation, **sub-microsecond**. The existing deterministic override at
  `cognition_handler.go:666` (`polyphonDirectAddressWinner`, value `>= 1.0`)
  already trusts this over the LLM router; the gate trusts it to mean
  **engage** for the named agent.
- **Token name-match fallback** (`scoring.go:245-303`): when no structured
  mentions exist, a lowercased token-set match against the agent's name, with a
  start-of-utterance / "hey {name}" heuristic distinguishing primary (1.0) from
  secondary (0.4). `tokenizeWords` over a short utterance is **a few
  microseconds** -- still off-the-charts cheap relative to any network call.

Gate rule: `direct_address >= 1.0` for the GA (or the single in-room agent)
-> **engage**, short-circuit. This is the "5 humans, one asks the assistant"
acceptance case from epic #475, decided with zero LLM cost.

### 3.2 Single-agent room (turn allocation) -- decisive, ~sub-1ms

The handler already has a single-agent fast path
(`cognition_handler.go:407-417`): "if exactly one SI candidate is in the room,
the winner is obvious and the LLM router is pure overhead ... saves a measured
~1000ms of routeMs per voice turn." The canonical case is a daily space (one
user + Sofia the GA) and every 1-on-1 standard space.

Gate rule: `len(candidates) == 1` -> the WHO is settled; the gate only has to
decide engage-vs-defer for *that* agent, which in a 1-on-1 collapses to "did
the user produce a real turn" (section 3.5) rather than "which of N agents."
**This is the structural reason the gate is OFF for 1-on-1** (section 6): there
is no traffic to cop.

### 3.3 Who-spoke-last / conversational thread -- decisive, ~sub-1ms

`scoreConversationalThread` (`scoring.go:310`) and the conductor's explicit
`lastResponder` input (`conductor_consult.go`, surfaced top-of-prompt in
`conductorTurn.tmpl`) capture "the user is mid-thread with agent X." When the
just-committed turn is a follow-up shape and the user was last talking to a
specific agent within the thread-timeout window (`VoiceThreadTimeout`, default
60s, `scoring.go:334-339`), the floor belongs to that agent.

Gate rule: an active thread with the GA (or the only agent) + a non-defer
classifier intent (section 3.5) -> **engage** that agent. This is the
conductor's existing "conversational continuity" principle, evaluated as a
timestamp comparison instead of an LLM judgment.

### 3.4 Presence / turn-allocation guards -- decisive, ~sub-1ms

`ConductorState` (`conductor.go:258`) already tracks the cheap state guards:

- `HumanIsTyping` / `IsHoldingDispatches()` (`conductor.go:407-422`): a human is
  mid-message -> **defer** (don't talk over them). Pure boolean + a
  `time.Now().Before(DispatchHoldUntil)` compare.
- `RecordAgentSpoke` / `AgentSpokeThisCycle` (`conductor.go:319,346`):
  anti-monopoly -- the agent already took this cycle.
- Greet-on-join pacing (`greet_on_join.go`): the 3s initial / 4s inter-greeting
  serialization is already a cheap timestamp gate that prevents a chorus.

These are read-side checks on an in-memory mutex-guarded struct: **nanoseconds**.

### 3.5 Cheap turn-completeness shape heuristic -- decisive, ~microseconds

Voice has a failure mode text does not: Deepgram commits a thinking-pause as
`is_final`, and a naive gate would engage on "um, let me think...". The handler
already mitigates this with two pure-Go string heuristics that run *before* any
LLM:

- `looksIncompleteVoiceUtterance(text)` (`cognition_handler.go:3079-3106`):
  ends in a function word / no terminal punctuation / too short -> the user is
  mid-thought -> **defer**.
- `isConversationalAckIntent(intent)` (`cognition_handler.go:3070`): "ok",
  "thanks", "got it" -> conversational ack, no action -> **defer**.

The `looksIncompleteVoiceUtterance` check is string scanning over one short
transcript -- **single-digit microseconds**. It is the cheap backstop for the
"don't barge in on a pause" case without consulting any model.

### 3.6 Cost summary of the heuristic ladder

| Signal                              | Source (real symbol)                                  | Cost (estimated) |
|-------------------------------------|-------------------------------------------------------|------------------|
| Structured `@mention` address       | `scoring.go:223` (`scoreDirectAddress`)               | sub-microsecond  |
| Single-agent room                   | `cognition_handler.go:407`                            | sub-microsecond  |
| Presence / typing / hold guards     | `conductor.go:407` (`IsHoldingDispatches`)            | nanoseconds      |
| Who-spoke-last / thread             | `scoring.go:310` (`scoreConversationalThread`)        | sub-microsecond  |
| Token name-match fallback           | `scoring.go:245`                                      | a few microseconds |
| Turn-completeness shape             | `cognition_handler.go:3079` (`looksIncompleteVoiceUtterance`) | single-digit microseconds |
| Domain keyword relevance            | `scoring.go:388` (`scoreDomainRelevanceKeywords`)     | tens of microseconds (stemming over short text) |
| **Full heuristic ladder, worst case** | sum of the above + the Polyphon scorer pass         | **well under 1ms** |

The Polyphon scorer's whole job (`scoreAgent`, six factors, `scoring.go:154`)
already runs per voice turn today (`cognitionScore`, `capabilities.go:24`) and
is the input to the deterministic direct-address override. Its cost is
in-process arithmetic over a handful of candidate agents -- **sub-millisecond**
in aggregate. The gate is, in the common case, *a reordering of work memQL is
already doing,* not new work. The sub-50ms target is met by roughly three
orders of magnitude; the sub-10ms reach is met by two-plus.

[OK] The engage decision is heuristic-first and sub-millisecond on every signal
above. No network call, no LLM, no DB round-trip on the common path.

---

## 4. Where the gate risks needing semantics, and the bounded escalation

The heuristic ladder decides the easy ~majority of turns. It cannot, on its
own, settle one class:

> **"They implicitly asked me."** A human says, to the room, "I wonder how the
> OAuth refresh actually works" -- no name, no `@mention`, no active thread with
> the GA, terminal punctuation present (not an incomplete fragment), but it is
> *semantically* a question the assistant should answer. The heuristics see "no
> direct address, multi-agent room, no thread" and would lean **defer**; the
> right answer is **engage, brief**.

This is exactly the case the LLM-driven classifier exists for. The bounded
escalation has three properties that keep it off the generation critical path:

### 4.1 The classifier is already a *tiny, fast, cached* model -- not the conductor

`messageClassifier.Classify` (`message_classifier.go:194`) is a single
structured-output call returning a small fixed schema
(`MessageClassification`, `message_classifier.go:41`): `intent`,
`carriesAction`, `answersPriorAgentPrompt`, `addressedAgentName`,
`addressedToRoom`, `confidence`. It is **not** the conductor: it does not
author a reply, does not load the full transcript window or session memory, and
does not produce per-agent plans. It answers semantic *facts about one
utterance* that "don't depend on which agents are in the room"
(`message_classifier.go:32-36`).

Critically:

- **It already runs on voice today, in parallel** (`cognition_handler.go:431`
  region; the classifier goroutine is part of the `errgroup` that also loads
  context). It is *not* a new dependency the gate introduces.
- **It is cached, 24h TTL, hash-keyed on (userText, lastAgentText)**
  (`message_classifier.go:177,262`). Repeated phrasings ("how does that work")
  return from the in-memory map with **zero network cost** -- the same
  microsecond-class lookup as the heuristics. The cold-miss cost is one small
  structured-output call; the cache makes the *amortized* cost approach the
  heuristic floor as a room warms up.
- **It fails open** (`classifyUnknown`, `message_classifier.go:247`): on any
  error it returns `unknown` / confidence 0, and the caller treats low
  confidence as "let the dispatch through" -- a false silence costs more than a
  false dispatch (`message_classifier.go:189-193`).

### 4.2 Escalation is bounded by running it *in parallel*, never in series

The architecture that keeps the bounded escalation off the critical path is
already present in the handler: the classifier goroutine runs concurrently with
context loading inside one `errgroup` (`cognition_handler.go`,
`g.Go(func() ...)` blocks around line 431-490, joined at `_ = g.Wait()`). The
gate inherits this shape:

1. Run the **heuristic ladder synchronously** (section 3). Sub-millisecond.
2. **If a high-confidence heuristic fired** (direct address, single-agent
   engage, clear incomplete-fragment defer) -> **decide now, do not wait for
   the classifier.** The classifier result is discarded for this turn (it still
   warms the cache for the next identical phrasing).
3. **Only the ambiguous residue** (no address, multi-agent, no thread, but a
   complete-looking utterance) waits on the already-in-flight classifier
   result. Its `intent` (`question` / `request_action` vs `affirmation` /
   `follow_up` / `farewell`) + `carriesAction` + `addressedToRoom` resolve
   "implicitly asked me" without a second model and without the conductor.

So the escalation is not "fall back to a slower brain when unsure" in series.
It is "the cheap model was already running concurrently; consult its result
*only for the turns the heuristics couldn't settle, and only as the tiebreak.*"
The full-LLM conductor authoring call is **never** reintroduced on the voice
critical path -- that is the regression epic #475 forbids, and the gate's whole
discipline is to not re-add it.

### 4.3 The escalation ceiling: one small classifier call, parallelizable, cached

| Path                        | Critical-path cost added by the gate                                  |
|-----------------------------|------------------------------------------------------------------------|
| Heuristic-decided turn      | ~0 (sub-1ms, overlaps nothing)                                          |
| Ambiguous, classifier-cache hit | ~0 (in-memory map lookup)                                          |
| Ambiguous, classifier-cache miss | one small structured-output call, run **in parallel** with the model's session warm-up / input commit -- not serialized in front of `response.create` |

The worst case the gate can produce is one small cached classifier call, and
even that overlaps the input-commit + session activity rather than blocking
`response.create`. There is no path on which the gate adds the conductor's
~1-1.5s authoring round-trip.

[OK] The semantic-understanding risk is real but bounded to a tiny, cached,
fail-open classifier that already runs in parallel today. No full LLM returns to
the critical path.

---

## 5. Mapping the gate to a per-turn directive (mode + brevity)

When the gate says **engage**, it must hand the model a per-turn directive that
shapes the response -- *without* authoring it. The mechanism already exists; the
change is what the directive *carries*.

### 5.1 The injection seam is `instructions` on `response.create`

The realtime executor already renders a per-response `instructions` string and
passes it to `CreateResponse` (`realtime_executor.go:466,479`;
`CreateResponse(instructions string) error`, `realtime_executor.go:64`). The
OpenAI Realtime API's `response.create` accepts a per-response `instructions`
that overrides the session default for that one response. This is the exact
injection point for the gate's mode+brevity directive.

Today that string is `RealtimeInstructionsForReply(reply)` (`instructions.go:114`)
-- it wraps the *conductor-authored reply text*. Epic #475's change is to
replace the authored-text payload with a **content-free directive** the model
conditions its own generation on. Sketch of the target renderer (a sibling to
the existing one; not built in this spike):

```
RealtimeInstructionsForDirective(d *AgentParticipationDirective) string
```

- `Mode` (`conductor.go:63`):
  - `primary`    -> "Answer the user directly and substantively."
  - `brief_ack`  -> "Acknowledge in one short sentence. No agenda."
  - `chimein`    -> "Add only your distinct angle; do not restate." (multi-agent)
  - `defer`      -> the gate never calls `CreateResponse` (suppress).
- `Brevity` (`conductor.go:87`):
  - `short`    -> "Keep it to one short sentence."
  - `normal`   -> "Keep it to a few sentences."
  - `detailed` -> "A longer answer is warranted; stay focused."
- Optional one-line framing (`GlobalGuidance` / `UserIntent` /
  `Temperature`, `conductor.go:158-181`) when the gate has cheaply derived it
  (e.g. the classifier's `intent`), so the spoken register matches the room.

The persona + grounding stay session-level (`BuildPersonaInstructions`,
`instructions.go:50`; the per-turn directive "overrides these defaults where
they conflict", `instructions.go:72`). The directive carries *only* the
turn-shaping mode + brevity -- the model generates the words.

### 5.2 The directive is built from cheap signals, not an authoring call

The mode + brevity for the gate's directive come straight from the heuristic
ladder + (when escalated) the classifier:

- direct address / single-agent engage on a question -> `primary`, `normal`.
- classifier `intent=affirmation`/`follow_up` that nonetheless warrants a reply
  (e.g. answers a prior prompt) -> `brief_ack`, `short`.
- classifier `intent=question` + `carriesAction=false`, implicit room ask ->
  `primary`, `short` (the "implicitly asked me" case from section 4).

No LLM authors this directive. `BuildDirective` (`conductor.go`, the directive
builder) and the cheap signals populate it. The model receives "answer, briefly"
and generates the answer itself -- which is the entire point of "one brain":
the voice reply and a future text reply both come from the model conditioned on
the same directive vocabulary, so they cannot drift in content (only in
modality), satisfying epic #475's "exactly one author" acceptance line.

[OK] The gate maps to a per-turn `instructions` directive via the existing
`CreateResponse` seam; the directive carries mode+brevity, never authored text.

---

## 6. One pipeline, one optional gate (1-on-1 off, multi-party traffic-cop)

Epic #475's invariant: "1-on-1 and multi-party share ONE core (model
generation + grounding + capture + authz) and differ in exactly one thing: the
gate is OFF for 1-on-1 and a fast traffic-cop for multi-party." The design above
satisfies this structurally, not by forking.

### 6.1 Why the gate is naturally OFF for 1-on-1

A 1-on-1 space is `len(candidates) == 1` (section 3.2). In that topology:

- There is no "which agent" question -- the single-agent fast path already
  settles it (`cognition_handler.go:407`).
- There is no "is the human talking to another human" question -- there is only
  one human and one agent.
- The only residual question is "is this a real user turn vs a thinking pause,"
  which is the cheap `looksIncompleteVoiceUtterance` / ack heuristic
  (section 3.5), not a traffic-cop decision.

So for 1-on-1 the gate degenerates to a near-passthrough: every complete user
turn engages the model; only obvious fragments/acks defer. With
`semantic_vad` owning turn-end natively (epic #475 issue #478), even the
fragment case largely dissolves -- `semantic_vad` decides turn-end by meaning,
not a fixed silence timer, so the model rarely sees a half-thought as a turn.
The gate adds essentially zero latency and zero decisions in 1-on-1: it is
**OFF** in the sense that matters (no traffic-cop logic runs), reached by the
*same* code path returning early on the single-agent branch.

### 6.2 Why the gate is a fast traffic-cop for multi-party

A polyphon room is up to 5 humans + up to 3 SI agents (per copresent's space
architecture). Here the heuristic ladder *is* the traffic cop:

- direct address routes to the named agent (section 3.1).
- thread continuity keeps the floor with the mid-thread agent (section 3.3).
- presence/typing/anti-monopoly guards prevent chorus and barge-over
  (section 3.4).
- the cheap completeness/ack heuristic suppresses side-chatter acks
  (section 3.5).
- the residual "implicitly addressed" case escalates to the parallel cached
  classifier (section 4).

Same signals, same code, same directive vocabulary. The *only* difference from
1-on-1 is that in multi-party the ladder actually has work to do (multiple
candidates, multiple humans), whereas in 1-on-1 it short-circuits on the
single-agent branch. There is no separate multi-party stack to maintain -- which
is exactly the "smell to reject in review" epic #475 calls out.

### 6.3 Interruption is the same gate, mid-response

The barge-in decision (interrupt vs hold) reuses the identical signals.
`realtime_executor.go` already has the barge-in mechanism (`onBargeIn`,
`response.cancel` + `output_audio_buffer.clear`, `realtime_executor.go:492`).
The *policy* of whether a given human onset should interrupt is the gate again:

- **1-on-1:** any human onset during assistant-turn is an interruption (the
  simple, correct default -- one human, they want the floor).
- **multi-party:** interrupt only on a floor-change read (direct address /
  the same direct-address + classifier signals), hold on side chatter -- so the
  assistant keeps talking over two humans chatting to each other.

One gate, two modes, one mechanism.

[OK] The gate is OFF for 1-on-1 (single-agent short-circuit) and a fast
traffic-cop for multi-party, both reached through one code path and one signal
set. The invariant holds.

---

## 7. Latency budget: decision -> first assistant audio

The headline comparison epic #475 asks for: gate cost vs the current ~1-1.5s
authoring round-trip.

### 7.1 The two timelines

**Current realtime path (gate-as-authoring-byproduct):**

```
user final transcript lands (voice_agent_handlers.go:558)
   |
   |  cognition consults the conductor / agent tool loop, AUTHORS the reply
   |  ~1-1.5s (cognition_handler.go:586-596 comment; the dominant term)
   v
VoiceAgentTurnComplete carries the authored text to the executor
   |
   |  RealtimeInstructionsForReply wraps it (instructions.go:114)
   v
response.create with "convey the following" + the authored text
   |
   |  model re-voices: ~170ms native TTFB (epic #475 "Why")
   v
first assistant audio frame
```

Decision->first-audio is dominated by the ~1-1.5s authoring step **in series**
in front of the model. The model's ~170ms is buried.

**Target gate path (WHEN-only, model authors WHAT):**

```
user final transcript lands (voice_agent_handlers.go:558)
   |
   |  HEURISTIC LADDER (section 3): sub-1ms, in-process
   |    + (only on the ambiguous residue) a parallel cached classifier
   |      that overlaps the input commit / session activity, never serial
   v
GATE DECISION: engage(mode, brevity) | defer        <-- sub-millisecond common case
   |
   |  RealtimeInstructionsForDirective renders mode+brevity (content-free)
   v
response.create with the directive (no authored text)
   |
   |  model GENERATES + voices natively: ~170ms native TTFB
   v
first assistant audio frame
```

Decision->first-audio is now ~`gate_cost + model_TTFB`. With `gate_cost` under
1ms on the common path, the budget is **dominated by the model's native ~170ms**
-- which is the entire goal: "1-on-1 voice responds at near-native latency; no
conductor round-trip in the generation path" (epic #475 acceptance).

### 7.2 Estimated budget

| Term                                  | Current path        | Target gate path                     |
|---------------------------------------|---------------------|--------------------------------------|
| Gate / authoring (decision)           | ~1000-1500ms (LLM authoring) | <1ms heuristic; <=1 cached small classifier call on the residue, parallelized |
| memQL -> executor push                | tens of ms          | tens of ms (unchanged seam)          |
| Model TTFB (first audio)              | ~170ms (re-voicing) | ~170ms (native generation)           |
| **Decision -> first audio (headline)**| **~1.2-1.7s**       | **~0.17s + push (dominated by model)** |

The gate removes the ~1-1.5s authoring term from the critical path entirely on
the heuristic-decided majority. The remaining latency is the model's own native
budget, which epic #478 (semantic_vad + native generation) is independently
chasing. The defer/silence path is even cheaper: it emits **no** `response.create`
at all (`realtime_executor.go:416`), so its "latency" is just the sub-ms gate
decision.

### 7.3 What only live infra settles (-> #484)

The estimates above are grounded in real code costs (in-process arithmetic for
the heuristics; the existing single-agent fast path's *measured* ~1000ms saving,
`cognition_handler.go:410`, is direct evidence the LLM term is the dominant one)
but the felt decision->first-audio number requires a credentialed room. Reuse
the existing structured voice-trace convention (`stage` +
`voiceTrace: voice:<spaceId>`; the executor already emits
`"voice trace: turntaking event"`, `realtime_executor.go:471`). Stamps:

| Stamp                          | Where                                          | `stage`                         |
|--------------------------------|------------------------------------------------|---------------------------------|
| T0 user final lands            | `voice_agent_handlers.go:558`                  | `voice.final`                   |
| T1 gate decision (engage/defer)| gate evaluation site (new)                     | `gate.decide`                   |
| T2 `response.create` sent      | `realtime_executor.go:479`                     | `turntaking.assistant.speak`    |
| T3 first assistant audio frame | first audio delta (AudioOut drain)             | `realtime.audio.first`          |

Headline: T3 - T1 (decision -> first audio). Splits: T1 - T0 (gate cost --
expected sub-ms on heuristic hits; bounded by one cached classifier call on the
residue), T3 - T2 (model native TTFB). Gate-correctness on the defer path is
measured by **absence**: no `response.create` over the window when the gate
defers (the "5 humans cross-talk, none address the assistant" scenario).

**[WARNING] The live decision->first-audio measurement requires real OpenAI
Realtime + LiveKit + Deepgram credentials and a real room; it is issue #484, not
executed within this spike.** This spike supplies the methodology, the
instrumentation points, and the code-grounded estimate.

---

## 8. Verdict

### GO

A cheap engage/defer/brevity gate is **viable**, and the latency budget holds:
decision->first-audio drops from the current ~1.2-1.7s (dominated by the
~1-1.5s authoring round-trip, `cognition_handler.go:586-596`) to ~the model's
native ~170ms + push, because the gate decision is sub-millisecond on the
common path and never re-adds a serial LLM authoring call.

**Why GO (the de-risking landed):**

- **Every engage signal already exists as cheap in-process work.** Direct
  address (`scoring.go:218`), single-agent turn allocation
  (`cognition_handler.go:407`), who-spoke-last/thread (`scoring.go:310`),
  presence/typing/anti-monopoly guards (`conductor.go:407`), and the
  turn-completeness shape heuristic (`cognition_handler.go:3079`) are all
  sub-millisecond and run today. The gate *reorders* existing work so the cheap
  signals decide first; it does not invent new extraction. The single-agent
  fast path's already-**measured** ~1000ms saving is direct evidence the LLM
  term is the one to remove.
- **The bounded escalation is already a tiny, cached, fail-open classifier**
  (`message_classifier.go:194`) that runs in parallel today. The "implicitly
  asked me" semantic case is handled without putting the full conductor LLM
  back on the critical path: the classifier is consulted only for the ambiguous
  residue, only as the tiebreak, and its result overlaps session activity rather
  than serializing in front of `response.create`.
- **The injection seam exists.** `CreateResponse(instructions)`
  (`realtime_executor.go:64,479`) already takes a per-response directive; the
  change is to carry a content-free mode+brevity directive
  (`RealtimeInstructionsForDirective` over the existing `DirectiveMode` /
  `Brevity` types, `conductor.go:63,87`) instead of the authored reply text
  (`RealtimeInstructionsForReply`, `instructions.go:114`).
- **One pipeline, one optional gate is structural.** The gate is OFF for 1-on-1
  by the single-agent short-circuit and a fast traffic-cop for multi-party
  through the *same* ladder and the *same* `CreateResponse` seam. No forked
  stack -- the invariant epic #475 protects is satisfied by construction.

**The caveats (why GO, not GO-WITH-CAVEATS -- these are knobs, not architecture
gaps):**

1. **The directive renderer is net-new, but trivial.** `RealtimeInstructionsForDirective`
   does not exist yet (only `RealtimeInstructionsForReply` does). It is a small
   string-builder over types that already exist (`conductor.go:63-92`). This is
   the gate refactor #479's first task, not a research risk.
2. **The "implicitly asked me" classifier accuracy is a tuning question.**
   Whether the cached classifier's `intent`/`carriesAction` reliably catches
   implicit room-asks without over-engaging is a precision/recall tune on a
   model that already ships, with a fail-open default that errs toward
   engaging. It is "needs tuning," not "needs new architecture."
3. **Felt decision->first-audio is the one number only #484 settles.** The
   estimate is code-grounded (in-process heuristic cost; the measured ~1000ms
   single-agent saving), but the live number waits on a credentialed room
   (section 7.3). The mitigations are all knobs already in the tree.

**No NO-GO findings.** Nothing surfaced a signal memQL lacks, a place the full
conductor LLM must serialize in front of generation, or a multi-party need that
forces a second stack. The single net-new piece (the directive renderer) is a
small string builder; everything else is reordering work the handler already
does. The verdict is plain GO: the cheap gate is viable and the budget holds.

---

## 9. Acceptance criteria mapping (-> epic #475)

Issue #477 + the rolled-up epic #475 acceptance lines this spike addresses:

- [x] **"What signals make the engage decision cheap + heuristic-first; quantify
  cost (sub-50ms, ideally sub-10ms)."** -> section 3: the ladder
  (direct-address / single-agent / thread / presence / completeness) is
  sub-millisecond, ~3 orders under the 50ms target. Cost table in section 3.6.
- [x] **"Where it risks needing semantics; bounded escalation without a full LLM
  on the critical path."** -> section 4: the "implicitly asked me" case
  escalates to the existing tiny, cached, fail-open `messageClassifier` run in
  **parallel** on the residue only; the full conductor authoring call never
  returns to the path.
- [x] **"How the gate maps to a per-turn directive (mode + brevity) injected
  into response instructions."** -> section 5: the existing
  `CreateResponse(instructions)` seam carries a content-free
  `RealtimeInstructionsForDirective` over the existing `DirectiveMode` /
  `Brevity` types; the model authors the words.
- [x] **"The latency budget: decision -> first audio, gate vs the ~1-1.5s
  authoring round-trip."** -> section 7: ~1.2-1.7s (current) -> ~0.17s + push
  (target, dominated by native model TTFB). Methodology + four trace stamps
  specified; the live number is flagged as #484.
- [x] **"How the same gate serves 1-on-1 (off) and multi-party (traffic-cop) --
  one pipeline, one optional gate."** -> section 6: OFF via the single-agent
  short-circuit for 1-on-1, fast traffic-cop via the same ladder for
  multi-party, one code path, no fork.
- [x] **"VERDICT: is a cheap gate viable + the measured/estimated budget, mapped
  to epic acceptance."** -> section 8 (GO) + this table.
- [~] **Epic acceptance: "the cheap-gate assumption is measured and holds (gate
  cost + decision->first-audio documented for both modes)."** -> design +
  code-grounded estimate complete; the *measured* number is the flagged
  follow-up #484 (the measurement issue at the end of the epic's phased plan).

Legend: [x] satisfied by this design / decision; [~] design complete, live
measurement is the flagged follow-up (#484).

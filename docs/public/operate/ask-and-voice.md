---
title: Ask conversations and voice
audience: public
status: stable
area: operate
sinceVersion: 0.22.9
owner: znas
---

# Ask conversations and voice

Ask is the MemQL OS conversation surface. The assistant is named MemQL.
The floating window and desk widget share the selected conversation, draft,
response and live call. Window-header Ask controls add that window's context.
Conversations are stored on the cluster under their authenticated owner;
private transcripts are never cached in browser local storage.

## Text and workspace actions

An eligible chat route is required. Connect an allowed local model through
Fleet and Cockpit, or configure a federated provider in Settings. Ask follows
MemQL routing policies; adding OpenAI does not silently override a person's
policy choices. Fleet's Model Library and routing views show the available doors.

Ask routes automatically for each step through the available Fleet models and
configured policies. The **Route** menu beside the composer changes that for a
message: **Auto** is the default, **Local** uses local models, **Apps** uses
Claude Code or Codex, and routes you made appear under Yours. A route that is
not ready is shown with its reason and cannot be chosen. Deliberate model or
reasoning overrides remain available on individual runs and steps in Nexus.

Ask checks current capabilities before answering questions about what it can do.
Installed operations and live Fleet model advertisements are separate evidence:
a model that can understand images may not generate them, and an offline or busy
model is not a permanent platform limitation. Machines shared with the signed-in
person are included under the same access rules used for execution. A partial
catalog or failed read does not establish that a capability is absent.

Generated images use the image model route and verified Files storage. Document
formats use the document composer. References such as “the first bird in the
file” require reading that saved source before selecting the subject. Completion
requires a saved-file receipt; Ask exposes its actual file through an Open action.
Model-routing failures offer Fleet; unrelated failures keep diagnostic Details
without directing the person to change model settings.

MemQL discovers named DSL capabilities and executes them with the person's
original authority. Role, app capability, account scope and row authorization
remain in force. Assistant identity never grants owner permissions. Internal
capabilities and credential inputs are excluded from discovery. Every new request opens the same durable work run used by Nexus and the API. The
compiler checks reusable procedures before asking a model; the agent executes
with the same budgets, receipts, and recovery. Runs remain available in Nexus.
Reconnecting observes the run rather than submitting it again. Stop requests
cancellation of the underlying goal and waits for the cluster receipt before
detaching. Model calls observe cancellation through the shared journal; writes
finish at a safe boundary and keep their receipts. A lost connection only
detaches the viewer. Fresh-data questions
still read fresh sources even when the procedure is reused.

## Where work runs

**Where** beside the composer offers Auto, Workbench and Computer. Nexus offers
the same choice, labelled **Run work on**, when starting a goal. The choice
applies to the submitted goal, its background work and child goals; changing
the control affects the next submission. Live voice captures the choice when
the call starts. Choosing the Apps route sets Where to Computer, because apps
run on your own machine; you can change it afterwards. Model selection follows
the Route menu and the configured routing policies.

- **Auto** uses the Workbench when its capabilities fit. If work needs your
  computer, or the Workbench queue is full, MemQL explains the reason and asks
  before using a connected computer.
- **Workbench** keeps shell and filesystem work on the hosted Workbench. If a
  required capability is unavailable, MemQL asks before moving it to your computer.
- **Computer** sends shell and filesystem work to your connected MemQL Cockpit.
  If that destination cannot do the work, MemQL asks before using the Workbench.

Selecting Computer does not grant file or command access. Those permissions
still name the exact operation and machine. An existing saved command permission
does not approve moving a new Auto or Workbench goal onto your computer. Approval
is recorded on the goal's run, expires, and is rechecked on the replica that
executes the operation. Work waiting for capacity starts no process until admitted;
the run's activity shows the wait. Stop cancels queued work too.

## Background work and memory

The same classifier separates quick replies, saved-data lookups, research and
projects. Greetings and answers supported by the current context stay inline.
Longer work is acknowledged after its goal and run are saved, then continues
while you send other messages in the same conversation. Each request keeps its
own captured context. A later message can update work that is already running,
but never silently: Ask shows which goal received it (see
[Messages while work runs](#messages-while-work-runs)).

The classifier writes a brief acknowledgment for the actual request in the same
call that decides its workload. A small link beneath it opens the run in Nexus.
While a slow classification is pending, Ask shows runtime status without
inventing assistant prose. There is no predefined conversational acknowledgment.

Previously generated wording can be offered to the classifier through exact or
semantic lookup. Candidates belong to the same person and captured context,
expire after 24 hours, and become ineligible when the prompt or embedding binding
changes. The original protected compile receipt is checked again. The classifier
must still decide the current intent and workload and adopt or rewrite the
wording; a similar request never replays an answer, tool use or authorization.
This avoids a separate model call just to write an acknowledgment. Cache failure
falls through to fresh wording within a bounded lookup window.

Above the composer, up to three cards show the active tasks and their current
steps. More than three collapse into a count, except a card that is asking
whether to stop its work. Open a card to inspect its run in Nexus, or the count
to see work from that conversation. A card takes a new title when a later
message changes what the task is about. Results appear when the work finishes,
beside a reminder of the original request. Closing Ask or losing the connection
does not discard the work.

For facts missing from the current context, MemQL can discover authorized
concepts and search earlier conversations through named MemQL reads. Results
identify their conversation, speaker and time. Semantic similarity applies
where content is indexed; an empty index is not proof that a fact was never
provided. Retrieved material cannot grant permissions or issue instructions.

The work prompt has stable execution instructions followed by a small current
viewer context: recorded name, job title, platform role and active organization
memberships. Contact details are not included. A membership is not a claim of
employment and does not choose the organization or client for the work.
Profile reads bypass replica-local result caches so changes are visible on the
next request.

`recallMemory` searches private conversation evidence by meaning, then checks
the original source and its content hash before returning it. Source updates
and completed background runs index evidence without holding up the reply.
Older conversations receive bounded lazy backfill; exact phrase search and
pagination remain available when indexing is incomplete or unavailable.
Both user messages and historical assistant responses keep their attribution;
an assistant response is not independent proof of a fact. Corrections and dates
must be compared, and current-world research still requires fresh sources.

Similarity queries reuse the shared embedding cache across replicas. Its
30-day TTL is a backstop; a different model binding changes the cache key
immediately. The cached item is the text vector, not an answer or permission.
Source validity is checked on every recall. Profile checks and memory recall
are recorded in the work journal. This evidence path does not depend on the
separate nightly belief-consolidation scaffold.

Blocking questions and requests for personal computer access pause the run in
Nexus. Computer access names the purpose, access level, eligible machines and
expiry. Recorded decisions survive replica changes; completed tool exchanges
are preserved when work resumes. Time spent waiting for you does not consume
the active execution allowance. Model calls, cost and retry limits still apply.

Compound generated files include a companion source ZIP with their editable
draft, captured inputs and template, assets, exact renderer source, pinned
dependencies and rebuild instructions. The renderer verifies the output hash.
A single Markdown or plain-text file needs no companion. Runnable web products
use Deployables; native desktop, mobile and game distribution remain coming
soon.

Replies default to English without emojis unless explicitly requested.
Recent conversation messages remain verbatim. When context grows, the shared
agent checkpoints older complete exchanges into facts, entities, decisions,
constraints and unfinished work. Checkpoints retain their source messages in
the owner-scoped journal; exact source prefixes reuse the same summary, and the
agent can search original messages with `recallWorkHistory`. A failed checkpoint
never silently discards history. This path applies to both Ask and Nexus.

For a request whose entire outcome is navigation, the shared planner can use
one fast triage call followed by a deterministic destination lookup. App and
section names come from the OS registry; registered record destinations use
the same authorized reads as their lists. Ambiguous records require a choice;
a missing match never becomes an instruction to create data. Other work retains
the shared agent reasoning path.

Execution can open the associated app and highlight the active window and tab. An
implemented control binding, such as adding a group member, receives the same
quiet cue. Cues represent server execution; they never simulate a cursor or
issue a second write from the browser. An app without an exact control binding
still opens and displays its normal live data.

## Messages while work runs

While a conversation has active work, Ask looks at each new message before it
reaches normal intake. A goal is active while it is preparing, working or
waiting. One fast model call reads your message and the active goals, and
chooses what the message does:

- **Updates one goal or several.** You do not name the goal, and one message can
  apply to related goals. "Actually, I'm in Phoenix" changes the weather check
  that is already running instead of starting a second one.
- **Splits.** The part about running work goes to that work. The rest becomes a
  new request, stated on its own, with its own answer or goal.
- **Asks which goal you mean.** The reply is a question and nothing is
  delivered. Your answer settles it, and both messages go to the goal you chose.
- **Offers to stop a goal.** "Never mind the Tucson one" puts a question on that
  goal's card: "Stop this work? It can't be resumed." with Keep and Stop.
  Nothing stops until you press Stop. A live call has no cards, so a clear yes
  to the spoken question confirms.

With no active work, nothing changes and no routing call is made. Messages with
attachments and messages over 4,000 characters are not routed. A message about
none of the active goals is handled as before.

The goal gets your message word for word. The routing call only chooses where it
goes; it never rewrites your words for a goal. How the goal takes the message
depends on what the goal is doing:

| The goal is | Your message | Label |
| --- | --- | --- |
| Preparing or working | Is read at the goal's next step | Queued, then Received |
| Waiting on a timer, a retry, a repair, a re-plan, other work or an outside event | Is read when the goal resumes | Queued, then Received |
| Waiting on a question for you | Becomes the answer | Answered |
| Waiting on a permission | Is a correction. Nothing is approved. | Sent, not approved |
| Paused on its budget | Is saved for when you raise the limit | Saved |
| Waiting on a review that takes no messages | Is not delivered. Open the request to answer it. | Not delivered |
| Ended before reading the message | Is not received | Not received |

Under your message, Ask shows one line for each goal it went to, such as "To
Weather in Phoenix · Received". Received means a successful model call used the
message, not that the goal followed it. MemQL replies in one line, and a split
message shows that line before the new request's own answer. If a delivery
failed, a plain account replaces the line, such as "Sent to Weather in Phoenix.
Hike plan already finished, so it didn't get this." When a message changes what
a goal is about, the goal takes a new title on its card and in Nexus.

If every goal a message was meant for had finished, and nothing else was
decided, the message goes through normal intake. A late "actually, Phoenix"
still gets an answer.

A message never approves a request, grants permission or raises a limit. Those
stay with the request's own decision controls. A goal accepts at most 16 routed
messages, and 16,000 characters in all, per run; a message past that is not
delivered. See [Work context and recovery](../ai/work-context-and-recovery.md)
for how a goal reads guidance.

The routing call runs under your own route. On Local, a local model routes your
message.

## Dictation and live voice

The microphone dictates into the editable composer by default. Ask settings can
choose immediate submission or review, and enable hold-Space dictation in the
floating window. Only one surface can hold the microphone at a time.

Talk with MemQL joins a private LiveKit room and replaces the conversation view
with the MemQL mark, microphone and end-call controls. Male and female voice
choices live in Settings → Ask. The shared conversation remains available when
the call ends. The browser requests echo cancellation and noise suppression,
and Ask checks the active microphone settings. An explicit refusal to enable
echo cancellation closes the microphone with an actionable error.

Microphone activity pauses playback while the cluster transcribes a candidate.
Empty or failed transcription, and multi-word text matching recent playback,
resume the same buffered audio. Confirmed new speech interrupts the reply.
The echo check uses capture time so slow local transcription does not turn
old playback into a new question. Acoustic cancellation remains the primary
protection; test real interruptions and playback with the intended microphone,
speakers and room. Room departure stops the voice
session; accepted work remains in its durable run. Canceled transcription does
not create a fictional user message.

The cluster's agent node runs the voice controller in Go: speech detection,
ASR, the shared MemQL work engine, and sentence-by-sentence TTS.
LiveKit carries WebRTC audio and private execution events. The current pipeline
uses routed ASR/chat/TTS, including OpenAI where configured; it does not use
OpenAI's native Realtime speech-to-speech protocol. Local models and federated
models can be combined independently across these three stages.

Dictation is bounded to two minutes, each live utterance to 30 seconds, and a
live session to 30 minutes. Raw audio is not written into conversation history.
Transcripts and execution evidence are stored under the person's identity.

## Local audio models

Cockpit's declared runtimes advertise `audio_in` and `audio_out`. A model must
also be in `models.allow`; the runtime declaration itself is not permission.
Whisper.cpp can provide local ASR; Kokoro's OpenAI-compatible endpoint can
provide speech. Cockpit supports multipart OpenAI ASR and whisper.cpp `/inference`.
Kokoro voice IDs can be mapped to MemQL's male/female choices in worker policy.
See [Cockpit local-model configuration](https://github.com/znasllc-io/memql-cockpit/blob/main/docs/local-models.md).

The entire call can stay local when Fleet offers ASR, tool-capable chat and TTS.
An eligible local tool-capable chat route enables text Ask; voice additionally needs both audio
routes. Model response times depend on memory pressure, model size and other
calls sharing the machine. The UI does not impose a 60-second response deadline.

## LiveKit deployment

The local k3d overlay includes `deploy/k8s/components/ask-voice`: two LiveKit
replicas and a shared Redis service. Go orchestration stays on agent nodes.
`make secrets` seeds the `memql-voice` secret idempotently, preserving existing
keys. `make up` uses ArgoCD to reconcile the overlay; `make dev` builds local
engine images as usual. No additional k3d host ports are needed.

Signaling uses `wss://voice.<domain>`; TURN uses `turn.<domain>:443` through the
existing TLS front door. The local wildcard certificate covers both names.
TURN is necessary because a browser on the host cannot reach k3d pod IPs.
The component's base media settings are private-network settings, so a cloud
overlay must supply its own reachable RTC/TURN endpoint and TLS configuration
before enabling the component. Local CIDRs must not be copied into a cloud setup.

Agent configuration:

| Setting | Purpose |
| --- | --- |
| `MEMQL_LIVEKIT_URL` | Internal signaling/service endpoint, normally `ws://livekit:7880`. |
| `MEMQL_LIVEKIT_PUBLIC_URL` | Browser-reachable WSS signaling endpoint. Set identically on agent and edge nodes; the edge admits this origin in API-enabled sites' connection policy. |
| `MEMQL_LIVEKIT_API_KEY` | LiveKit server API key from `memql-voice`. |
| `MEMQL_LIVEKIT_API_SECRET` | LiveKit server secret from `memql-voice`. Never sent to the browser. |

The browser receives a two-minute room-join token restricted to its private
room and microphone publication. It cannot publish data or run tools. Only the
MemQL participant's audio and execution events drive the Ask UI.

## Response estimates and Activity

A diminishing ring shows the expected response time for the selected route.
Ask uses recent successful calls for that person and provider/model/modality,
then an initial route estimate when there is no history. The estimate stops
near its end and says Still working when exceeded. It is an expectation, never
a completion percentage or timeout. Switching providers selects that route's
history instead of reusing the previous model's speed.

Activity contains the actual provider attempts, fallback, selected and served
model, policy/rule, execution surface, reuse, latency, token counts and cost
when reported. Estimated usage and unknown pricing remain explicit. Local
inference is identified as local. Text and live-voice evidence survives reopening
a conversation; dictation evidence is visible during the current session, and
router call records remain available in Fleet's call history.

The call that routes a message appears in Activity as Routing message. If
routing is unavailable, or its answer is invalid twice, the message goes through
normal intake and Activity shows that entry as a fallback.

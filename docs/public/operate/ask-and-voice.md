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

MemQL discovers named DSL capabilities and executes them with the person's
original authority. Role, app capability, account scope and row authorization
remain in force. Assistant identity never grants owner permissions. Internal
capabilities and credential inputs are excluded from discovery. Every turn opens the same durable work run used by Nexus and the API. The
compiler checks reusable procedures before asking a model; the agent executes
with the same budgets, receipts, and recovery. Runs remain available in Nexus.
Reconnecting observes the run rather than submitting it again. Stop requests
cancellation of the underlying goal and waits for the cluster receipt before
detaching. Model calls observe cancellation through the shared journal; writes
finish at a safe boundary and keep their receipts. A lost connection only
detaches the viewer. Fresh-data questions
still read fresh sources even when the procedure is reused.

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

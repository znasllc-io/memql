# memql-sdk-ts

The TypeScript counterpart to [`memql/sdk/go`](../go/). One logical
surface, two language implementations: every operation here mirrors
the Go SDK's verbs and shapes so a feature shipped once carries
across every memQL client.

**Status: spec only.** No code in this package yet -- the contract
below is the agreed shape; implementation lands in a follow-up once
the spec is reviewed and a first consumer (CoPresent web frontend)
has integrated against it locally.

---

## Scope

The SDK is the canonical client surface for memQL. It speaks the
gRPC contract over the `/memql/ws` WebSocket bridge (browsers can't
do raw gRPC). Internally organized by concern:

| Sub-module | Purpose |
|---|---|
| `client/` | Connection, dispatcher, queries, subscriptions -- the wire-level baseline. Mirrors `sdk/go/client/`. |
| `voice/` | Voice helpers: push-to-talk transcription, room-voice (LiveKit) session helpers. Mirrors `sdk/go/voice/`. |
| `sense/` | MemQL Sense: tokenize / diagnose / complete / hover / signatureHelp over `.memql` source. Mirrors `sdk/go/sense/`. |
| `chat/` | Chat state machine: subscribe to `v1:cognition:utterance`, send utterances, observe assistant replies. |
| `computer-use/` | Worker pairing, scope-elevation card subscription, kill switch. (Future.) |
| `identity/` | Magic-link auth, token rotation, partition access. (Future.) |

Every consumer (CoPresent web app, future thin clients) goes through
this package. No bespoke wire wrappers in the consumer.

---

## Distribution

In-repo (Go monorepo) for now. The TS source lives at
`memql/sdk/ts/`. CoPresent is the only initial consumer and imports
it as a workspace path. Versioning + npm publishing happens once a
second TS consumer needs it.

---

## Naming + grammar

The TS package mirrors the Go SDK 1:1:

| Go | TS |
|---|---|
| `client.Connection` | `Connection` |
| `client.Dispatcher` | `Dispatcher` |
| `client.QueryClient` | `QueryClient` |
| `client.SubscriptionManager` | `SubscriptionManager` |
| `voice.PushToTalk(...)` | `pushToTalk(...)` |
| `voice.PartialTranscript` | `PartialTranscript` |
| `voice.FinalTranscript` | `FinalTranscript` |
| `voice.Options` | `PushToTalkOptions` |

Idioms diverge where the host language demands it: Go's `io.Reader`
becomes a browser `ReadableStream<Uint8Array>`; Go's `chan` becomes
an `AsyncIterable`; Go's `context.Context` becomes an `AbortSignal`.
Logical verbs do not diverge.

---

## Wire transport

```ts
import { Connection } from "@memql/sdk";

const conn = await Connection.dial({
  endpoint: "wss://staging.memql/memql/ws",
  auth: { bearer: jwt },
  partition: "acme",
});

// conn.dispatcher : Dispatcher
// conn.query      : QueryClient
// conn.subscribe  : SubscriptionManager
// conn.done()     : Promise<void>           (resolves on stream close)
// conn.close()    : void
```

- Endpoint is the `/memql/ws` bridge URL (browser) or a raw gRPC URL
  in the future Node-native build.
- `auth.bearer` is a JWT issued by the identity service. Guest flows
  use `auth.guestToken`; worker flows use `auth.workerToken`.
- `partition` auto-stamps every outbound envelope, same as the Go
  SDK's `Dispatcher.SetPartition`.

`QueryClient` exposes `execute(query: string)`,
`listConcepts()`, `getMyAccess()`. `SubscriptionManager` exposes
`subscribe(pattern, handler)` returning an `unsubscribe` function.
The pattern grammar matches the server side:
`graph.node.created.*.v1:cognition:utterance`, etc.

---

## Voice: push-to-talk

```ts
import { pushToTalk, PartialTranscript, FinalTranscript } from "@memql/sdk/voice";

const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
const recorder = new MediaRecorder(mic, { mimeType: "audio/webm;codecs=opus" });
const audio = recorderToReadableStream(recorder); // helper TBD

const result: FinalTranscript = await pushToTalk(conn.dispatcher, audio, {
  audio: { encoding: "opus", sampleRate: 48000, channels: 1 },
  language: "en",
  onPartial: (p: PartialTranscript) => {
    // render p.text in the composer as the user holds the PTT button
  },
  signal: abortController.signal, // cancel mid-stream
});
// result.text is the final transcript -- caller writes the utterance row.
```

Same contract as Go: SDK owns the transcription protocol, caller
owns the audio source. The Web side uses `ReadableStream<Uint8Array>`
in place of Go's `io.Reader`; helpers like
`recorderToReadableStream` will live in `sdk/ts/voice/audio.ts` to
bridge browser audio APIs to the SDK input format.

Concurrent `pushToTalk` calls work the same way as in Go: each
session gets its own `request_id`, registered with the dispatcher's
stream-routing layer.

---

## Chat (future, sketch only)

```ts
const chat = conn.chat(spaceId);

const sub = chat.onUtterance((u) => {
  // u: { id, speakerId, speakerKind, text, citations, createdAt }
});

await chat.send("hello"); // wraps mutationSendTextUtterance

// when done:
sub.unsubscribe();
```

`chat.send` writes a `v1:cognition:utterance` row; cognition routes
to the owner's assistant; the reply lands as another utterance that
the existing subscription picks up. The SDK exposes the verbs --
it does not embed a UI.

---

## Computer-use (future, sketch only)

```ts
// In-app surfaces (CoPresent's PlanScopeElevationCard):
const elevation = conn.computerUse.onScopeElevationRequest((req) => {
  // render approval card; call req.allow({...}) or req.deny() on user action
});

// Kill switch:
await conn.computerUse.setKillSwitch(false);
```

Worker pairing itself runs through `identity/` (magic-link flow);
the computer-use module wraps the runtime coordination.

---

## What's NOT in scope

- Rendering. The SDK exposes data and verbs. UI is the consumer's
  job.
- Audio device selection / volume meters / level monitoring -- all
  consumer-side.
- LiveKit integration. Browser LiveKit goes through the LiveKit SDK
  directly; the memQL TS SDK only exposes the room-join token-mint
  helper.

---

## Open questions

1. **Build target**: ESM only, or dual ESM + CJS? Default to ESM until
   a CJS consumer shows up.
2. **proto-gen-ts**: which generator (`buf` + `protobuf-ts`, or
   `ts-proto`)? Pick at first-code time; whichever produces the
   leaner generated surface for the oneof messages.
3. **Auth refresh**: Go SDK has `Dispatcher.RotateAuth`. TS should
   either match (explicit method) or wrap the token-refresh inside
   the SDK with an `onTokenExpired` hook. Pick when implementing.
4. **Streaming abstraction**: `AsyncIterable` over delta events, or
   callback-based `onPartial`? Go uses callbacks; web idiom is
   `for await ... of`. Probably both: `onPartial` for the common
   case, optional `iter()` for advanced consumers.

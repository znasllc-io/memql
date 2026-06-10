---
title: Participant Video + Vision: Backend & Frontend Specs
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Participant Video + Vision: Backend & Frontend Specs

**Status:** Living coordination document.
**Owners:** CoPresent team (frontend) + memQL Polyphon team (backend).
**Last updated:** 2026-04-18.

## Context

CoPresent + Polyphon already run LiveKit end-to-end for **audio**:
human voice + AI agent voice are routed through a per-Space LiveKit
room provisioned by `PolyphonRoomTokenMsg` and bridged to the active
voice provider (Deepgram by default, OpenAI as fallback) by the
Bridge Agent.

This branch extends that pipeline to carry **video** as well, so:

1. Humans can publish their **camera** to the LiveKit room and see
   each other's video.
2. The Presence panel reactively re-lays out around the **active
   speaker** (LiveKit's `activeSpeakers` event drives the highlight).
3. The Bridge Agent **subscribes to remote video tracks** in the
   room and forwards frames to a vision sink — initial sink is a
   no-op logger; the real vision microservice plugs in later.
4. Polish: capacity badge ("3 / 5 humans"), disabled-reason tooltip
   on the invite button, "Invite external (coming soon)" placeholder.

Frontend work: `feat/participant-video` on the CoPresent repo.
Backend work: `feat/participant-video-backend` on the memQL repo
(short-lived feature branch off `main`).

## What already exists (do not rebuild)

| Concern                          | Where                                                                  |
|----------------------------------|------------------------------------------------------------------------|
| LiveKit Server (local dev)       | `memQL/docker/docker-compose.polyphon.yml` — `livekit:7880`            |
| LiveKit Server (lab/prod)        | `memQL/infra/polyphon/k8s/livekit/deployment.yaml` — k8s LoadBalancer  |
| Token issuance (gRPC)            | `PolyphonRoomTokenMsg` → `polyphon_handlers.go:handlePolyphonRoomToken` |
| Token issuance (HTTP)            | `POST /polyphon/room-token` → returns `{token, roomName, livekitUrl, expiresAt}` |
| Token signing (Go)               | `component/polyphon/localroom.go:LocalRoomProvider.GenerateToken`       |
| Frontend room hook (audio)       | `src/hooks/usePolyphonRoom.ts`                                         |
| Frontend device enum             | `src/hooks/useMediaDevices.ts`                                         |
| Interaction-mode hook            | `src/hooks/useInteractionMode.ts` (already has a `voice-video` mode)   |
| Bridge Agent (audio bridge)      | `component/polyphon/bridge/bridge.go`                                  |
| Polyphon types (frontend)        | `src/types/polyphon.ts`                                                |
| Active-speaker event             | LiveKit `RoomEvent.ActiveSpeakersChanged` (already wired in hook)      |

The implication: this feature is **mostly an extension of the existing
Polyphon stack**, not a greenfield build. No new LiveKit deployment,
no new token mutation, no new room concept.

## Architectural decisions (locked)

1. **Reuse the Polyphon room.** One LiveKit room per Space carries
   both audio and video. No separate "video room" concept.
2. **Video publishing extends `usePolyphonRoom`.** A new
   `videoStream?: MediaStream | null` option is added to
   `UsePolyphonRoomOptions`; when set, the hook publishes the first
   video track. Audio behavior is unchanged.
3. **Remote video tracks land in `PolyphonHuman.videoTrack` and
   `PolyphonAgent.videoTrack`.** The hook tracks
   `videoTrack: MediaStreamTrack | null` per remote participant and
   exposes them through the existing arrays. No global store; the
   hook is the source of truth.
4. **Self-tile is the local participant's published video track.**
   No separate preview branch. We attach the local
   `LocalVideoTrack`'s `MediaStreamTrack` into a `<ParticipantVideo>`
   the same way as remotes.
5. **Camera lifecycle is owned by a new `useCameraTrack` hook.**
   Single getUserMedia call, single producer of the camera
   MediaStream. UI components subscribe; the room hook receives the
   stream and publishes it. Stopping the track is a single
   responsibility (the hook on cleanup).
6. **Active-speaker = LiveKit's `ActiveSpeakersChanged` event.**
   Already wired in `usePolyphonRoom`; this branch reuses it as the
   source of truth for both layouts. The existing
   `presenceState === 'responding'` signal continues to drive the SI
   "thinking" pill but is **not** used for the video spotlight.
7. **No new wire envelopes.** `PolyphonRoomTokenMsg` already returns
   what we need. Vision frames stay server-internal (Bridge Agent →
   vision sink in-process); they do not cross the gRPC boundary in
   this branch.
8. **Vision sink is an interface, not a service.** The Bridge Agent
   gets a `VideoFrameSink` interface; default impl is a logger
   (counts frames per participant per minute). When the real vision
   microservice ships, it implements this interface. No protocol
   surface in this branch.
9. **No backwards compatibility.** Pre-release; both sides update
   atomically. No flag gates.
10. **Deployment unchanged.** LiveKit already runs in dev
    (docker-compose.polyphon.yml) and lab/prod (k8s). No Terraform,
    no new GCE VM, no new docker-compose file. If a standalone GCE
    deploy is needed later, it ships in its own branch.

## What this branch delivers

### Backend (`feat/participant-video-backend` in memQL worktree)

- `component/polyphon/bridge/video_sink.go` — new `VideoFrameSink`
  interface + a `LoggingVideoFrameSink` default that counts frames
  and logs every N seconds.
- `component/polyphon/bridge/bridge.go` — extend the room
  subscription handler to also subscribe to remote video tracks and
  hand frames to the configured `VideoFrameSink`. Add
  `WithVideoFrameSink(sink)` option to `New()`.
- `component/polyphon/bridge/video_sink_test.go` — sanity tests.
- `docs/internal/planning/participant-video-specs.md` — this document.

No edits to `memql.proto`, `component/grpc/`, or any other file
outside the polyphon-bridge surface for this phase.

### Frontend (`feat/participant-video` in copresent)

**Schema:**
- `src/types/polyphon.ts` — add `videoTrack: MediaStreamTrack | null`
  to `PolyphonAgent` and `PolyphonHuman`; add `videoStream` and
  `cameraEnabled` options to `UsePolyphonRoomOptions`; expose
  `localVideoTrack` on the result.
- `src/types/copresent.ts` — populate `Session.streams.videoTrackId`
  with the LiveKit publication SID; add
  `Session.humanInput.videoInputDeviceId` and
  `Session.humanInput.audioInputDeviceId`.

**New hooks:**
- `src/hooks/useCameraTrack.ts` — getUserMedia({video, audio}) with
  device-id selection, permission state, error surface, and clean
  teardown on unmount.

**Modified hooks:**
- `src/hooks/usePolyphonRoom.ts` — accept `videoStream`, publish the
  local video track, subscribe to remote video tracks, expose them
  on `agents[i].videoTrack` / `humans[i].videoTrack` and as
  `localVideoTrack`.

**New components:**
- `src/components/presence/MediaControls.tsx` — mic/cam toggle pair
  + device picker dropdowns (driven by `useMediaDevices`). Lives in
  the `PresencePanel` footer.
- `src/components/presence/SpotlightLayout.tsx` — single large tile
  for the active speaker + horizontal filmstrip below. Default on
  small viewports.
- `src/components/presence/GalleryLayout.tsx` — refactored from
  `ParticipantStack`. Default on desktop.
- `src/components/presence/LayoutModeToggle.tsx` — header toggle
  with localStorage persistence.
- `src/components/presence/CapacityBadge.tsx` — "3 / 5 humans" pill
  + tooltip explaining why the invite button is disabled when full.

**Modified components:**
- `src/components/presence/PresencePanel.tsx` — host
  `MediaControls` (footer), `LayoutModeToggle` + `CapacityBadge`
  (header), self-tile rendering, render via
  `LayoutMode === 'spotlight' ? SpotlightLayout : GalleryLayout`.
  Wire the disabled "Invite external (coming soon)" menu item.
- `src/components/presence/ParticipantCard.tsx` — accept an optional
  `MediaStream` from the Polyphon room (built from
  `agent.videoTrack` / `human.videoTrack`) so existing
  `<ParticipantVideo>` rendering picks it up unchanged.
- `src/components/presence/ParticipantStack.tsx` — becomes a thin
  re-export from `GalleryLayout` for the same PR; no compat shim
  long-term — fully removed once the layouts are exercised by tests.

**Tests:**
- `src/hooks/__tests__/useCameraTrack.test.ts` — vitest with mocked
  `navigator.mediaDevices`.
- `src/hooks/__tests__/usePolyphonRoom.video.test.ts` — vitest with
  the LiveKit `Room` mocked; covers publish on attach, unpublish on
  detach, remote-track wiring.

### Optional follow-up (not in this branch)

- Standalone Terraform/GCE module for a non-Polyphon LiveKit if the
  product ever needs an isolated room cluster. The existing k8s
  deploy is sufficient for everything described here.
- gRPC service for the vision microservice (when it ships, it
  implements `VideoFrameSink`).
- External-invite (Zoom-style) flow — Polyphon already supports
  anonymous identities via `participantId = "guest-{uuid}"`; this
  branch ships only the disabled UI affordance.

## Environment variables

No new variables. The Polyphon Bridge Agent already reads
`POLYPHON_LIVEKIT_URL`, `POLYPHON_LIVEKIT_API_KEY`,
`POLYPHON_LIVEKIT_API_SECRET` from `.env.local`. The frontend never
sees these — it gets the URL and a per-session token from
`/polyphon/room-token`.

If a developer wants to run the stack:

```bash
make dev-cluster-up
```

This brings up the 2-replica parity cluster (Postgres, the memQL mesh
nodes, identity, the copresent SPA, and LiveKit) behind the nginx TLS
front door. The voice/avatar overlay (`docker-compose.polyphon.yml`) is
pending re-home onto the cluster (memql#1310).

## Open coordination points

- **Mobile refactor (`temp/feature-mobile-responsive.md`).** The
  Presence panel ends up inside a bottom sheet on mobile. The video
  feature ships a `LayoutModeToggle` that defaults to spotlight on
  small viewports (Tailwind `md:` breakpoint until the mobile
  refactor introduces a canonical mobile flag).
- **Polyphon enablement.** Today, Polyphon mode is gated by the
  `voicePlatform` setting (`'standard' | 'polyphon'`). The video UI
  in this branch is gated by the same flag — humans only see the
  cam toggle when the Space is on Polyphon. (This matches the
  existing voice-mode behavior.) When `voicePlatform === 'standard'`,
  the cam toggle is hidden and the existing 1:1 audio path is
  unchanged.
- **Vision microservice.** Out of scope. Bridge Agent ships with
  `LoggingVideoFrameSink`. The next branch (whoever owns vision)
  swaps in a real sink.
- **AI agent video.** Bridge Agent does **not** publish a video
  track in this branch. AI agents appear with their existing avatar
  fallback (`AIBlobAvatar` or LiveAvatar). When the avatar pipeline
  publishes its rendered frames into the LiveKit room, that flows
  through the same `<ParticipantVideo>` path and "just works".

## Implementation order

1. Backend: `VideoFrameSink` interface + `LoggingVideoFrameSink` +
   Bridge Agent video subscription. Compile and unit-test.
2. Frontend: `useCameraTrack` hook + tests.
3. Frontend: extend `usePolyphonRoom` for video publish/subscribe + tests.
4. Frontend: `MediaControls`, `CapacityBadge`, `LayoutModeToggle`.
5. Frontend: `SpotlightLayout`, `GalleryLayout`. Wire into
   `PresencePanel`.
6. Frontend: invite polish + external-invite placeholder + edge cases.
7. Verification: `npm run check` + tests pass on both sides.

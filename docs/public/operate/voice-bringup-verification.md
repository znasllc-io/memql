---
title: Voice-agent bring-up verification
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Voice-agent bring-up verification

Operator runbook for the voice-agent's "is it actually working end-
to-end on this box" check. Pair with
[docs/public/operate/auth/voice-agent-jwt.md](auth/voice-agent-jwt.md) for the
provisioning flow and [docs/public/operate/voice-eou-tuning.md](voice-eou-tuning.md) for
end-of-utterance tuning.

## When to run this

- After bringing up the local k3d cluster (`make up`) to confirm the
  `voice` pod is running with a valid `VOICE_AGENT_TOKEN`.
- After editing anything in `integrations/voice/agent/` or the
  LiveKit transport.
- After a deploy to staging / prod, before declaring voice
  available.

> The voice/media plane is environment-selectable (Epic #2184): the local
> k3d cluster runs the audio path against a **LiveKit Cloud** project
> (outbound; Cloud's TURN handles NAT), while staging/prod self-host LiveKit
> (`deploy/k8s/base/livekit.yaml`). There is no self-hosted livekit-server /
> livekit/sip / ngrok TURN in the local dev loop. Avatar VIDEO is validated
> on staging.

## The smoke check (manual)

The fast "is the agent alive + authenticated" check against the local
k3d cluster:

```bash
# 1. Confirm the voice pod is Running (not CrashLoopBackOff -- the
#    symptom of a missing VOICE_AGENT_TOKEN).
kubectl get pods -n memql -l app=voice

# 2. Confirm VOICE_AGENT_TOKEN is populated inside the pod.
kubectl exec -n memql deploy/voice -- sh -c 'test -n "$VOICE_AGENT_TOKEN" && echo OK'

# 3. Scan the recent log tail for auth failures or gRPC
#    UNAUTHENTICATED responses.
kubectl logs -n memql deploy/voice --tail 200
```

If step 2 fails, the seeded `VOICE_AGENT_TOKEN` Secret didn't land --
re-seed with `make secrets` (see the failure-modes table below).

## The env contract

The voice-agent reads the following at startup
(`integrations/voice/agent/config.go`, `LoadConfig`). In the local k3d
cluster each value comes from the manifests in `deploy/k8s/base` /
`deploy/k8s/overlays/local` plus the seeded `livekit-secrets` +
`memql-secrets` Secrets:

| Var | Source in the local cluster | Required? |
| --- | --- | --- |
| `LIVEKIT_URL` | `wss://<project>.livekit.cloud` from `livekit-secrets` (seed-secrets.sh sources it from your `LIVEKIT_URL` env, #2186) | yes |
| `LIVEKIT_API_KEY` | the LiveKit Cloud project key, from `livekit-secrets` | yes |
| `LIVEKIT_API_SECRET` | the LiveKit Cloud project secret, from `livekit-secrets` | yes |
| `MEMQL_GRPC_ADDR` | `bff:50051` (cluster DNS) | yes |
| `MEMQL_OPENAI_API_KEY` | `memql-secrets` Secret (seeded from the genesis envelope) | yes |
| `VOICE_AGENT_TOKEN` | `memql-secrets` Secret, seeded by `make up` / `make secrets` | yes |
| `MEMQL_AVATAR_VENDOR` | `anam` default; overridable in the overlay | no |
| `MEMQL_ANAM_API_KEY` | `memql-secrets` Secret | required when `MEMQL_AVATAR_VENDOR=anam` |
| `MEMQL_SIMLI_API_KEY` | `memql-secrets` Secret | required when `MEMQL_AVATAR_VENDOR=simli` |

Set `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` to your LiveKit
Cloud project before `make up`; without them the voice pod stays degraded
(LiveKit not configured). Avatar VIDEO is validated on staging. Staging/prod
resolve these to the self-hosted `livekit-server` instead.

## The full round-trip (manual)

The smoke check confirms the agent is healthy + authenticated.
End-to-end voice quality and latency still need a human in the
loop:

1. Open the frontend SPA (built and deployed from its own repo -- the
   engine's local overlay does not include it; any client that drives
   the `PolyphonRoomTokenMsg` room-join path works).
2. Create or join a space; the BFF's `PolyphonRoomTokenMsg`
   handler dispatches the voice-agent into the room as the
   General Assistant participant.
3. Speak. Watch the voice-agent logs:
   ```bash
   kubectl logs -n memql deploy/voice -f
   ```
   You should see:
   - `voice agent partial` lines (interim transcripts)
   - `voice agent final` line (final transcript)
   - `voice agent turn request` line (memql cognition dispatched)
   - TTS playback in the browser
   - Avatar lip-sync only applies on staging (audio-only locally)
4. If anything goes silent, the next places to look are:
   - `kubectl logs -n memql deploy/cognition` -- routing decision +
     agent dispatch.
   - `kubectl logs -n memql deploy/bff` -- the room token grant.
   - The **LiveKit Cloud** project dashboard -- room join / publish (there is
     no local `livekit` pod on the Cloud dev plane; staging/prod use
     `kubectl logs -n memql deploy/livekit`).

## Common failure modes

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `voice` pod CrashLoopBackOff | `VOICE_AGENT_TOKEN` empty | re-seed with `make secrets` then `kubectl rollout restart -n memql deploy/voice`, or run [the manual mint-and-recreate](auth/voice-agent-jwt.md#bring-up-injection-dev--prod) |
| Auth works but no TTS | OpenAI key missing | seal the key into the genesis envelope, then `make secrets` |
| `UNAUTHENTICATED` in voice-agent logs | Token expired or identity row soft-deleted | re-mint with `make voice-agent-token INSTANCE=voice-agent-local`, re-seed, and roll the voice Deployment |
| `voice agent turn request` lands but cognition doesn't reply | Cognition node down or routing broken | `kubectl logs -n memql deploy/cognition`; `kubectl rollout restart -n memql deploy/cognition` |

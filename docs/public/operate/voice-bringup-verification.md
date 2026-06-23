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

> Avatar VIDEO is staging-only. The local k3d cluster runs the audio
> path against the in-cluster LiveKit Deployment
> (`deploy/k8s/base/livekit.yaml`); there is no ngrok TURN relay or
> public avatar URL locally. Verify the avatar on staging.

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
re-seed with `make k3d-secrets` (see the failure-modes table below).

## The env contract

The voice-agent reads the following at startup
(`integrations/voice/agent/config.go`, `LoadConfig`). In the local k3d
cluster each value comes from the manifests in `deploy/k8s/base` /
`deploy/k8s/overlays/local` plus the seeded `memql-secrets` Secret:

| Var | Source in the local cluster | Required? |
| --- | --- | --- |
| `LIVEKIT_URL` | `ws://livekit:7880` (the in-cluster LiveKit Service) | yes |
| `LIVEKIT_API_KEY` | `devkey` from the local overlay | yes |
| `LIVEKIT_API_SECRET` | `secret` from the local overlay | yes |
| `MEMQL_GRPC_ADDR` | `bff:50051` (cluster DNS) | yes |
| `MEMQL_OPENAI_API_KEY` | `memql-secrets` Secret (seeded from the genesis envelope) | yes |
| `VOICE_AGENT_TOKEN` | `memql-secrets` Secret, seeded by `make up` / `make k3d-secrets` | yes |
| `MEMQL_AVATAR_VENDOR` | `anam` default; overridable in the overlay | no |
| `MEMQL_ANAM_API_KEY` | `memql-secrets` Secret | required when `MEMQL_AVATAR_VENDOR=anam` |
| `MEMQL_SIMLI_API_KEY` | `memql-secrets` Secret | required when `MEMQL_AVATAR_VENDOR=simli` |

The avatar VIDEO path (which needs a publicly reachable LiveKit URL /
TURN relay) is not wired locally -- audio works without it. Verify the
avatar on staging.

## The full round-trip (manual)

The smoke check confirms the agent is healthy + authenticated.
End-to-end voice quality and latency still need a human in the
loop:

1. Open CoPresent (via the copresent SPA port-forward,
   `kubectl port-forward -n memql svc/copresent 8080:8080`).
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
   - `kubectl logs -n memql deploy/livekit` -- room join / publish.

## Common failure modes

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `voice` pod CrashLoopBackOff | `VOICE_AGENT_TOKEN` empty | re-seed with `make k3d-secrets` then `kubectl rollout restart -n memql deploy/voice`, or run [the manual mint-and-recreate](auth/voice-agent-jwt.md#bring-up-injection-dev--prod) |
| Auth works but no TTS | OpenAI key missing | seal the key into the genesis envelope, then `make k3d-secrets` |
| `UNAUTHENTICATED` in voice-agent logs | Token expired or identity row soft-deleted | re-mint with `make voice-agent-token INSTANCE=voice-agent-local`, re-seed, and roll the voice Deployment |
| `voice agent turn request` lands but cognition doesn't reply | Cognition node down or routing broken | `kubectl logs -n memql deploy/cognition`; `kubectl rollout restart -n memql deploy/cognition` |

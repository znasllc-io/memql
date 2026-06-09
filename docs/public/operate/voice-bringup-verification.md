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

- Right after `make dev-refresh` to confirm the freshly-minted
  `VOICE_AGENT_TOKEN` actually landed in `polyphon-voice-agent`.
- After editing anything in `integrations/voice/agent/` or the
  LiveKit transport.
- After a deploy to staging / prod, before declaring voice
  available.

## The smoke check (manual)

The fast "is the agent alive + authenticated" check:

```bash
# 1. Confirm the container is running (not restarting -- the symptom
#    of a missing VOICE_AGENT_TOKEN).
docker ps --filter name=polyphon-voice-agent

# 2. Confirm VOICE_AGENT_TOKEN is populated inside the container.
docker exec polyphon-voice-agent sh -c 'test -n "$VOICE_AGENT_TOKEN" && echo OK'

# 3. Scan the recent log tail for auth failures or gRPC
#    UNAUTHENTICATED responses.
docker logs --tail 200 polyphon-voice-agent
```

If step 2 fails, the mint-and-inject step in dev-refresh didn't land
-- see the recovery in the failure-modes table below.

## The dev env contract

The voice-agent reads the following at startup
(`integrations/voice/agent/config.go`, `LoadConfig`). Each one's
source is locked in dev:

| Var | Source in dev | Required? |
| --- | --- | --- |
| `LIVEKIT_URL` | Hardcoded `ws://livekit:7880` in `docker-compose.polyphon.yml` (service-to-service) | yes |
| `LIVEKIT_API_KEY` | Hardcoded `devkey` in compose | yes |
| `LIVEKIT_API_SECRET` | Hardcoded `secret` in compose | yes |
| `MEMQL_GRPC_ADDR` | Hardcoded `bff:50051` in compose | yes |
| `MEMQL_DEEPGRAM_API_KEY` | `.env.local` via `env_file:` (genesis-sealed in `genesis.znas`) | yes |
| `VOICE_AGENT_TOKEN` | **Shell env at compose-up time** -- minted by `scripts/dev/refresh.sh` step 4 (see [#184](https://github.com/znasllc-io/memql/issues/184)) | yes |
| `MEMQL_AVATAR_VENDOR` | Compose default `anam`; overridable via shell env | no |
| `ANAM_API_KEY` | `.env.local` via `env_file:` | required when `MEMQL_AVATAR_VENDOR=anam` |
| `SIMLI_API_KEY` | `.env.local` via `env_file:` | required when `MEMQL_AVATAR_VENDOR=simli` |
| `LIVEKIT_PUBLIC_URL` | `.env.local`, rewritten by `lib_refresh_ngrok` to a fresh ngrok tunnel | required for the avatar; audio-only works without it |

If the token check (step 2 above) fails, the mint-and-inject step in
dev-refresh didn't land. Recover with the manual mint-and-recreate in
the failure-modes table below.

If the avatar fails to render but audio works, `LIVEKIT_PUBLIC_URL`
is the usual cause -- check `ngrok` is installed and authed
(`scripts/dev/install-deps.sh` surfaces a hint).

## The full round-trip (manual)

The smoke check confirms the agent is healthy + authenticated.
End-to-end voice quality and latency still need a human in the
loop:

1. Open CoPresent (`https://app.local.znas.io` after dev-refresh).
2. Create or join a space; the BFF's `PolyphonRoomTokenMsg`
   handler dispatches the voice-agent into the room as the
   General Assistant participant.
3. Speak. Watch the voice-agent logs:
   ```bash
   docker logs -f polyphon-voice-agent
   ```
   You should see:
   - `voice agent partial` lines (Deepgram interim transcripts)
   - `voice agent final` line (Deepgram final transcript)
   - `voice agent turn request` line (memql cognition dispatched)
   - TTS playback in the browser (Aura-2)
   - Avatar lip-sync (Anam, if `LIVEKIT_PUBLIC_URL` is reachable)
4. If anything goes silent, the next places to look are:
   - `docker logs memql-cognition` -- routing decision + agent
     dispatch.
   - `docker logs memql-bff` -- the room token grant.
   - `docker logs polyphon-livekit` -- room join / publish.

## Common failure modes

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `polyphon-voice-agent` restarting forever | `VOICE_AGENT_TOKEN` empty | re-run `make dev-refresh`, or run [the manual mint-and-recreate](auth/voice-agent-jwt.md#bring-up-injection-dev--prod) |
| Auth works but no TTS | Deepgram key missing in `.env.local` | seal the key into `~/.memql/genesis.znas` via `memql-cockpit genesis init` |
| Audio works but no avatar | `ngrok` missing or `LIVEKIT_PUBLIC_URL` stale | install ngrok (`make install-deps` surfaces the hint), re-run `make dev-refresh` |
| `UNAUTHENTICATED` in voice-agent logs | Token expired or identity row soft-deleted | re-mint with `make voice-agent-token INSTANCE=voice-agent-local` and recreate the service |
| `voice agent turn request` lands but cognition doesn't reply | Cognition node down or routing broken | `docker logs memql-cognition`; bounce the cognition node |

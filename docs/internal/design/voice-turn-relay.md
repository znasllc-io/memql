---
title: Anam avatar TURN media relay (local dev)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Anam avatar TURN media relay (local dev)

> **Determination (memql#1277): local cloud-avatar VIDEO does not render over
> this relay, and cannot on free tunneling.** This relay reliably carries
> LiveKit **signaling** and **browser <-> LiveKit** media, and the TURN path
> passes a `turnutils_uclient` round-trip. But the **cloud-engine <-> LiveKit
> media leg never connects**, so the Anam/Simli direct avatar shows no video
> locally. Proven from live dev-cluster logs (see "Why cloud-avatar video can't
> render locally" below). The cloud engine joins LiveKit signaling fine, then
> ICE fails because both sides only offer **unreachable host candidates** and
> the cloud engine forms **no relay candidate** from the advertised TURN
> server. This is **not vendor-specific** (Anam and Simli fail identically) and
> **not a timeout-tuning issue** (memql#1274 ruled that out). **Supported local
> path: direct avatar audio-only. Cloud-avatar VIDEO is validated on STAGING
> (memql#784)**, where LiveKit is directly reachable and advertises real host
> candidates. The relay below remains useful for the browser media path and as
> the signaling tunnel.

The Anam direct/Guide avatar needs Anam's **cloud** engine to exchange
WebRTC media with the **local** dev LiveKit. The browser is on the same
host and reaches LiveKit fine, but Anam's cloud cannot reach the host's
loopback, and ngrok only tunnels **signaling** -- not UDP media. This
doc describes the standalone-coturn relay that bridges that gap and how
`make dev-refresh` stands it up automatically (memql#764 / #770).

## The path

```
Anam cloud engine
   |  turn:<ngrok-tcp-host>:<port>?transport=tcp
   v
ngrok (RAW TCP passthrough)
   |
   v
coturn  (memql-coturn, docker/coturn/turnserver.conf, :3478)
   |  relays over the docker network
   v
polyphon-livekit  (rtc.node_ip = its docker-bridge IP)
```

LiveKit advertises the external coturn to every participant via
`rtc.turn_servers` in `docker/livekit/livekit-dev.yaml`; Anam picks it
up in its join response and relays through it.

## What dev-refresh does

A single `make dev-refresh` brings the whole relay up, in two steps:

**`lib_refresh_ngrok`** (before compose-up) starts **one** ngrok agent
running **both** endpoints via `ngrok start --all` over a generated
config merged on top of the user's global `ngrok.yml` (for the
authtoken):
- `livekit` (https) -> `localhost:7880` -- LiveKit signaling; stamped
  into `LIVEKIT_PUBLIC_URL` / `POLYPHON_LIVEKIT_PUBLIC_URL`.
- `coturn` (tcp) -> `localhost:3478` -- the TURN relay.

One agent (not two) because ngrok v3 binds a single local API on
`:4040`; two agents collide there. The generated config uses v3
`endpoints:` syntax, where the TCP protocol comes from the endpoint
`url: tcp://` (an ephemeral addr, or `tcp://<MEMQL_NGROK_TURN_ADDR>`
when a reserved one is set) -- there is no `protocol:` field.

**`lib_refresh_turn_relay`** (`step4c` in `refresh.sh`, after the stack
is up) then:
1. Reads `polyphon-livekit`'s docker-bridge IP -> stamps `rtc.node_ip`.
2. Reads the `coturn` tcp tunnel's `host:port` from the same ngrok
   `:4040` API -> stamps `rtc.turn_servers[0]`.
3. Restarts `polyphon-livekit` so it reloads the stamped config.

`coturn` itself is the `memql-coturn` service in
`docker/docker-compose.polyphon.yml`; it comes up with the rest of the
stack and needs no per-refresh step.

## One-time operator setup: reserve an ngrok TCP address

Without a reservation, ngrok hands out a fresh `N.tcp.<region>.ngrok.io:<PORT>`
every time the tunnel restarts, so Anam can never be pre-configured and
the committed `turn_servers` host drifts on every refresh. Reserve a TCP
address once (requires a pay-as-you-go ngrok plan) and pin it:

1. In the ngrok dashboard reserve a TCP address (e.g.
   `1.tcp.us-cal-1.ngrok.io:12345`).
2. Export it before `make dev-refresh`:
   ```bash
   export MEMQL_NGROK_TURN_ADDR=1.tcp.us-cal-1.ngrok.io:12345
   ```
   `lib_write_ngrok_tunnels_config` pins it as the coturn endpoint's
   `url: tcp://<addr>` so host:port stay stable across restarts.

Both tunnels run from a single ngrok agent (`ngrok start --all`), so a
reserved TCP address is the only paid-plan dependency; a stock free
agent still brings the relay up on a dynamic addr each refresh.

## Credential

`coturn`'s static user (`docker/coturn/turnserver.conf`) and LiveKit's
`turn_servers[0].credential` are both committed with the **same** pinned
dev-only literal. It is not a real secret; it only gates the local relay.
Keep the two in sync if you rotate it.

## Hard-won gotchas (each cost a debugging cycle)

1. **Use a RAW ngrok TCP endpoint + plain `turn:` (`transport=tcp`), NOT
   an ngrok TLS endpoint + `turns:`.** ngrok's TLS endpoint terminates
   the handshake but mangles the bidirectional TURN stream --
   `turnutils_uclient` fails `ERROR: recv: Success` and Anam's allocate
   never completes. Raw TCP passthrough works cleanly. (TLS endpoints
   require PAYG, which the account now has, but they still don't work
   for TURN.)
2. **`rtc.node_ip` MUST be LiveKit's docker IP, not `127.0.0.1`.** From
   coturn's container, `127.0.0.1` is coturn itself, and coturn forbids
   relaying to loopback, so Anam's `CREATE_PERMISSION` for LiveKit's
   candidate fails `403 Forbidden IP`. The docker-bridge IP is reachable
   by both coturn (same network) and -- on Linux -- the host browser.
   dev-refresh stamps it.
3. **Don't use LiveKit's EMBEDDED `turn:` block.** It emits ZERO logs
   even at debug, so allocation failures are undiagnosable. The
   standalone coturn gives full per-allocation logging (`verbose`).

## Verifying the relay (yourself, no browser)

```bash
# coturn allocations as Anam connects:
docker logs -f memql-coturn | grep -E 'ALLOCATE|CREATE_PERMISSION|CHANNEL_BIND'

# A turnutils round-trip THROUGH the ngrok endpoint:
docker exec memql-coturn turnutils_uclient -T -u livekit \
    -w 54fc7588c919556c921567b313a66585 -p <ngrok-tcp-port> <ngrok-tcp-host>

# LiveKit advertising the external TURN + the stamped node_ip:
docker logs polyphon-livekit | grep -i turn
```

A working relay logs `ALLOCATE ... success`, `CREATE_PERMISSION ...
success`, `CHANNEL_BIND ... success`, and relays bytes both ways
(non-zero `rp/rb/sp/sb`). That round-trip + the browser media path is all
this relay was ever validated for -- the render of the avatar itself was
flagged as a separate, unverified concern (memql#772), and memql#1277
confirmed it does NOT render.

## Why cloud-avatar video can't render locally (memql#1277)

Pulling the live `polyphon-livekit` + `memql-coturn` logs during an actual
Simli direct-avatar join (room `avatar-*`) shows the relay is plumbed
correctly but the cloud-engine media leg still never connects:

1. **Signaling works.** Simli's cloud LiveKit agent (`kind=AGENT`,
   `sdk=PYTHON 1.1.8`, from Simli's cloud IP) joins the room over the ngrok
   https tunnel. It retries the join ~5x, ~15s apart, then gives up -- that
   window is the ~30s `engageVendor` "context deadline exceeded".
2. **The TURN relay IS advertised to the cloud engine.** LiveKit's join
   response to the avatar agent carries
   `iceServers: [{urls: ["turn:<ngrok-tcp>?transport=tcp"], ...}]` with
   `forceRelay: UNSET`.
3. **The cloud engine never uses it.** The cloud engine's IP never connects
   to coturn (every coturn session is from the docker host gateway =
   LiveKit-side + browser-side); it gathers **no relay candidate**.
4. **ICE has only unreachable host candidates -> media dies.** The avatar
   room's ICE pair stats show LiveKit offering only its docker host candidate
   (`172.18.0.x:7882`, unreachable from the cloud) and the cloud engine
   offering only its cloud-internal host candidate (`192.168.x.x`,
   unreachable from our LiveKit): `requestsSent: 8, responsesReceived: 0` ->
   both transports `failed` -> `leave reason CONNECTION_TIMEOUT`.

The blocker is intrinsic: relaying a **public cloud engine** into a
**NAT'd/dockerized local LiveKit** over **free tunneling** has no candidate
both sides can reach. Even forcing relay (`rtc.force_relay`) does not help --
LiveKit's own relay candidate would be coturn's docker-internal relay address
(no `external-ip` is set, and a single ngrok TCP port cannot carry coturn's
dynamic relay-port range anyway). Making it work would require LiveKit on a
publicly-reachable host (i.e. staging) or a TURN server with a real public IP
and full relay-port range -- neither is a local-dev-box shape.

**Fail-loud surfaces (so this is never re-debugged as a bug):**
- `make dev-refresh` (`lib_refresh_turn_relay`) prints a NOTE that local
  cloud-avatar video will not render.
- `avatardirect.engageVendor` replaces the cryptic vendor "context deadline
  exceeded" with a definitive error pointing at this limitation + the
  staging-validated path, when the engine URL is a dev ngrok tunnel.

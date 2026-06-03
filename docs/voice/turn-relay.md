# Anam avatar TURN media relay (local dev)

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

## What dev-refresh does (`step4c_turn_relay`)

`scripts/dev/lib.sh:lib_refresh_turn_relay` (called from
`scripts/dev/refresh.sh`) after the stack is up:

1. Reads `polyphon-livekit`'s docker-bridge IP and stamps it into
   `livekit-dev.yaml` `rtc.node_ip`.
2. Starts an `ngrok tcp 3478` tunnel to coturn (a **separate** ngrok
   agent on web-addr `:4041`, so it never shadows the LiveKit-signaling
   HTTPS tunnel on `:4040`).
3. Resolves the tunnel's `tcp://host:port` and stamps it into
   `rtc.turn_servers[0]` host/port.
4. Restarts `polyphon-livekit` so it reloads the stamped config.

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
   `lib_refresh_turn_relay` passes it to `ngrok tcp --remote-addr` so
   host:port stay stable across restarts.

Running both the signaling HTTPS tunnel and this TCP tunnel at once
needs a plan that allows two simultaneous agents (PAYG does).

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
(non-zero `rp/rb/sp/sb`). The render of the avatar itself is a separate,
Anam-cloud-side concern (memql#772).

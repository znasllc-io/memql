---
title: Calls
audience: public
status: stable
area: operate
sinceVersion: 0.31.40
owner: znas
description: Video calls on a deployable -- how a call works, turning calls on, the routes a page calls, the media servers calls take place on, and what to check when a call fails.
---

# Calls

A deployable can hold video calls. A person with a key starts a call or books one and
shares its link, and whoever opens the link joins it, with no sign-in and no MemQL
account. A call's video and sound travel between each participant's browser and a media
server (LiveKit), never through MemQL: MemQL decides who may take a seat, hands each
participant a short-lived pass, drives the server through its control API, and hears
what happens on it through the server's webhooks (the `livekit` source of
[inbound delivery](inbound-delivery.md)).

Calls are off on every deployable until an operator turns them on
([Turning calls on for a deployable](#turning-calls-on-for-a-deployable)). The media
servers are outside the cluster in every installation: locally three containers beside
k3d ([The local media server](#the-local-media-server)), and for a cloud installation a
rented server ([Production media server](#production-media-server)). How the edge serves
a calls deployable -- its ten routes, their budgets, the session cookie and the page
policy -- is in [Calls on a deployable](site-hosting.md#calls-on-a-deployable).

## How a call works

### Keys, sessions, links and passes

- **A key** opens calls on one deployable: `ck_` and 28 symbols of Crockford base32 (no
  `i`, `l`, `o` or `u`, and case does not matter), the last two a checksum that catches
  any one mistyped symbol and any two swapped neighbours. MemQL stores only a keyed
  digest of it (HMAC-SHA256 under the cluster's key pepper), so a key is shown once, when
  it is minted, and a lost key is replaced, never recovered. A key belongs to the
  deployable's owner, whoever minted it, and is `active`, `frozen` (it opens no new call)
  or `banned`.
- **A session** is what a browser holds: a key's, opened by unlocking the key, or a
  seat's, opened by a first join through a call's link. It rides the
  `__Host-memql_calls` cookie (`HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`), lasts
  a day, and opens nothing on any other deployable. A session is not a MemQL identity: it
  carries no JWT, opens no stream and reaches no other route. Leaving a call does not end
  it: a key holder starts the next call on it, and a guest who hung up comes back on the
  same seat.
- **A call's two links** are handed out once, when the call is started or booked: the
  guest link `https://<host>/#j=<guest token>`, and the host link, which adds
  `&h=<host token>`. `<host>` is the deployable's live custom domain when it has one, and
  its own hostname otherwise. The tokens are in the fragment, so no server log and no
  `Referer` carries them, and a page joins with one of them: the host token alone for a
  host link.
- **A pass** is what a participant connects to the media server with: a LiveKit token
  for one seat in one room, signed with the server's key pair and good for `passTtl`
  (120 seconds) to connect with. A person's pass grants the microphone, the camera, the
  screen share and its sound, and data messages, and never the right to rename itself. A
  pass cannot be revoked once it is handed out, so the seat it was for is what decides:
  the media server's participants are held to the seats (below).

### Seats, the lock and the line

Every participant takes a seat in the call's room. A join is decided against the room and
its media server, in this order:

1. A call that has ended refuses it (`call_ended`).
2. A host passes the lock: the key holder who started the call, anyone who joins with its
   host link, and a seat a host admitted.
3. A room holds `maxPeople` people (8) and `maxAgents` agents (2); past that a join is
   refused (`room_full`, `agent_seats_full`).
4. In a locked call anybody else waits (`wait`, reason `locked`) until a host admits them
   or unlocks the call.
5. A media server carries the seats `MEMQL_CALLS_MEDIA_SEATS` gives it, across every call
   on it. With none free, a join takes a place in the server's line (`queue`, reason
   `capacity`). The line drains by class -- people joining a call already running, then
   booked calls that are due, then new calls -- and by arrival within a class. A waiter
   polls (`GET /queue`) every `queuePoll` (3 seconds), keeps its place for
   `queueEntryTtl` (10 minutes) after its last poll, and is let in on its own poll, the
   only answer that can carry its pass. A waiter whose call is locked while it holds a
   place keeps its arrival, and the place comes back when the lock lifts; one that waited
   behind the lock from the start joins the back of the line then.

A room counts every join attempt, a refused one and a rejoin included, and takes
`roomJoinsPerMinute` (30) a minute (`rate`, answered `429`). A session already seated in
the call rejoins on the same seat with a fresh pass, counted against no limit; one seated
in another open call is refused (`in_another_call`). One key holds one call at a time: a
start is refused while a call of the key is open (`key_in_call`).

**Every join confirms three things**, and the seat records them: the person is 18 or
older, nobody under 18 will appear on their camera or screen, and which version of the
terms they accept. A join without all three is refused.

**Every pass is handed out once its call's media room is open.** The media server opens no
room a connection names (`room.auto_create` is off), and it can lose a room a call still
holds: it closes one that has stayed empty, and forgets them all when it restarts. So
MemQL opens the call's media room again, with the call's limits (`maxPeople` +
`maxAgents` + 1 connections), before it hands out each pass, and everybody who reconnects
after a restart gets a room that exists. A media server that cannot open the room hands
out no pass, and the seat keeps its place for the next ask.

### Hosts

A host control is made by a host seat in the call now: the key holder who started it,
anyone who joined with its host link, or a key holder the call was handed to. A call can
have several hosts, and any of them acts. The controls are `POST /host` with an `action`
and, for one done to a seat, the `seat`:

| Action | Does |
|---|---|
| `mute` | mutes the seat's microphone; only its holder unmutes it |
| `camera_off` | turns the seat's camera off |
| `stop_share` | stops the seat's screen share and its sound |
| `remove` | takes the seat out of the call for the rest of it |
| `lock`, `unlock` | a locked call makes every newcomer wait for a host |
| `admit` | lets one waiting seat in past the lock |
| `end` | ends the call for everyone in it |

**The roster** is how a host sees whom the controls name. `GET /session` answers it to a
host seat in an open call: whether the call is locked (so the host is offered Lock or
Unlock, not both), the seats in the call, each with its media identity, which is the
media server's participant identity, and the seats waiting to come in, in line order. A
host's page polls it about every 3 seconds; nothing else tells a page who is knocking,
since a waiter is not in the media room. Any other session's answer carries no roster.

**Who hosts** is told the same way. Every seat's `GET /session` says `host: true` while
the seat hosts its open call, so a key holder the call was handed to learns that it hosts
it without a new pass, and its page can show the host controls.

One screen share at a time: a new one stops every other participant's share in the call.

### How a call ends

A call ends in one of three ways, and its room records which (`endReason`):

- **`ended`**: a host ended it.
- **`host_left`**: nobody hosting it has been in it for `hostLeftGrace` (5 minutes). A key
  holder still in the call takes it over at once instead, and a host who comes back
  within the grace stops the countdown. A host who never got in, because they waited in
  line from the start, never left, so nothing counts down for them.
- **`empty`**: the last person in it left with nobody waiting to come in.

**A booked call is its host's until its booked end.** A host who opens it early to check
it and leaves is counted from the booked start, so the call never ends before it begins.
A host who never came has not left: a guest who opens the call, early, on time or late,
waits behind its lock, nothing counts down before the booked end, and a host who arrives
late is let in by the host link and runs the call. After the booked end a call whose host
never came is counted from the end: it ends `host_left` the grace later if somebody still
waits in it, and `empty` if nobody does. **It ends empty only after its booked end**:
before that, a booked call that empties stays open for the people still to come. A call
that runs past its booked end is never cut off.

**Every seat is told the countdown.** While it runs, every seat's session says when the
call ends unless a host is back first (`countdownEndsAt`, below), the instant the
reconcile pass ends it at, so every page can show the same countdown.

**A media disconnect is not a leave.** A browser disconnects from the media server the
same way when its page is refreshed, its tab is closed or its call is hung up, so the
media server's report of a disconnect only records the seat's last sign of life. Only
the leave route, `POST /leave`, the hang-up, closes a seat at once. A seat whose
participant stays gone is closed by the reconcile pass once it has been missing for one
pass lifetime (`passTtl`), counted from the first pass that found it missing, and it is
closed as having left when it was last seen: a host seat closed so starts the countdown
from then, and the empty rule applies then. A pass that misses a seat only records that
(`absentSince`), so neither a refresh, which reconnects within seconds, nor a media
server restart that a pass lands in the middle of, ends a call.

### Booked calls

A key's session books a call (`POST /schedule`): a start, a length in minutes (at most
`maxScheduledMinutes`, 480) and an optional title of up to 80 characters, at most
`scheduleHorizon` (30 days) ahead, at most `maxUpcomingScheduled` (20) upcoming per key,
and not overlapping another booking of the key. The answer carries the call's two links,
once, and a calendar file (`ics`) with the guest link.

Until 10 minutes before the start, a link answers `not_yet` with the time until it
opens. From then, the first join opens the call's room, locked: guests wait for a host,
and the host link passes the lock. Moving a booking (`POST /reschedule`) replaces both
links, so the old ones open nothing, and hands out a calendar file with the first one's
UID. A booking can be cancelled (`POST /cancel`) only before its
room opens; its links then answer `cancelled`, and those of a booking whose end passed
unopened answer `call_ended`.

### What the media server reports, and the reconcile pass

The media server sends four events to `https://api.<domain>/inbound/livekit`. Each is
verified with the webhook key pair, applied by the `applyMediaEvent` automation, and
applied only when it is newer than what the rows record:

- `participant_joined` puts the seat in the call (`in_call`) and records it as seen.
- `participant_left` records when the seat was last seen, and nothing else (above).
- `track_published` of a screen share stops every other share in the call.
- `room_finished` ends a call only when the empty rule would; otherwise the next pass
  opens the room again.

The media server's own list of participants is the truth, and the events only follow it.
So every 30 seconds one bff replica, the holder of the cluster lease `calls-reconcile`,
repairs every open call against that list: it closes the seats whose participant has
stayed gone for `passTtl` (above), disconnects every participant that holds no seat of
the call (a pass cannot be revoked,
so this is what keeps a removed person out), expires the places in line nobody polls,
applies the countdown and ends the calls nobody is in. It hands out no pass.

### What is stored

Every calls row is server-written and unreadable by any client: the routes and the
builtins below are the only ways in, and a secret is stored only as its digest. A
deployable's rows belong to the deployable's owner.

| Concept | One row per |
|---|---|
| `v1:calls:key` | key: its digest, kind and status |
| `v1:calls:session` | session: its token's digest; deleted 30 days after it is written |
| `v1:calls:room` | call: its media server, lock, host-left time and how it ended; its superseded versions are deleted a day after they are written, and its newest stays |
| `v1:calls:seat` | participant in a call: kind, status, confirmations, when last seen and when a pass found it missing |
| `v1:calls:queueEntry` | place in a media server's line; deleted after 30 days |
| `v1:calls:scheduledCall` | booked call: its links' digests and its status |
| `v1:calls:mediaServer` | media server: the cluster's, and no deployable's |
| `v1:calls:policy` | deployable's limits, where they differ from the defaults |

The key pepper and the media server's two key pairs are sealed global secrets
(`calls.key-pepper`, `calls.media.bootstrap`, `calls.media.webhook`).

## Turning calls on for a deployable

Four builtins. The three that name a deployable are a person's acts, as Deployables makes
them: they refuse the operator credential, a connector and any call made inside a run,
and they need `execute app:deployables/calls` (the owner and developer roles hold it, and
an organization's group can be granted it), judged in that deployable's organization, on
a deployable the caller can read. The fourth, the media server list, is a cluster owner's
read.

```
builtin callsSiteEnable(siteId: "<deployable>", on: true)
builtin callsTestKeyCreate(siteId: "<deployable>", label: "Reviewers")
builtin callsPolicySet(siteId: "<deployable>", maxPeople: 4)
builtin callsMediaServers()
```

- **`callsSiteEnable`** switches a deployable's calls on or off. Turning them on needs a
  deployable with an owner, since every calls row belongs to the owner; a system-owned
  deployable holds no calls. Switched off, every calls route answers `404`: the edge drops
  its copy of the deployable through the change feed, and the bff checks the switch again
  on every request it is forwarded. People already in a call keep their media connections
  until they leave, and nobody joins or starts another.
- **`callsTestKeyCreate`** mints a test key and answers `{keyId, key}`, the key shown this
  once. A test key spends no balance; nothing mints any other kind yet. It needs a media
  server configured on the cluster, because the key pepper exists only once one has
  been. The label is the operator's name for the key and is never sent to a caller.
- **`callsPolicySet`** sets any of a deployable's limits. A limit not named keeps its
  value, and one never set is the default. Counts are whole numbers and durations whole
  seconds, each held to the cluster's bounds, so no deployable claims more than the
  cluster allows. A media room keeps the participant cap it was opened with, so a call
  already open keeps its old cap on the media server.
- **`callsMediaServers`** lists the active media servers,
  `{id, provider, publicUrl, region, seats, state, createdAt}`: never a credential and
  never the control address.

| Limit | Default | Bounds |
|---|---|---|
| `maxPeople` | 8 | 1 to 1000 |
| `maxAgents` | 2 | 0 to 100 |
| `roomJoinsPerMinute` | 30 | 1 to 10000 |
| `maxUpcomingScheduled` | 20 | 0 to 1000 |
| `maxScheduledMinutes` | 480 | 1 to 1440 |
| `hostLeftGrace` | 300 s | 0 to 86400 s |
| `passTtl` | 120 s | 10 to 900 s |
| `scheduleHorizon` | 2592000 s (30 days) | 0 to 31622400 s |
| `queueEntryTtl` | 600 s | 10 to 86400 s, and longer than `queuePoll` |
| `queuePoll` | 3 s | 1 to 60 s |

To try a deployable, mint a test key, switch its calls on, open the deployable and unlock
with the key, start a call, and open its guest link in a second browser.

## The routes

A page calls ten routes on its own deployable's origin, under `/_memql/calls/`. Every
POST sends a JSON object (`Content-Type: application/json`) from the deployable's own
origin, holding no field the route does not take, and every answer is JSON and
`Cache-Control: no-store`.

| Route | Sends | Answers |
|---|---|---|
| `POST /unlock` | `{"key"}` | the key's session, and sets the cookie |
| `GET /session` | nothing | what the cookie holds; to a seat in an open call, whether it hosts it and, while the countdown runs, when the call ends; to a host, the roster too |
| `POST /start` | `{"name", "confirm"}` | `{"call": {"id", "guestToken", "hostToken"}, "join"}` |
| `POST /schedule` | `{"start", "minutes", "title"}` | the booking: `id`, `start`, `end`, `title`, `guestToken`, `hostToken`, `guestLink`, `hostLink`, `ics` |
| `POST /reschedule` | `{"id", "start", "minutes", "title"}` | the booking, with new links |
| `POST /cancel` | `{"id"}` | `204` |
| `POST /join` | `{"token"}` or `{"hostToken"}`, with `"name"` and `"confirm"`; neither token, to rejoin on the session | a join, and the cookie of a new session |
| `GET /queue` | nothing | a join: a waiter's poll, or a fresh pass for a seat already in |
| `POST /host` | `{"action", "seat"}` | `204` |
| `POST /leave` | `{}` | `204`: the hang-up |

`confirm` is `{"adult": true, "noMinors": true, "terms": "<version>"}`, and `start` an RFC
3339 time. A session's answer is `{kind, keyId, seatId, roomId, expiresAt, expiresInMs}`,
its ids bare, and never its token; to a seat in an open call it adds `host: true` while
the seat hosts the call, `countdownEndsAt` (an RFC 3339 time) while the host-left
countdown runs, and to a host the `roster`, each absent otherwise. `expiresAt` less
`expiresInMs` is the server's clock, which a page counts `countdownEndsAt` down from. A
join's answer is
`{outcome, url, pass, identity, host, expiresInMs, retryAfterMs, position, reason}`:
`admit` with the media server's address, the pass and the seat's media identity, or
`wait` (behind the lock) or `queue` (with the place, counted from one), each with when to
ask again, in `Retry-After` too. A host's roster:

```json
"roster": {
  "locked": false,
  "inCall": [{"seat": "<seat id>", "displayName": "Ada", "kind": "person", "host": true, "status": "in_call", "identity": "<media identity>"}],
  "waiting": [{"seat": "<seat id>", "displayName": "Lin", "kind": "person", "since": "2026-11-19T12:00:00Z"}]
}
```

A refusal is `{"error", "reason", "retryAfterMs"}`, its reason stable for a page to
switch on:

| Status | `error` | When |
|---|---|---|
| 400 | `invalid` | a body that cannot be read, a missing confirmation, and `in_past`, `beyond_horizon`, `too_long`, `title_too_long`, `minutes_invalid` |
| 401 | `no_session` | no session, or one that ended or expired; the cookie is taken back |
| 401 | `invalid_key` | an unlock whose key opens nothing |
| 403 | `forbidden` | `removed`, `key_not_active`, `not_host`, a session that may not do this, and the edge's `foreign_origin` |
| 404 | `not_found` | nothing here: calls off, a link or a booking that opens nothing; every miss is the same 404 |
| 409 | `conflict` | `room_full`, `agent_seats_full`, `in_another_call`, `key_in_call`, `no_media_server`, `overlaps`, `too_many_upcoming`, `started`, `not_waiting`, and `not_yet` with when the call opens |
| 410 | `ended` | `call_ended`, `cancelled` |
| 413, 415 | `too_large`, `not_json` | a body over the route's cap, a POST that is not JSON |
| 429 | `limited` | the room's join attempts for this minute, or one of the edge's budgets, with when to ask again |
| 502 | `failed` | anything else, which the bff logs with the route and the deployable |

The bff serves the routes at `/calls/*` and answers only what the edge forwards: the edge
stamps the deployable and its owner, and the bff reads the deployable again under that
owner and requires its calls switch to be on before it reads anything else. Nothing is
read until a key, a link or a cookie of a shape MemQL issues is presented.

## Environment

| Variable | Read by | Is |
|---|---|---|
| `MEMQL_CALLS_MEDIA_PROVIDER` | bff | `docker-local` or `hetzner`, recorded on the media server and decided on by nothing |
| `MEMQL_CALLS_MEDIA_API_URL` | bff | the control API: http, https, ws or wss |
| `MEMQL_CALLS_MEDIA_PUBLIC_URL` | bff | where browsers connect: wss or https |
| `MEMQL_CALLS_MEDIA_SEATS` | bff | how many participants the server carries at once |
| `MEMQL_CALLS_MEDIA_API_KEY`, `MEMQL_CALLS_MEDIA_API_SECRET` | bff | the key pair rooms and passes are signed with; the secret 32 characters or more |
| `MEMQL_CALLS_MEDIA_WEBHOOK_KEY`, `MEMQL_CALLS_MEDIA_WEBHOOK_SECRET` | bff | the key pair the server signs its webhooks with: another key, the secret 32 characters or more |
| `MEMQL_CALLS_MEDIA_HOST_SUFFIX` | edge | the DNS name every media server's host is at or under, which a calls page's policy admits |

**All eight or none.** With none set the cluster has no calls media server, and a call
can be neither started nor opened (`no_media_server`). A partial set refuses the bff's
boot, naming what is missing. The eight come from the `memql-calls-media` Secret, which
the bff reads optionally (`deploy/k8s/components/calls-media`), so an installation
without it starts unchanged. At boot the bff records the server the values name and
seals its key pairs into the `calls.media.bootstrap` and `calls.media.webhook` global
secrets, and the first boot that finds no key pepper generates `calls.key-pepper`. The
pepper is never replaced: every key's digest depends on it. A pod reads its environment
when it starts, so a Secret created or changed afterwards reaches the bff when it
restarts.

**The suffix** is derived as `media.<domain>` in every overlay, so set it only when the
media servers live under another name. Unset, or not a host name, a calls page's policy
names no media server: the routes answer and passes are minted, but the browser refuses
to connect to the call, and the edge's one warning at boot is the clue.
[Environment variables](env-vars.md) lists all nine.

### The local media server

Locally the media server is three containers beside k3d, from `deploy/media/compose.yaml`:
LiveKit, its Redis, and Caddy terminating TLS with the front door's mkcert pair at
`wss://media.<domain>:7443`.

```bash
make media-up     # start it; make up runs this last
make media-down   # stop it and remove its rendered configuration; make down runs this first
make up MEDIA=0   # bring the cluster up without it
```

`make secrets` seeds the `memql-calls-media` Secret whether or not the media server runs,
so with `MEDIA=0` or after `make media-down` the bff still places calls on a server that is
not running, and a join fails when the call's room is opened; `make media-up` starts it
again and nothing needs restarting.
[Reproduce the cloud locally](reproduce-the-cloud-locally.md#the-calls-media-server) has the
rest.

## A deployable on its own domain

A calls deployable can answer on a domain of its own (a custom domain binding), and the
links its calls hand out then name that domain. When the domain sits behind a fronting
proxy such as Azure Front Door, the binding declares the front in `frontedBy`, so that its
pointing check accepts the CNAME chain through the front's endpoint:
[Behind Azure Front Door](front-door.md#behind-azure-front-door). The edge's same-origin
check compares a POST's `Origin` with the request's `Host`, so the front must forward the
`Host` header unchanged, as that section configures it. The media server is never behind
the front: browsers connect to it directly.

**Behind a front, the edge's budgets are the front's.** The edge counts its calls budgets
(10 unlocks, 30 joins, 120 host controls and hang-ups and 600 other requests a minute per
address, on each replica) by the right-most `X-Forwarded-For` address, and behind a front
that address is the front's, not the visitor's. So every visitor arriving through one
front address shares those budgets: one of them sending 10 unlocks a minute keeps the rest
from unlocking, and enough polling pages behind one address use up its 600 between them.
Hanging up and the host's controls have their own 120, so polling and joining never keep a
person from leaving or a host from acting. The front's own per-address rate limit
([Behind Azure Front Door](front-door.md#behind-azure-front-door), step 4) is what bounds
each visitor until the edge can learn the visitor's address behind a proven front.

## Production media server

A production media server is one Hetzner Cloud server in a US location (`ash`,
Ashburn, or `hil`, Hillsboro) running LiveKit, the Redis it keeps room state in, and
Caddy with the layer-4 module in front of both. The engine reaches it through the eight
`MEMQL_CALLS_MEDIA_*` values in the installation's `memql-calls-media` Secret:

| Value | On a Hetzner server |
|---|---|
| `MEMQL_CALLS_MEDIA_PROVIDER` | `hetzner`, recorded on the media server and decided on by nothing |
| `MEMQL_CALLS_MEDIA_API_URL` | `https://m1.media.<domain>`: the control API, through Caddy |
| `MEMQL_CALLS_MEDIA_PUBLIC_URL` | `wss://m1.media.<domain>`: where browsers connect |
| `MEMQL_CALLS_MEDIA_SEATS` | how many participants the server carries at once |
| `MEMQL_CALLS_MEDIA_API_KEY`, `MEMQL_CALLS_MEDIA_API_SECRET` | the key pair rooms and passes are signed with |
| `MEMQL_CALLS_MEDIA_WEBHOOK_KEY`, `MEMQL_CALLS_MEDIA_WEBHOOK_SECRET` | the key pair the server signs its webhooks with |

An installation runs one media server for now: the eight values name exactly one, and
every call is placed on it. `<domain>` below is the installation's domain, and `m1` the
server's name.

You need a Hetzner Cloud project with your SSH public key in it (Security > SSH keys)
and a read-write API token for it (Security > API tokens); the instance's DNS; a way to
set the instance's Secrets; and `curl`, `jq`, `openssl` and `ssh` on your machine.

### 1. Provision the server

`scripts/media/provision-hetzner.sh` reads the API token from `HCLOUD_TOKEN` in your
environment. Read it without echoing it, so it lands in neither the terminal nor the
shell's history:

```bash
read -rs HCLOUD_TOKEN && export HCLOUD_TOKEN
```

Look first, then create:

```bash
scripts/media/provision-hetzner.sh --name=m1 --location=ash --server-type=<type> \
  --ssh-key=<key name> --dry-run=true
scripts/media/provision-hetzner.sh --name=m1 --location=ash --server-type=<type> \
  --ssh-key=<key name> --confirm=provision-m1
```

`--server-type` is a type the location offers (Hetzner's console lists them). Size it by
a load test; until one has run, start with at least 4 GB of memory. `--image` defaults
to `ubuntu-24.04`, the only image the server's first boot is written for.

The dry run calls nothing and needs no token: it prints the two requests a run would
send and, on stderr, the cloud-init the server would boot with. A run without
`--confirm=provision-<name>` is refused before anything is called, because the server
is billed by the hour. A run:

1. creates the firewall `memql-media`, once, and applies it to every server labelled
   `memql-media`;
2. creates the server, labelled `memql-media=m1`, with an IPv4 and an IPv6 address and
   your SSH key;
3. answers `{serverId, ipv4, ipv6}`. `ipv6` is the server's own address, the first in
   the /64 Hetzner routes to it.

Run again, it answers the server it made and creates nothing. It refuses a server or a
firewall of that name that it did not make, and a server it made that is somewhere
else or of another type: it never changes an existing server to match.

The firewall opens these ports, and the server's own firewall (ufw) opens the same:

| Port | For |
|---|---|
| 22/tcp | SSH, for step 3, which hands the server its keys |
| 80/tcp | certificate issuance (the ACME HTTP challenge) |
| 443/tcp | signalling, the control API and TURN over TLS, routed by server name |
| 7881/tcp | media over TCP |
| 7882/udp | media over UDP |
| 3478/udp | TURN over UDP, which also serves participants as STUN |
| ICMP | ping, and path MTU discovery |

The script creates the firewall and never rewrites one that exists, so you may narrow
the SSH rule to your own address in the Hetzner console (and the matching `ufw` rule on
the server).

The server's first boot installs Docker from Ubuntu's archive, writes the media
server's files to `/etc/memql-media`, opens the host firewall and pulls the pinned
images. It starts nothing: the server cannot run before it has its keys.

### 2. Add the DNS records

| Name | A | AAAA |
|---|---|---|
| `m1.media.<domain>` | `ipv4` | `ipv6` |
| `turn.m1.media.<domain>` | `ipv4` | `ipv6` |

The first is where browsers connect and the engine drives the server; the second is
its TURN server, which LiveKit offers browsers at port 443 under that name. Caddy
obtains a certificate for each as soon as it starts, so both names must point at the
server before step 3 (a certificate authority prefers IPv6 when it finds an AAAA
record, so a wrong one can fail the certificate too).

**The server's public host must sit under the media host suffix**, which is
`media.<domain>` unless you set it otherwise (step 4): a calls page's policy lets the
browser connect only to hosts under that suffix, and the browser refuses any other host
without telling the page why. The TURN name is the public host with `turn.` in front,
so it is under the suffix too. The suffix's own host, `media.<domain>`, is reserved:
no deployable can take it.

### 3. Make the eight values, hand them to the server, and put them in the Secret

Write the eight values to a file only you can read, one `NAME=value` a line with no
quotes (the format `kubectl` reads and the server's command reads). `echo` is built
into the shell, so the new secrets appear in no command line:

```bash
umask 077
{
  echo "MEMQL_CALLS_MEDIA_PROVIDER=hetzner"
  echo "MEMQL_CALLS_MEDIA_API_URL=https://m1.media.<domain>"
  echo "MEMQL_CALLS_MEDIA_PUBLIC_URL=wss://m1.media.<domain>"
  echo "MEMQL_CALLS_MEDIA_SEATS=<seats>"
  echo "MEMQL_CALLS_MEDIA_API_KEY=callsapi$(openssl rand -hex 12)"
  echo "MEMQL_CALLS_MEDIA_API_SECRET=$(openssl rand -hex 32)"
  echo "MEMQL_CALLS_MEDIA_WEBHOOK_KEY=callshook$(openssl rand -hex 12)"
  echo "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET=$(openssl rand -hex 32)"
} > media.env
```

Hand them to the server, over SSH, on the standard input of the command its first boot
installed (the first connection asks you to confirm the server's host key):

```bash
ssh root@<ipv4> memql-media configure --domain=<domain> < media.env
```

`memql-media configure` waits for the first boot to finish, checks the values (all
eight, the provider `hetzner`, the control API at the public host over TLS, secrets of
32 characters or more, two different keys), renders LiveKit's configuration from the
same template the local target uses (`deploy/media/livekit.yaml.tmpl`, with the
server's public IPv4 from Hetzner's metadata, the webhook URL
`https://api.<domain>/inbound/livekit`, and its TURN server on), renders Caddy's for the
two names, starts the server, and waits until both names answer through Caddy: LiveKit
at the public host, and a TLS handshake with a certificate for the TURN name (what
answers behind that name is TURN, so the handshake is the whole check; each attempt gives
up after 10 seconds, so a name that never completes TLS cannot hold the command). If it reports
either missing, the server is started but Caddy has no certificate for that name yet:
check the records from step 2 and that ports 80 and 443 reach the server, then run the
same command again, which changes nothing already in place and checks again.

Then put the same file in the instance's `memql-calls-media` Secret, the way the
instance keeps its Secrets: an entry in its secret store that its ExternalSecret reads,
or directly:

```bash
kubectl create secret generic memql-calls-media --namespace=memql --from-env-file=media.env
```

The bff reads the Secret optionally: an installation without it starts unchanged, and a
join answers that no media server is configured. Delete `media.env`, or keep it only
where the instance keeps its secrets; it holds both key pairs.

**Why the keys travel this way.** The server's user data, which its first boot runs, is
kept by Hetzner and handed out again (through its API and console, and through the
metadata service to every process on the server), so the cloud-init carries no key,
token or secret, and the provision script never reads the media server's values at all.
The keys arrive on the command's standard input instead: `ssh` carries it encrypted to
the server it authenticated, the values are in no command line (which every process on
a machine can read), in no container's environment, and in no file on the way, and the
one copy written is the configuration the server runs on,
`/etc/memql-media/livekit.yaml`, mode 0600 in a 0700 directory. What runs on the server
can sign passes with those keys, so the server is the one place outside the cluster
that holds them.

### 4. Check the media host suffix

The edge's `MEMQL_CALLS_MEDIA_HOST_SUFFIX` is derived from the installation's domain as
`media.<domain>` (`deploy/k8s/components/calls-media`), so a server named
`m1.media.<domain>` needs nothing set. Override it only when the media servers live
under another name: patch the value on the edge Deployment in the instance's overlay,
to a DNS name every server's public host is under. `memql-media configure` warns when
the public host is not under `media.<domain>`.

### 5. Sync

Sync the instance (its ArgoCD Application), then restart the bff so it reads the Secret:
a pod reads its environment when it starts, so a Secret created or changed after the bff
started reaches only the pods started after it.

```bash
kubectl rollout restart deployment/bff --namespace=memql
kubectl logs deployment/bff --namespace=memql | grep "calls: media server ready"
```

At boot the bff records the media server the values name and seals its credentials, and
the `callsMediaServers` builtin (a cluster owner's read) lists it with provider
`hetzner` and its public URL. Calls are then switched on deployable by deployable, with
`callsSiteEnable`.

### Draining a server

MemQL does not yet move calls between servers or mark one as draining, so a server leaves
service only when no call is open on it. To drain it:

1. Switch calls off on every deployable that has them on (`callsSiteEnable` with
   `on: false`). Their calls routes then answer `404`, so nobody starts or joins a call;
   people already in a call keep talking until they leave.
2. Wait until the server holds no room:

   ```bash
   ssh root@<ipv4> memql-media rooms
   ```

   It prints how many rooms LiveKit holds; drained is `0`.

### Rotating the keys

Rotating restarts LiveKit with the new keys, which ends every call on the server, and
until the bff restarts too a pass it mints is signed with the old key. So:

1. Drain the server.
2. Write a new `media.env`: the same first four values, two new key pairs.
3. Hand it to the server: `ssh root@<ipv4> memql-media configure --domain=<domain> < media.env`.
4. Replace the instance's `memql-calls-media` Secret with it, and restart the bff (step 5):
   at boot it seals the new credentials over the old ones.
5. Switch calls back on.

### Replacing a server

A server keeps what its first boot wrote and pulled: the LiveKit template, the compose
file, `memql-media` itself and the pinned images. `memql-media configure` renders from
those files, never from the repository, so a later change under `deploy/media`, a
LiveKit upgrade included, reaches production only on a server made from the new
revision. Until then the local target, which renders from the checkout, can run what no
production server runs. To move the installation from `m1` to `m2`:

1. Drain `m1`.
2. Create `m2` from the new revision: steps 1 to 3 under its own name, with its own DNS
   records and a `media.env` of its own, with new key pairs.
3. Put `m2`'s values in the `memql-calls-media` Secret and restart the bff (step 5): the
   media server MemQL records takes `m2`'s addresses and keys.
4. Check that a call works, then switch calls back on.
5. Remove `m1`, below.

### Removing a server

Drain it first. MemQL repairs a call's seats from what its media server reports, and a
server that is gone reports nothing, so a call still open on a removed server is never
repaired: its seats stay in the call.

- **To replace it**, follow [Replacing a server](#replacing-a-server), which removes the
  old server last.
- **To leave the installation without one**, keep calls off. With the eight values gone
  from the Secret the bff records nothing new, but it keeps the media server it recorded
  last and places calls on it, so switching calls on again would place them on a server
  that is not there, and every join would fail when its room is created.

Delete the server in the Hetzner console, or with `hcloud server delete m1`, and remove
its four DNS records. The `memql-media` firewall stays for the other media servers;
delete it with the last one.

### What runs on the server

| Container | Image | Does |
|---|---|---|
| `livekit` | `livekit/livekit-server:v1.13.7`, pinned by digest | signalling and the control API on loopback port 7880, media on 7881/tcp and 7882/udp, TURN on 3478/udp and behind Caddy on 5349 |
| `redis` | `redis:7.4-alpine`, pinned by digest | room state, on loopback, none of it kept: a restarted server starts empty and MemQL repairs its calls' rows |
| `caddy` | `livekit/caddyl4:v2.11.3`, pinned by digest | port 443, routed by TLS server name: the public host to LiveKit's port 7880 over HTTP/1.1, the TURN name to its TURN server with the client's address in a PROXY header; the certificates for both |

All three run on the host's network, as LiveKit's own self-hosting layout runs them, from
`/etc/memql-media/compose.yaml`, and Docker restarts them after a reboot. Docker runs
with `live-restore`, so a restart of the Docker daemon, which Ubuntu's unattended
security updates make when they update Docker, leaves the containers and their calls
running. Every container logs to the system journal (Docker's `journald` log driver),
and the journal keeps nothing past six days and at most 1 GB
(`/etc/systemd/journald.conf.d/90-memql-media.conf`), so LiveKit's logs, which name
participants' identities and rooms, live less than a week; `docker logs` and
`docker compose logs` read them there. A
server is made with the LiveKit and Redis versions the local target runs, and keeps them
until it is replaced ([Replacing a server](#replacing-a-server)): an upgrade of LiveKit
is reviewed first, for both targets. The Caddy build is LiveKit's own, Caddy with the
layer-4 and YAML modules.

A server made from a revision matches that revision's local target wherever the two can
agree: rooms exist only once MemQL creates them, the codecs are Opus with redundancy,
VP8 and H.264 with retransmission, a participant receives at most 10 video tracks at
once, and the webhook sends only the four events MemQL applies. Unlike the local target,
it runs its own TURN server, so a participant whose network lets out only HTTPS still
reaches it.

## Troubleshooting

- **Every calls route answers `404` `not_found`.** The deployable's calls switch is off,
  the deployable is not live or is system-owned, or the edge has no
  `MEMQL_EDGE_API_TARGET`. When the edge forwarded a request and the bff answered `404`,
  the bff logs `calls surface: a request was answered 404, see reason`, with the reason:
  the stamped owner reads no row of the deployable, the read answered another deployable,
  the stamped owner is not the deployable's owner, or its calls switch is off.
- **A start or a join answers `409` `no_media_server`.** The cluster has no active media
  server: the `memql-calls-media` Secret is missing, or the bff started before it existed
  and needs a restart. `callsMediaServers` lists what the cluster places calls on.
- **A start or a join answers `502` `failed`, and the bff logs
  `calls surface: a request failed` with an error about opening the call's media room.**
  MemQL could not reach the media server's control API. Locally, run `make media-up`; in
  production, check the server, its Caddy and `MEMQL_CALLS_MEDIA_API_URL`.
- **A page is handed a pass, and the browser never connects.** The page's policy does not
  admit the media server: its public host must be under `MEMQL_CALLS_MEDIA_HOST_SUFFIX`,
  and the edge warns at boot when the suffix is unset or not a host name. Or the browser
  does not trust the server's certificate, or, in production, the participant's network
  lets out only HTTPS and the TURN name has no certificate: `memql-media configure`
  checks both names and says which one does not answer.
- **Seats stay `admitted` and never read `in_call`, and a participant who left stays
  listed until a pass closes the seat.** The media server's webhooks are not arriving. The
  reconcile pass still repairs every call every 30 seconds, so calls work, with that lag.
  Check that the server reaches `https://api.<domain>/inbound/livekit` (locally, the
  LiveKit container trusts the mkcert root and maps `api.<domain>` to the host), and that
  the bff answers there: one with no media server configured answers `404`.
- **The bff's boot is refused, naming `MEMQL_CALLS_MEDIA_*` values.** A partial set of
  the eight: set them all, or none.
- **A join answers `429` `limited`.** The room has taken its `roomJoinsPerMinute` join
  attempts for this minute, or one of the edge's budgets ran out; the answer says when to
  ask again.
- **A call ended.** Its room's `endReason` says why: a host ended it (`ended`), the
  host-left countdown ran out (`host_left`), or the last person left with nobody waiting
  (`empty`). A booked call ends neither of the last two ways before its start, nor, when
  its host never came, before its booked end.
- **Everybody's connection dropped at once.** The media server restarted, or the call's
  media room was closed. Each page asks for a fresh pass (a poll or a rejoin), and each
  pass opens the call's media room again first, so everybody comes back into a room that
  exists. A reconcile pass that lands before they are back closes nobody: it records each
  seat missing, and closes only one still missing `passTtl` later, so everybody back
  within that keeps their seat. On a production server Docker runs with `live-restore`,
  so a restart of Docker alone, which an unattended security update can make, leaves
  calls running; a restart of LiveKit ends them.
- **A person a host removed is back.** A pass cannot be revoked, so the reconcile pass
  disconnects their identity again within 30 seconds whenever the media server shows it.
  A person who cleared their session and came back through the link has a seat of their
  own ([Known limits](#known-limits)).
- **A page's background blur does not start.** A calls page's policy allows compiling
  WebAssembly (`'wasm-unsafe-eval'` in `script-src`) and nothing more, so the policy lets
  it run; the page has to load the model the blur runs from its own origin.

## Known limits

- One media server per installation, and calls are not moved between servers.
- No operation drains or removes a media server. A media server's row has a state
  (`active`, `draining`, `removed`), but only the boot that records the server writes it,
  as `active`, so a server leaves service by being [drained](#draining-a-server) by hand. A
  call still open on a server that is gone is never repaired.
- One webhook credential. Every media server's webhooks are verified with the one
  `calls.media.webhook` key pair, so a second server with webhook keys of its own needs
  the engine to find the credential by the webhook's issuer first.
- A removed person who clears their session and comes back through the call's link takes
  a new seat: nothing identifies a person across sessions yet.
- A waiter held behind the lock keeps no record of its polls, so one who closed the page
  stays in the host's roster until the call ends or a host removes them, and a waiter
  whose place ran out is listed in its old place until the place is marked expired.
- A waiter a host has just admitted holds no place in line until its next poll, so a call
  whose last person leaves in that moment ends with that waiter still asking.
- A screen share's event that is worked after a newer share began, but before the newer
  one's own event arrives, stops both shares, and the newer sharer starts again.
- Events are ordered by the media server's clock against the engine's, so a skew between
  the two moves which events count as newer; the reconcile pass repairs what that misses.
- A participant's traffic is capped only as far as LiveKit's configuration (at most 10
  video tracks received at once) and the page's own publish settings go: there is no
  per-participant bitrate limit.
- Media over UDP uses one port. LiveKit can spread it over a range of ports sized to the
  server's cores; the load test decides whether that is needed.
- The provision script creates and answers servers; it does not delete them or change
  their type.
- What runs on a real Hetzner server (the first boot, the certificates, TURN through
  Caddy) is the layout LiveKit documents, with configuration both programs load, and is
  first exercised in full by an installation's first call.
- Every join, poll and leave of the calls on one media server is decided under that
  server's one lock, whichever replica takes it, so the server admits as fast as one lock
  serves; no ceiling has been measured. In each process calls decisions hold at most two
  of its four write-gate connections and the rest wait in the process, so a burst cannot
  take the connections the process's other features need, and a decision that waits more
  than six seconds answers `502` `failed`.
- The capacity count reads a media server's open rooms and the rooms open on it within
  the last day (a room's superseded versions are deleted after a day). A key's own reads,
  its open rooms when it starts a call and its bookings when it books or moves one, still
  read every room and booking the key ever had; part 1's operator-minted keys keep them
  small.

## Checking it end to end

`TestTwoPeopleHoldACallThroughTheFrontDoor` (`test/clustere2e/calls_live_test.go`) holds a
call on the local two-replica cluster and its media server. It writes a deployable, mints a
test key, switches calls on and checks that every edge replica has the switch within 10
seconds, unlocks and starts through the front door, joins a guest by the call's link,
connects both to the media server, checks that each receives the other's microphone within
10 seconds and that both seats read `in_call` on every bff replica, and ends the call. It
runs only with its build tag, never in `make test`, and skips, naming what to start, without
the cluster, the media server, a cluster owner's token or the cluster's database:

```bash
make up SERVERS=2 && make scale N=2       # make up starts the media server last
kubectl port-forward -n memql svc/bff 50051:50051 &
kubectl port-forward -n memql svc/memql-db-rw 55433:5432 &
MEMQL_E2E_TOKEN=<a cluster owner's JWT> \
MEMQL_E2E_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55433/memql?sslmode=disable' \
  go test -tags clustere2e -count=1 -timeout=600s ./test/clustere2e/ \
  -run TestTwoPeopleHoldACallThroughTheFrontDoor -v
```

The machine it runs on has to trust the mkcert root (`mkcert -install`), since the media
server's client trusts the system's roots alone.

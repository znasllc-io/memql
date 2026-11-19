---
title: Calls
audience: public
status: stable
area: operate
sinceVersion: 0.31.40
owner: znas
description: Running the media servers calls take place on, from creating a server to rotating its keys and removing it.
---

# Calls

A deployable with its calls switch on holds video calls (see
[Calls on a deployable](site-hosting.md#calls-on-a-deployable)). A call's video and
sound travel between each participant's browser and a media server, never through
MemQL: MemQL decides who may take a seat, hands each participant a short-lived pass,
drives the server through its control API, and hears what happens on it through the
server's webhooks (the `livekit` source of [inbound delivery](inbound-delivery.md)).

The media servers are outside the cluster in every installation. Locally that is three
containers beside k3d (`make media-up`, in
[Reproduce the cloud locally](reproduce-the-cloud-locally.md#the-calls-media-server)).
For a cloud installation it is a rented server, described below.

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
answers behind that name is TURN, so the handshake is the whole check). If it reports
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
with `live-restore`, so a restart of Docker itself, which Ubuntu's unattended security
updates make when they update it, leaves the containers and their calls running. A
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

### Known limits

- One media server per installation, and calls are not moved between servers.
- Media over UDP uses one port. LiveKit can spread it over a range of ports sized to the
  server's cores; the load test decides whether that is needed.
- The provision script creates and answers servers; it does not delete them or change
  their type.
- What runs on a real Hetzner server (the first boot, the certificates, TURN through
  Caddy) is the layout LiveKit documents, with configuration both programs load, and is
  first exercised in full by an installation's first call.

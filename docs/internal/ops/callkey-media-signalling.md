---
title: Callkey media signalling admission
audience: ops
status: draft
area: ops
sinceVersion: unreleased
owner: znas
---

# Callkey media signalling admission

The fixed signalling relay admits `/rtc`, `/rtc/validate`, `/rtc/v1` and
`/rtc/v1/validate` through native Callkey authority. It is not a general HTTP
proxy. Its upstream comes from the room's registered media server. Media packets
still travel directly between the participant and LiveKit.

Validation uses `CheckMediaSignal`, which never registers or replaces a signalling
intent. Success says native authority is current, not that the provider is ready.
Validating an already-connected pass cannot evict its socket.

The native pass contains a second, pepper-HMAC-signed admission attribute. This
binds the room, identity, seat generation, native session, credential, and exact
delegated approval. A processing pass instead binds the exact consented native
worker lease. LiveKit token refresh may extend its own JWT but cannot alter the
native proof. Old passes without this binding must obtain a fresh native pass.

`integrations/calls/media_signal_auth.go` checks current native state on every
connection and every upstream write. Room, account and delegation gates serialize
bounded writes with the same gates used by revocation. No gate spans a socket's
lifetime. Callbacks are in-process capabilities, not public authorization RPCs.
An upstream write has a five-second bound; the transport must honour its context.

Each gated attempt reads room seats and agent admissions concurrently, at most two
named queries at a time, on ordinary request connections. Both reads finish before
the gate can be released, including on cancellation or panic. The sorted gate set
is still read before and after acquisition; a membership change retries under the
new set. Only the full post-acquisition seat rows enter the callback. Forward and
reconciliation can use those rows for departure and exact-seat predicates while
the same room/member gates stay held: neither callback writes seats. They must not
reuse them after a seat mutation or in another callback/frame. Session, credential,
family, delegation and time predicates remain fresh, including after provider I/O;
no allowed verdict is cached. This removes duplicate serialized reads without
extending the frame deadline or weakening revocation ordering.

`media_signal_authority_db_test.go` exercises real named DB reads with injected
latency, same-room Forward contention and reconciliation contention. It also
checks a departure committed while Forward waits for its gate, later-frame leave
denial, and session expiry during provider inventory. These controlled regressions
do not establish production transport capacity; served initial/rejoin and actual
production media acceptance remain separate qualification steps.

Before forwarding, Begin persists a pending intent in `mediaSignalState`. A
definitive upstream success must be confirmed before its initial response is
released to the client. The pending handshake has 20 seconds from intent creation,
after admission reads and gate waits. Its creation and expiry share one timestamp,
so slow admission cannot persist an already-invalid journal entry. Confirmed sockets
have a 30-second lease, renewed by a native check at least every ten seconds.
Current native authority remains required independently of either lease.

SDK reconnect can overlap its old socket. Begin atomically replaces only a
confirmed intent for the identical native binding. The old writer is fenced by
its missing intent ID, and its eventual Close cannot drain the new connection.
An unconfirmed or uncertain handshake cannot be replaced this way.

Native revocation commits denial first. Existing mutation success responses mean
the authority change committed; they do not claim a media cutoff at that instant.
Closing a WebSocket does not prove that established SRTP stopped. Native media
reconciliation removes revoked/expired participants and reads provider inventory
back. `SeatCall.media_cleanup` remains pending when a handshake is unresolved,
provider cleanup fails, or provider readback is unavailable. Its read-only
projection requires a recent successful provider audit and current authority.

An EOF or timeout after sending an upstream request is ambiguous. Its intent
survives replica changes and process restart as uncertain. Expiry never silently
turns uncertainty into cleanup success. The bounded journal holds at most 128
intents per room; saturation refuses admission. A definitive late response can
settle the intent but cannot restore an expired or revoked connection. While an
intent is uncertain, reconciliation can remove an observed participant, but does
not mistake one empty readback for proof that the original handshake cannot land
later. A provider-generation reset recovery operation is not implemented; do not
delete these records or describe an unobserved restart as confirmed cleanup.
Superseded journal row versions expire after one hour with `keepLatest=true`;
the latest unresolved barrier remains durable, rather than accumulating an
unbounded history of heartbeat versions.

During rollout, currently authorized legacy participants with no journal may
remain until reconnect. Their status is `legacy_media_unverified`, never clear.
An identity with no current native admission is removed even when no intent
record names it. Managed ingress is exempt only through its exact active native
resource lease, owning credential and delegated resource authority; there is no
identity-prefix exemption. Its private ingress network and revocable stream key
remain separate from browser signalling. A resource's device credentials never
enter this relay.

The public listener must reject other LiveKit routes, including direct WHIP and
administrative services. The formerly raw HTTP listener must expose only fixed
control operations and separately authenticated internal signalling. The internal
guard is SHA-256 of `memql-media-signal-v1:` followed by the media API secret.
It is service-to-service authorization, not a browser credential. Neither a
valid LiveKit user JWT nor the guard alone is a public native admission.

`MediaPermissionBackend.UpdatePermissions` supplies explicit publication
permissions and provider readback. It refuses unrestricted empty source lists
and treats mismatched readback as pending enforcement. Permission updates never
automatically activate a device.

## Presentation ownership and audience permissions

`presentation_set`, `presentation_clear` and `presentation_status` use the shared
human/agent Workflow boundary. A mutation requires the current host, exact
run/visit/revision/control epoch and an idempotency key. Delegated mutation also
requires both `skills_control` and `control`, the approved skill version and room,
and no human control hold. Set requires an active presentation step and Insights;
clear can restore this workflow's restrictions after the run or entitlement ends.
Family/children's Skills remain explicitly unavailable.

The policy binds its presenter and optional temporary speaker to the exact native
seat identity and joined-at generation. The audience has independent microphone,
camera and screen-share permissions; screen-share audio follows screen-share.
A temporary speaker receives only the microphone permission already supported by
its native kind, for at most 300 seconds. Presenter assignment never broadens an
avatar to screen publishing. Public status names presenter/speaker ownership;
only a host with current control authority receives eligible choices.

Each signalling intent pins its native grant digest. A pending handshake for an
older policy cannot be forwarded after narrowing. An already-sent handshake must
be fenced, removed and confirmed absent before it can be settled; its first
response is not released. Established connections with narrowing permissions are
also fenced and disconnected. This deliberate interruption avoids relying on
LiveKit's permission-update ordering for a previously acknowledged, delayed track.
The client can obtain a fresh native pass and reconnect with devices off.
Widening may use permission update/readback on the existing connection, without
starting devices. Old/refreshed JWT claims never override the current desired
grant; reconnect receives a newly minted restricted provider JWT.

Presentation changes retain pending enforcement until narrowed participants are
confirmed absent, other current permissions are read back, and no unresolved
handshake can land with older permissions. Every
provider confirmation is bound to participant SID and grant digest. Failures stay
pending, including restoration after cancellation, completion, a new visit,
Insights/authority loss or presenter departure. Restoration recomputes current
native grants; it never replays a stale broad permission snapshot, unmutes a track
or starts a device. A temporary-speaker expiry is checked by every guarded media
operation and by the durable thirty-second workflow lifecycle pass, independently
of any open UI. Native expiry and queued enforcement are distinct from provider
readback and a confirmed media cutoff; the expiry timestamp is not an SRTP cutoff
guarantee.

Managed ingress retains its own owner credential and native lease. Assignment may
select its existing native seat but cannot start a device or transfer ownership.
Before a presentation is set, every active ingress lease in the room must be that
exact selected lease; otherwise `presentation_stop_other_resources` asks for an
explicit stop first. Preparing and revoking leases count until DeleteIngress is
confirmed, including leases whose seats already departed. A live policy refuses
new competing resource starts/switches. Existing selected-lease retries remain
idempotent. This release does not provide an atomic combined stop-and-handoff.

Regression evidence is in `media_signal_test.go`, `media_signal_db_test.go` and
`media_signal_epoch_db_test.go`: delayed admission preserves a readable pending
journal and the exact 20-second forwarding bound; signed binding tampering, stale
session/credential/grant and consented worker authority, managed resource leases,
overlapping resume, in-flight revocation,
late handshakes, replica recovery, journal bounds, provider failures and native
permission readback. Transport bypass closure and actual LiveKit/browser
acceptance are separate checks; native tests alone do not establish those facts.

Processing admission binds the exact current consented worker lease. The existing
sampler provider grant permits room subscription; the trusted processing worker
selects the lease's approved tracks. This signalling boundary does not provide
cryptographic isolation of individual subscription targets for a custom sampler
client holding that provider JWT.

# Callkey media signalling admission

The fixed signalling relay admits `/rtc`, `/rtc/validate`, `/rtc/v1` and
`/rtc/v1/validate` through native Callkey authority. It is not a general HTTP
proxy. Its upstream comes from the room's registered media server. Media packets
still travel directly between the participant and LiveKit.

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

Before forwarding, Begin persists a pending intent in `mediaSignalState`. A
definitive upstream success must be confirmed before its initial response is
released to the client. The initial handshake has 20 seconds. Confirmed sockets
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

`MediaPermissionBackend.UpdatePermissions` adds the explicit provider permission
and readback seam for presenter/audience enforcement. It refuses unrestricted
empty source lists and treats mismatched readback as pending enforcement. This
seam alone does not enable a presentation policy or automatically activate any
device.

Regression evidence is in `media_signal_test.go` and `media_signal_db_test.go`:
signed binding tampering, stale session/credential/grant and consented worker
authority, managed resource leases, overlapping resume, in-flight revocation,
late handshakes, replica recovery, journal bounds, provider failures and native
permission readback. Transport bypass closure and actual LiveKit/browser
acceptance are separate checks; native tests alone do not establish those facts.

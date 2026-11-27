---
title: Callkey 0.32.1 release preparation
audience: ops
status: draft
area: ops
sinceVersion: 0.32.1
owner: znas
---

# Callkey 0.32.1 release preparation

This patch fixes creation of a pending media signalling intent after slow native
admission and adds bounded diagnostics for the remaining production connection
failure. It is release preparation, not a publication or deployment receipt.
Cut and publish the exact verified source through the
[native release workflow](../../public/operate/release-cutting.md), then verify
the signed artifacts before selecting the installation update.

Previously, Begin calculated expiry before the final gated admission reads but
recorded creation afterward. An admission delay of 20 seconds or more could
therefore persist an intent whose expiry was not after its creation. The journal
reader correctly refused that inconsistent state, preventing the subsequent
signalling handshake.

Creation and expiry now use one timestamp immediately before the durable pending
intent is appended. Its handshake lifetime remains 20 seconds. Forward and
Confirm still require current native authority, the exact binding and current
grant. The save must succeed before any upstream connection is attempted. This
patch neither deletes uncertain intents nor repairs an already-invalid journal.
See the [signalling contract](callkey-media-signalling.md).

The real-database regression
`TestMediaSignalDatabaseHandshakeEpochStartsAtPendingIntentCreation` advances the
admission clock by 20 and 21 seconds. Both cases fail before the fix because the
persisted journal is unreadable. With the fix, they prove a readable pending
intent, exactly 20 seconds of validity from creation, forwarding before expiry,
refusal at expiry and definitive cleanup. The focused `TestMediaSignalDatabase`
suite passed all 16 top-level tests (23 named cases) against an isolated local
database with a fake media provider. These checks do not prove a production
media connection or successful TURN allocation.

The existing IPv4 TURN setup now explicitly requires A-only DNS for its separate
TURN hostname. Production's advertised IPv6 path completed TLS but its
credential-bearing allocation received an unsigned 508; IPv4 allocation and its authenticated deletion
succeeded. Removing only the TURN AAAA record preserved the other three DNS
records. Authoritative and recursive readbacks passed after its TTL. The next
browser attempt acquired a relay candidate and initially connected the host, but
the guest still failed during signalling. This establishes progress, not a
completed media journey. Its native journal had no invalid intent epochs, so the
epoch repair does not explain that later failure.

The connection diagnostics retain one bounded terminal receipt with fixed phase
names, counts, durations and closed failure classes. They record the first failure
separately from later cleanup errors. They never include identities, tokens, URLs,
queries, SDP, frame bodies or raw error text. Passive socket waits and ordinary
socket closure do not become slow-operation or failure reports. No diagnostic
endpoint is added, and neither authority checks nor media deadlines change.
The actual source must pass its privacy and delayed-read/gate tests before release;
the metadata is for distinguishing a gate/read delay, protocol refusal and
transport failure, not for declaring an unobserved cause.

The native repository-gate step also has a 45-minute aggregate bound. The earlier
identical gate passed in 851 seconds, while a later run hit its 30-minute outer
deadline after several packages passed. Its incomplete output establishes neither
CPU contention nor a hung test. Each package retains its 600-second bound, and
the step's command, memory and CPU settings are unchanged. The final native run
must succeed; a larger bound alone is not verification.

No database migration, public API shape, UTCP version or SDK version changes are
required by these changes. Review the final source delta, preserve every failed
acceptance attempt, and qualify the deployed engine and media host together before
announcing readiness.

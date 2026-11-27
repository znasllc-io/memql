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
admission. It is release preparation, not a publication or deployment receipt.
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

No database migration, public API shape, UTCP version or SDK version changes are
required by this repair. The separate production TURN allocation failure must
be diagnosed and qualified independently; this epoch defect alone does not
establish the cause of that failure. If provisioning changes join the release,
record their own validation and review the final source delta before cutting it.

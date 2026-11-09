---
title: Guest access lets anyone open a deployable's gate without a code, once an operator turns it on
audience: internal
status: approved
area: operate
sinceVersion: 0.31.26
owner: znas
---

# Deployable guest access

Owner brief, 2026-11-08. The first deployable to use access codes, the Operations
Desk on the owner's instance (memql-znas `clients/kraken-operations`), becomes a
public example from the MemQL team. The owner wants anyone to be able to explore it
without a code, "just the same as the ones with access code", while code holders
keep access that runs out, which is there "to showcase the security thing". The
engine admits nobody without a code today, on purpose (the access-code record,
section 2), so this needs the engine.

This record amends [2026-10-29 deployable access codes](2026-10-29-deployable-access-codes-design.md),
called "the access-code record" below. Everything there holds unless a section here
says otherwise.

**Approvals.** The owner, in chat, on 2026-11-08: chose guest access in the engine
over a preview built into the page that could not run a live check; approved this
design ("Looks right"), including one new route on the visitor protocol, an HTTP
exception under the gRPC-first policy like the four before it; and approved the
rollout ("Yes, do all of it"). A code built into the page was rejected: it puts a
working credential in the page's public JavaScript, which undercuts what the codes
are there to show.

## 1. What the owner asked for

| # | Requirement, in the owner's terms |
| --- | --- |
| G1 | Anyone can open the deployable without a code, and explore it the same as somebody with one. |
| G2 | Code holders keep their codes as they are: access that ends, and being taken out the moment it does. |
| G3 | Managed in MemQL OS, beside the codes, and turned off as easily as on. |
| G4 | Generic, like the codes: any deployable, any gate. Moving a product onto it is that product's own work. |

## 2. Shape of the solution

**A guest pass is a code that nobody holds.** It is a code's row, marked `guest`,
with no secret at all: its digest is of a string no code can produce. A new route
opens a session on it for anybody who asks, with no code. From there it is a code's session
in every respect: the same row, cookie, 24-hour cap and checks on every request.
Removing the pass, or letting its end date pass, takes every guest out at once by
the access-code record's section 8, unchanged.

**Why the access-code record's argument still holds.** Its section 2 rests on reach
coming from an operator, not from DSL: "the annotation gives a code something to
open; it never creates reach on its own." A guest pass keeps that. Only a person
holding `execute app:deployables/access` creates one, in Deployables, for one gate of
one deployable, with an end date, and removes it the same way. DSL still opens
nothing to the internet. What changes is the population: an operator who allows
guests has chosen the internet, for that gate, until they say otherwise.

**Why a code row and not a new concept.** Everything that ends access already works
on a code row: expiry to the instant, removal, the organization check of D15, the
audit log, the 30-day retention of sessions and "last opened". A product's own rows
are stamped with `accessCodeId`, so to a product all guests together are one more
code, and its limits and partitions need no change.

## 3. Decisions

| # | Decision | Why |
| --- | --- | --- |
| G-D1 | Guest access is an operator act and never DSL. A deployable with no guest pass answers the guest route `closed`. | Keeps the access-code record's section 2 true: no annotation creates reach. |
| G-D2 | A guest pass is a `siteAccessCode` row with `guest: true` and no secret. Its `codeHash` is SHA-256 of `guest:` and the site's bare id, a string that no input to unlock normalizes to, since unlock hashes only 25 symbols of the code alphabet. | Reuses every mechanism that ends access and leaves nothing to leak or type. Unlock can never find the row, and the guest route finds it through the existing digest index, so no index has to be built: a new one scans the whole node table on deploy (the indexes migration of 2026-10-30 says what that costs). |
| G-D3 | A deployable has at most one guest pass that is not removed. Regenerate refuses a guest pass. | One switch to reason about. A secret nobody holds has nothing to rotate. |
| G-D4 | A guest pass expires, like every code (D8), with the same presets and the same 366-day cap, and its expiry can be changed later. | An operator who forgets a public door still has it close. |
| G-D5 | A guest session is a code session: one `siteAccessSession` row whose `codeId` is the pass, the same cookie, the 24-hour cap of D10 and the checks of D11. | Turning guest access off reaches open tabs exactly as removing a code does. |
| G-D6 | A call from a guest is stamped as a code's is: `accessCodeId` is the pass, `accessCodeLabel` its name on a mutation, `accessGate` and `accessSubject` its own. No new reserved argument. | To a product, all guests are one code: it shares their limits and their partition without a change. A product that must tell guests apart is section 13. |
| G-D7 | Opening a guest session costs one read through the digest index before anything else, and the edge limits it in buckets of its own: 10 a minute per (site, address) and 60 a minute per site, on each replica. | A guest open has no secret to check first, so "a stranger costs no read" cannot hold for this route. The buckets bound what it costs, and keeping them apart from unlock's stops a crowd of guests spending the budget a code holder's unlock draws on. |
| G-D8 | The unlock, session and guest replies carry `guest`, `true` for a guest's session and `false` for a code's. | So a page can say "Guest" instead of counting down to an end its visitor never chose. |

## 4. Data

`siteAccessCode` gains `guest bool`, read as `false` when empty, written only by the
mint. A guest pass's `codeHash` is the digest of G-D2, the same for every pass a site
ever has, so the rows of a site's earlier, removed passes share it, and the guest
route takes the one that is not removed. It carries no hint, since there is no code
to tell apart. No session field changes. The concept snapshot and both SDKs are
regenerated.

## 5. Operator surface

**DSL.** In `dsl/platform`, beside the access-code record's section 5:

| Construct | Purpose |
| --- | --- |
| `builtin siteAccessGuestPassCreate(siteId, label, gate, subject?, expiresAt)` | Mints a guest pass. Returns `{codeId, expiresAt}`, never a code. Refused while the site has a guest pass not removed, with the sentence "This deployable already lets guests in. Change when guest access ends, or turn it off first.", and with every refusal a code's mint has. Refused inside a run. |
| `builtin siteAccessCodeRegenerate(codeId, expiresAt?)` | Refuses a guest pass: "Guest access has no code to regenerate. Change when it ends, or turn it off." |
| `builtin siteAccessCodeSetExpiry(codeId, expiresAt)`, `builtin siteAccessCodeRemove(codeId)` | How an operator changes or ends guest access. Removal is unchanged and a run may do it. Re-timing a guest pass is refused inside a run, since keeping a deployable open to anybody is an operator's decision. |
| `builtin siteAccessCodesForSite(siteId)` | Each row gains `guest`. |

This package refuses by sentence and has no reason codes. A run may not create or re-time a guest pass, and may remove one, because ending guest access only narrows who can get in.

**Authority** is the code mint's: `@requiresCapability("execute",
"app:deployables/access")`, with the site as the organization target in
`organization_action_capabilities.go`. **Audit:** the mint writes
`v1:identity:auditEvent` with `targetType: siteAccessCode` and says it is a guest
pass. Regenerate, expiry and removal write theirs as they do now.

**MemQL OS.** In the Access codes dialog, a Guest access part above the codes,
following `clients/os/DESIGN.md`: kit pieces only, status never colour alone, and an
act that is not legal is absent rather than disabled.

- **Off:** "Off. Only someone with a code can open it." and an act to allow guests,
  absent when the deployable declares no gate.
- **Turning it on** is a code's add form without the code: Name (prefilled
  "Guests"), Opens (the deployable's gates), Subject only when the chosen gate's
  constructs use `accessSubject`, and Ends (the same presets and Custom).
- **On:** what it opens, when it ends ("Ends in 12 days", the exact time behind it,
  "Ended" once past), "Last opened 2 minutes ago" or "Not opened yet", and two
  acts: Change end date, and Turn off, which confirms in place: "Everyone in as a
  guest is taken out now."
- **Never a code.** No step shows one, and the code list leaves the guest pass out.

## 6. Visitor protocol

One row joins the access-code record's table:

| On the site's origin | bff route | Reply |
| --- | --- | --- |
| `POST /_memql/access/guest`, body `{}` | `POST /access/guest` | `200 {"gate","subject","expiresAt","remainingMs","guest":true}` plus the cookie. `403 {"state":"closed"}`. `400 {"error":"invalid_body"}`. `429` with `Retry-After`. |

The other replies that describe a session, unlock's `200` and the session check's
`200`, gain `"guest"`.

**The edge** answers five method and path pairs, exact as before. The guest route
has unlock's content type and `Origin` checks and its 1 KiB cap, and draws on the
buckets of G-D7. The stamp, the header strip and the cookie handling are unchanged.

**The bff**, in the order of what it costs:

- **Missing stamps:** the one `404`, with no read.
- **A body that is not a JSON object:** `400`, with no read. Its fields are ignored.
- **The site's guest pass,** one read through the existing index on `codeHash`, by
  the digest of G-D2. None, one that is removed, and one past its end date are
  `closed`, with no site read.
- **Then the site, read as the cluster** as unlock does after a digest hit, and the
  stamp compared with its owner. A pass whose copied organization differs from the
  site's is `closed`. A stamp that does not check out is the one `404`, with a `WARN`.
- **Then the session:** mint a token, write the row with the pass's generation, set
  the cookie, answer `200`.

Every reason guest access is closed is the same `403 closed`. The route is public,
so the reply is no oracle about anything a visitor could not learn by asking again.

## 7. Ending in real time

The access-code record's section 8 applies unchanged. Turning guest access off is a
removal, which a run may also do, refused at the server at once and reaching an idle tab within 5 seconds. An
end date is exact to the timer.

**The client half gains one obligation.** When a guest's session ends, the page
asks the guest route once. A `200` is a new session and the visit goes on. `closed`
means guest access has ended, and the page leaves. A guest's session most often ends
at its 24-hour cap, so this keeps a long visit unbroken without letting it outlive
guest access.

## 8. Threats

Added to the access-code record's section 9:

| Threat | Answer |
| --- | --- |
| A stranger opening guest sessions in bulk | The buckets of G-D7, a session written only after the pass and the site check out, and the 30-day retention on sessions. Each open costs one indexed read, one point read and one write. |
| A crowd of guests locking code holders out | Guest opens draw on buckets of their own, and a code's unlock never draws on them. |
| Guests spending a product's per-code budget | By design all guests are one code, so they share its limits. Code holders keep their own. |
| A guest pass's secret | There is none. Its digest is of `guest:` and the site's id, which no code normalizes to, so no typed code opens it, and regenerate refuses it. Knowing the digest opens nothing: the guest route is the way in, and it is open to everyone already. |
| A cross-site page opening a guest session | `SameSite=Strict`, a JSON-only POST and the `Origin` check, as for unlock. It would only hand the visitor what the operator gave everyone. |
| What a guest can reach | One gate's declared constructs, exactly what a code for that gate reaches. |

**Known limitation.** All guests share one partition. Whatever a product writes for
one guest, stamped with the pass, is readable by every guest through the gate's
queries. A product must keep anything personal out of a gate it opens to guests, and
the public docs say so.

## 9. Observability

`memql_site_access_requests_total` counts the guest route as `op="guest"`. The
pass's "last opened" answers whether guests are coming. Operator acts are in the
audit log.

## 10. Testing

**Go.**

- Mint: the reply carries no code, a second guest pass is refused while one stands,
  regenerate refuses a guest pass, and the capability and organization refusals.
- Guest open: open, none, removed, past its end, another organization's copy, a site
  with no owner, and a body that is not an object.
- The session reply's `guest` on all three routes, the 24-hour cap, and a removal
  taking an open guest session to `ended`.
- Call: stamping identical to a code's, the pass's label on a mutation only.
- Edge: the five pairs and nothing else, the caps, `Origin`, content type, and both
  guest buckets apart from unlock's.
- Surface gates: `AssertUnauthenticatedSurfaceDeclared`, the front-door generator,
  `test/dslconformance` and the OS part parity gate.

**OS.** Vitest with the deployables harness: off, the form, on, Change end date,
Turn off's confirmation, the guest pass left out of the code list, and no code shown
at any step. Rendered review through the QA harness in both modes, plus a narrow
capture.

**End to end.** On production, as a guest: open, call, and watch Turn off take an
open tab out within 5 seconds.

## 11. Documentation

`docs/public/operate/site-access-codes.md` gains a Guest access section: the route,
the client's obligation and the shared partition. Updated: `CLAUDE.md`'s Allowed HTTP
Exceptions row (five pairs and the guest route), `docs/public/operate/deployables.md`,
and `clients/os/README.md` where it describes the panel.

## 12. Rollout

1. This change lands on `main` and ships in the next engine release, 0.31.26, cut
   by hand.
2. The instance pins that release.
3. The product ships its guest entry.
4. An operator turns guest access on in the OS, and the end-to-end check in section
   10 runs on production.

A page that asks the guest route of an older engine gets no session, so the product
ships after the pin.

## 13. Not in this version

- **`accessGuest`,** a reserved argument telling a construct that its caller is a
  guest, for per-guest limits or wording. Added when a product asks for it.
- More than one guest pass on a deployable.
- Per-guest identity, which would undo the shared partition.

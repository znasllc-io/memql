# `memql access` — what this cluster says you are

One command, one question: **what am I on this cluster, and why was I just
refused?**

```
$ memql access acme
Your access on "acme"

  Signed in as    Ada Lovelace <ada@example.com>
  User id         u_01JQ8ZK
  Session         sess_01JQ90F  (this device)

  Role            Release Manager
                  release-manager · rank 150
  Groups          Platform (team) · g_1 — Acme Corp
                  Release Captains (role-group) · g_2 — Acme Corp
  Account scope   acct_9
```

The cockpit half of the access program's roles-as-data epic
(memql-cockpit#403; engine epic memql#5166). The design record lives in the
**engine** repository:
`docs/superpowers/specs/2026-09-07-roles-as-data-design.md`, section G, with
the program index beside it at `2026-09-07-access-program.md`. This repository
has no separate record.

---

## Why a command and not a portal page

The portal renders what the cluster believes about a **browser session**. A
person hitting a refusal *from this machine* needs what the cluster believes
about **this credential** — and those differ exactly when it matters: a stale
token, a PAT with a different ceiling, a second account, a machine somebody
else signed in on months ago.

**It asks rather than decoding the bearer.** The cockpit does not parse its own
tokens — the standing client rule that `session_id` and `display_name` exist on
the wire to serve. A claim is what was true when the token was minted; a role
changed since is the whole thing somebody running this command is trying to
find out.

---

## The role is a slug, a name and a rank

Not one of five enum values. A **custom role is none of the five**, and its
**rank** is what says where it stands in the ladder — so the cockpit prints all
three and compares the role to nothing.

```
  Role            Release Manager           <- role_name
                  release-manager · rank 150
                  ^ role (the catalog slug)   ^ rank
```

The slug is repeated under the name because the slug is what every grant,
invitation and delegation ceiling is written in terms of. Group lines carry
their **id** for the same reason — it is what you quote to whoever administers
grants, and the only thing that tells two same-named groups apart.

### Rank 0 gets a sentence of its own

```
  Role            retired-role
                  rank 0 — this role holds no permissions on this cluster
```

Rank 0 is the record's **unknown slug**: the resolver treats the holder as
holding nothing, everywhere, until they are re-roled. It is the documented
consequence of the cluster owner's direct-write escape (a role row deactivated
under a holder), and the single fact that explains every refusal the reader is
about to hit — so a bare `rank 0` would bury it.

A rank is only ever read **alongside a slug**. On its own it says nothing: an
`int32` of 0 puts no bytes on a proto3 wire, so "rank 0" and "no rank sent" are
the same silence. Attached to a role that did arrive, it stops being ambiguous.

---

## Two absences, and they are not the same fact

Every line here has a state where nothing came back, and there are **two** of
them:

| Rendered | Means |
|---|---|
| `not reported by this cluster` | This build's wire has no such field. No cluster could have sent one; the engine change is named at the bottom of the report. |
| `none reported` | This build understands the field and the cluster sent nothing in it — an older node, or a credential with no row behind it. |

Only the first is explained by a pending engine issue. Printing that
explanation under the second would tell somebody to wait for a change their
cluster already has.

**Presence comes from the response, never from this binary.** The descriptor is
compiled into the cockpit, so once the pin moves past memql#5181 *every* build
carries a `role` field — and a check that only asked "does the field exist?"
would report every answer as reported, including from a node one release
behind. A cluster that said nothing would render as a person who holds nothing
everywhere, which is the inversion this whole surface exists to prevent.

**An absence is a sentence, never a blank.** A blank or a dash reads as the
*opposite* claim — "you hold no role" instead of "this cluster did not say" —
and the first sends somebody to ask for a grant they already have. It is the
rule `probe.Figure` keeps for a measurement that could not be taken.

### What proto3 cannot tell apart, the cockpit does not claim to

An empty repeated field and an absent one are **the same bytes**; so are a
`false` bool and an unsent one. That is the same limitation the worker's
`apps_present` flag exists for. So "in no groups" and "sent no groups" collapse
here — and they collapse toward the safe reading, because a person told *not
reported* looks further, where one told *none* believes they hold nothing.

---

## Today: the cluster does not report it yet

```
  Role            not reported by this cluster
  Groups          not reported by this cluster
  Account scope   not reported by this cluster

This cluster does not report roles as data yet. The engine change that adds
the role slug, its name and its rank to MyAccess is memql#5181; until that
lands there is nothing here to show. The cockpit does NOT fall back to the
retired five-value enum: a custom role is none of those five, so a guess
would be wrong in exactly the case this command exists for.

Group membership and account scope arrive with memql#5165.
```

**This is the expected state at the current pin**, and it is not a failure.
memql#5181 (role) and memql#5165 (groups, scope) are separate engine PRs
editing one message, so the note names **only** the blocks actually missing — a
report telling you group membership has not arrived, three rows under a list of
your groups, is worth nothing.

**No cockpit code changes when they land** — but the binary does have to be
rebuilt at a pin that carries them. The generated descriptor is compiled in, so
an installed `memql` keeps reporting what its own wire knows about. What is
avoided is a *code* change: no field mapping to write, no release to plan
around, no numbers to chase.

---

## By name, never by number

Both design records that add fields to `MyAccessResult` settle the **names**
and hand the **numbers** to the engine implementer:

> Field numbers are chosen by the implementer against the current message; the
> names are the contract.
> — *groups-and-grants*, on the wire

> gains `string role = 11`, `string role_name = 12`, `int32 rank = 13` (numbers
> chosen against the message by the implementer …)
> — *roles-as-data*, section G

A number written into the cockpit is a **guess the engine is free to
contradict**, and it fails in the worst way: whatever the landed message puts at
field 11 renders as somebody's role.

It also rules out the obvious alternative. Proto keeps unrecognised fields as
`unknownFields` **bytes keyed by number only** — the name never travels on the
wire — so `role` cannot be pulled from an unknown-field blob without already
knowing the number the records decline to settle. Reading the **descriptor by
name** is the only correct option.

`internal/access` also checks **kind and cardinality**, so a future field
reusing one of these names for a different type reads as absent rather than
being coerced. `FuzzDecodeMatchesFieldNamesExactly` is the guard, and it builds
a probe of **each of the five kinds the contract uses** — string, int32, bool,
repeated string, repeated message. A string-only probe could assert nothing
positive about four of the six slots, and case-insensitive matching in their
lookups sailed straight past an earlier version of it for a million executions.

`future_wire_test.go` builds the message both records describe **at field
numbers deliberately not the ones they illustrate** (41/42/43, not 11/12/13)
and runs the real decode against it.

---

## Choosing the cluster

```
memql access                  # the selected cluster, or the only one
memql access acme             # by name
memql access local            # the built-in local cluster, always addressable
memql access acme --json      # flags work in any position
```

With no argument the command uses `selected_cluster` from
`~/.memql/clusters.yaml` (the same value the VS Code extension reads), or the
only registered cluster when there is exactly one. A stale `selected_cluster`
— `memql cluster remove` does not clear it — falls through to the only
remaining cluster rather than dead-ending.

**Several registered and none selected is an error, not a guess.** Reporting
somebody's access on a cluster they did not name is worse than asking, because
the answer looks exactly like the one they wanted — and being believed is the
entire job of this command.

Exit codes: `0` success, `1` the cluster could not be reached or refused, `2`
bad invocation.

---

## `--json`

```json
{
  "cluster": "acme",
  "user_id": "u_01JQ8ZK",
  "role": {
    "on_wire": true,
    "reported": true,
    "slug": "release-manager",
    "name": "Release Manager",
    "rank": 150
  },
  "groups": { "on_wire": true, "reported": true, "items": [] },
  "account_scope": {
    "on_wire": true, "reported": true,
    "account_ids": ["acct_9"], "every_account": false
  }
}
```

Every block that can be absent carries **both** flags the text report
distinguishes, and they answer different questions:

- **`on_wire`** — does this build's wire carry the field at all? `false` means
  no cluster could have sent one.
- **`reported`** — did a value actually arrive in *this* response?

A block's contents appear only when `reported` is true, so no caller ever sees
`"slug": ""` and has to guess which it means.

Three things are deliberately **not** `omitempty`:

- **`rank`** is a pointer, so an explicit `0` survives. `omitempty` on a plain
  integer would delete precisely the value a caller most needs to see.
- **`items`** and **`account_ids`** are always emitted as `[]`, never dropped
  and never `null`. A dropped key is a *third* shape meaning the opposite of the
  second, and `.groups.items[]` would die on it for the ordinary user who simply
  belongs to no groups.
- **`every_account`** is emitted only when a scope was reported. A `false` bool
  is wire-identical to silence, so emitting one for a cluster that never
  answered would hand an audit script a confident negative the cockpit invented.

---

## The credential

The **signed-in user's**, resolved interactively: a person typed this command,
so a browser sign-in is the right answer to an expired token rather than a
window nobody will open. (The worker's own paths do the opposite, deliberately —
see `EnsureValidTokenNonInteractive`.)

**The sign-in is not under the round-trip deadline.** Twenty seconds is a good
ceiling for "the cluster did not answer" and a terrible one for "the human has
not finished logging in".

**The round trip is bounded by a `select`, not by the context alone.** The SDK
opens its stream on `context.Background()` by design — the stream must outlive
the connect timeout — so a deadlined context passed into `Connect` does not
reach the stream open. Against a peer that completes a TCP handshake and then
says nothing (a firewall dropping packets, a wedged load balancer — exactly the
case this ceiling is for) the deadline would pass unnoticed and the command
would wait forever at a prompt somebody is sitting in front of.

A `mql_wkr_` worker token cannot be used here: it is admitted on WorkerService
and nowhere else. A PAT works, and shows an empty session — a PAT names no
session row, which the wire calls out as not an error:

```
  Session         none — this credential carries no session
```

---

## A refusal arrives as a successful response

`MyAccess` answers with a `QueryError` inside a perfectly good message rather
than a transport error, so a client that only checked the error return would
render an empty record — the same shape as the honest answer for somebody who
really does hold nothing. The refusal is printed verbatim, with its **code**
and the cluster's name:

```
ERROR: acme refused: access context not available (UNAUTHENTICATED)
```

The code is the difference between "something went wrong" and "your session was
revoked", which is the most likely reason to be running this command at all.

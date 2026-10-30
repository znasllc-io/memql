---
title: Access codes -- open part of a deployable only to people who hold a code
audience: public
status: stable
area: operate
sinceVersion: 0.31.10
owner: znas
---

# Access codes

A hosted site is public. The edge answers any hostname it can resolve to a live
deployable and hands the bytes of its bundle to whoever asks. An **access code**
is how a deployable keeps part of itself back: data and actions its own DSL
declares, which open only for a visitor who types a code an operator gave them.

A code is random (125 bits), shown to the operator once, stored only as a
digest, checked by the cluster on every request, and ends on a clock. Replacing
one takes everyone using the old one out. Codes are managed in the Deployables
app of MemQL OS, and the mechanism is generic: nothing in the engine knows what
a deployable's gates mean.

**Audience:** operators who hand codes out, authors who declare what a code
opens, and front-end authors who build the page that takes one.

Related: [Deployables](deployables.md) · [Site hosting](site-hosting.md) ·
[Packages and deployables](packages.md) ·
[Building a pack](../build/building-a-pack.md) ·
[Access model](auth/access-model.md)

Design record: `docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md`.

---

## What a code opens, and what it does not

A code opens **gated constructs**: the queries and mutations in a deployable's
own DSL that carry `@accessGate` for the gate the code was minted for. That is
the whole of its reach.

- **It opens** the constructs marked `@accessGate` for its gate, on its own
  deployable.
- **It does not open** any file in the bundle, any other gate, deployable or
  route, or any MemQL API. A session is no token, no actor and no stream.

**Nothing in a public bundle is protected.** The edge serves a bundle's bytes
to everyone who can resolve the hostname, with a code or without one, and a code
does not change that. Put nothing in the bundle that a visitor without a code
should not read -- not a document, not a name, not a price. What is private
lives in the graph and reaches the page as the answer to a gated call.

Gating a path of static files (a PDF, a video, a demo) is **not in this
version**. A file that must stay private does not belong in the bundle yet.

## A visit, start to finish

1. An operator adds a code for one gate of a deployable, optionally for one
   subject (one client, say), and gives the plain value to a person. The cluster
   keeps its SHA-256 and nothing else.
2. The visitor's page posts the code to `/_memql/access` on the deployable's own
   origin. The edge checks the shape of the request, spends a rate-limit token
   and forwards it, stamped, to the bff.
3. The bff finds the code by its digest and, if it is live, opens a **session**:
   a row in the graph, and an `HttpOnly` cookie holding a random token whose
   digest is in that row.
4. The page calls the constructs its gate declares through
   `/_memql/access/call`. Every request reads the session and its code from the
   database again. Nothing in the decision is cached.
5. The visit ends at the earliest of the code's expiry, 24 hours after the
   session opened, a regenerate, a removal, or the visitor leaving.

A session is honored by exactly one handler, for exactly one site. It
authenticates nobody to MemQL.

---

## Handing a code out

In MemQL OS, open **Deployables**, select a deployable, and choose the lock
after Traffic in the page head: **Access codes**. The control is *absent*, not
disabled, when you do not hold the part, when the deployable is one the cluster
manages itself, and when it was not deployed from a package, since only a
package's own deployable can declare a gate.

**The part is `execute` on `app:deployables/access`**, seeded on owner and
developer like `deploy`, `publish` and `preview`, and checked against the
deployable's own organization ([access-model.md](auth/access-model.md)).

The panel lists the codes in label order. Each row shows the name the operator
gave it, what it unlocks (`Unlocks engagement for acme`), the last four
symbols of the code so two can be told apart, when it expires ("Expires in 8
minutes", with the exact time behind it, and "Expired" once past) and when it
was last opened ("Last opened 2 minutes ago", or "Not opened yet"). A removed
code is hidden; its row stays in the graph as the record.

**Add** takes four things:

- **Name** -- the operator's label, 1 to 80 characters. A visitor never sees it.
- **Unlocks** -- one of the gates the deployable declares. With none declared
  the panel says so and explains `@accessGate`, and Add is absent.
- **Subject** -- offered only when the chosen gate's constructs read
  `accessSubject`. It says whose part of the app the code opens (`acme`), up to
  120 characters.
- **Expires** -- 10 minutes, 1 hour, 1 day, 7 days, 30 days, or a date. It must
  be more than a minute away and no more than 366 days. **Every code expires**;
  there is no "never", because the lifetime is the one honest bound on a bearer
  credential. A long engagement picks a far date and extends it later.

**The code appears once.** After Add or Regenerate the panel shows it with a
copy control and a notice that this is the only time it is shown. It is held in
the page's memory alone, and dropped when the next act starts. Lose it and the
answer is Regenerate, never recovery: the cluster does not have it.

| Act | What it does | What stops working |
|---|---|---|
| **Regenerate** | Replaces the secret and keeps the code's id, name, gate and subject, so rows a product stamped with the id stay attached across a rotation. Keeps the expiry unless given a new one; a code that has expired needs one. | The previous code, at once. Anyone using it is taken out now. |
| **Change expiry** | Moves when the code stops opening the site, without changing the code. Takes effect on the next request, in either direction: extending an expired code revives it. | A shortened expiry takes out sessions already open. |
| **Remove** | Ends the code. The row stays, so it still answers who could get in, and when. | Every session opened with it, at its next request. **Removal is final**: a removed code cannot be regenerated or re-timed, and a new code is added instead. |

Regenerate and Remove ask in place, under their own row, and say what stops
working.

### What Add refuses

Nothing is minted unless every one of these holds, and a refusal says what to
change:

- the deployable has an **owner**. A call runs under its owner's authority, so a
  deployable with no owner, or one the cluster manages itself, cannot have codes;
- it was deployed from a package, and that package **declares the gate** you
  chose. A source deployed before access codes existed has no list of the DSL
  domains it ships, so it declares none until it is deployed again
  ([below](#when-a-gate-answers-404));
- the name, the subject and the expiry are inside the bounds above.

A regenerate or a new expiry on a deployable that has since lost its owner is
refused too, since such a code opens nothing. **Only removal stays allowed
there.**

### A code is minted by a person

Add and Regenerate refuse **inside a run**. The plain code exists in the reply
and nowhere else, and a run's journal is durable and readable by the cluster, so
a pack that regenerates a client's code and emails it would have written it into
the journal. All four acts also need a person behind them, because the person is
recorded on the code and in the audit event: a synthetic actor, a connector, an
anonymous caller and the operator credential are refused. Tooling that holds
the operator key can read the list. It cannot end a code; a person signs in and
does that.

The acts are DSL builtins, and the panel is a client of them. Called by a person
from anywhere else, they pass the same part and the same organization gate:

| Builtin | Reply |
|---|---|
| `siteAccessCodeCreate(siteId, label, gate, subject?, expiresAt)` | `{codeId, code, expiresAt}` -- `code` appears here and nowhere else, ever |
| `siteAccessCodeRegenerate(codeId, expiresAt?)` | the same shape |
| `siteAccessCodeSetExpiry(codeId, expiresAt)` | `{codeId, expiresAt}` |
| `siteAccessCodeRemove(codeId)` | `{codeId}` |
| `siteAccessCodesForSite(siteId)` | every code of the site, ended ones included, with `lastOpenedAt` from the newest session of the code's current generation. Never the code or its digest |
| `siteAccessGates(siteId)` | `{gate, usesSubject, constructs[]}` for each gate the deployable declares |

Each act writes a `v1:identity:auditEvent` with `targetType: siteAccessCode`
and the action `site_access_code_created`, `site_access_code_regenerated`,
`site_access_code_expiry_changed` or `site_access_code_removed`, naming the
person. The code is never in it.

### Moving a deployable between organizations

A code copies its deployable's organization when it is minted, and the part is
checked against that copy. So a deployable's organization **cannot change while
it has a code that has not been removed** -- an expired one included, since a
new expiry revives it. The refusal says to remove the codes first. A code whose
copy differs from its deployable's (one a move racing a mint can leave) opens
nothing, and the panel offers only Remove for it.

### Where the rows live

Two concepts hold this, `v1:platform:siteAccessCode` and
`v1:platform:siteAccessSession`. Both are written only by the server, and **no
client reads them directly**: a raw read, `parentOf`, `asOf` and a subscription
are all refused, because every client path goes through a builtin that passes
the part and the organization gate. Neither publishes graph events, so no
automation or rule can journal one. Sessions are kept for 30 days, which is as
far back as "last opened" can see.

---

## Declaring what a code opens

`@accessGate(deployable="client-desk", gate="dashboard")` goes on a `query` or
a `mutation` in the deployable's own DSL.

- `deployable` is the `name` the package's manifest gives the deployable
  ([packages.md](packages.md#the-manifest)).
- `gate` is the part of that app a code opens. A code opens **one** gate.

It opens no endpoint. There is one engine-owned route family on the site's
origin; the annotation only says which constructs a gate reaches. A deployable
with no codes answers every call the way it answers a stranger.

### A worked example

A desk shows each client some briefs. The `dashboard` gate shows the holder of
one code the briefs written for that code. The `engagement` gate is for a whole
client: every code issued with the subject `acme` reads the same briefs.

```memql
/// One brief the desk shows to a client. A gated call runs under the deployable
/// owner's authority, so the rows are the owner's; each also says which code
/// wrote it and which client it is about.
@rowAuthz(owner="ownerUserId")
concept deskBrief {
  ownerUserId  string
  accessCodeId string
  client       string
  title        string
  body         string
}

@row
shape deskBrief deskBriefCard {
  row.id
  title
  body
}

/// What the dashboard shows the holder of one code. accessCodeId is reserved:
/// the handler stamps it, so the filter can only ever match this code's rows.
@accessGate(deployable="client-desk", gate="dashboard")
@actor
query deskBrief dashboardBriefs {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
  shape deskBriefCard
}

/// One client's engagement, for every code issued for that client. accessSubject
/// is reserved too: it is the subject the operator typed when adding the code
/// (acme), and a code with no subject cannot call this construct at all.
@accessGate(deployable="client-desk", gate="engagement")
@actor
query deskBrief engagementBriefs {
  args {
    accessSubject string!
  }
  filter row => row.ownerUserId == actor.userId && row.client == args.accessSubject
  paginate 20
  shape deskBriefCard
}
```

The page asks for `dashboardBriefs` with no arguments at all. It has nothing to
send, because the handler supplies the one that matters:

```
POST /_memql/access/call
{"call":"dashboardBriefs","args":{}}

200 {"data":[{"id":"8f2k1q","title":"Kickoff","body":"..."}]}
```

### The reserved arguments, and how they isolate data

Four argument names are **reserved**. When a construct declares one, the handler
stamps it, *last*, from what it knows about the session. A caller that sends one
is refused with `invalid_args`, even to a construct that does not declare it,
so a forged argument cannot tell anybody what a construct declares.

| Name | Stamped with | Use it to |
|---|---|---|
| `accessCodeId` | the bare id of the code in use. It survives a regenerate | keep each code's rows apart |
| `accessSubject` | the subject the operator gave the code. **Not stamped at all** for a code with none -- never an empty string | scope by client |
| `accessGate` | the gate the code opens | let one construct serve two gates and tell them apart |
| `siteId` | the bare id of the deployable's site | scope by deployable |

That is the isolation mechanism, and it is the author's to use: **rows carry the
stamped `accessCodeId`, or match `accessSubject`, and the filter says so.** A
gated construct that reads rows with neither opens them to everyone holding any
code for that gate. Writing the annotation without a filter is deciding exactly
that.

Declare a reserved argument as `string!`. A construct that needs `accessSubject`
and is called by a code with no subject is refused by the engine for the missing
argument, and the visitor sees `502 refused`. Every other declared argument is
the caller's, and is **untrusted input**. A product that puts one in a prompt it
generates has made that a prompt-injection question; the engine cannot answer it
for you.

### Writing a gated mutation

The call runs under the owner's authority, and a mutation writes with it. Three
things are the author's to get right:

- **Do not let the visitor choose the row's id.** An `insert` at an id that
  already names a row writes a new version of *that* row, whoever owns it: one
  code can overwrite another's. Leave the id out and the engine mints one for
  every call (the example below), or build it from the stamped `accessCodeId` so
  two codes can never reach one row. If a visitor must name the row, as a retry
  key, make every field the mutation writes `@createOnly`, so a repeat cannot
  change a row that exists.
- **A nested name is not checked.** The handler refuses a reserved name at the
  top level of `args` and looks no deeper. If a construct declares an object
  argument and spreads it into the row (a bare `args.payload`), a visitor can
  send `{"payload": {"accessCodeId": "<another code>"}}` and the spread writes
  it. Accept named fields (`accept { body }`) instead, or name the reserved field
  explicitly beside the spread, which the engine applies after it.
- **A mutation answers with only the row's bare id** (`{"data":[{"id":"..."}]}`).
  It has no shape clause, so there is nothing else that may leave.

```memql
/// A note a visitor leaves on the desk.
@rowAuthz(owner="ownerUserId")
concept deskNote {
  ownerUserId  string
  accessCodeId string
  body         string
}

/// Leave a note under the code that opened the gate. There is no id argument, so
/// the engine mints one for every call and a visitor cannot name another code's row.
@accessGate(deployable="client-desk", gate="dashboard")
@actor
mutation deskNote leaveNote {
  args {
    accessCodeId string!
    body         string!
  }
  insert {
    accept { body }
    stamp {
      accessCodeId: args.accessCodeId
      ownerUserId:  actor.userId
    }
  }
}
```

The shape of what *not* to write -- a visitor-chosen id, and a spread object with
no stamp beside it:

```memql retired
/// Two ways to let one code write over another's note.
@accessGate(deployable="client-desk", gate="dashboard")
@actor
mutation deskNote editNote {
  args {
    noteId  string!
    payload object!
  }
  update {
    id: args.noteId
    args.payload
  }
}
```

### What load refuses

These are load faults. They land on the load report, so a strict boot refuses
them and package analysis reports `dsl_refuses_boot` before a deploy. A construct
is refused when:

- the annotation is on anything but a `query` or a `mutation`, is written twice,
  or lacks either key. `deployable` matches `^[a-z0-9][a-z0-9-]{0,62}$` and
  `gate` matches `^[a-z][a-z0-9-]{0,39}$`, and neither has a default;
- it also carries `@serverOnly`, which bars the very call a visitor makes, or
  `@requiresRank` or `@requiresCapability`: the borrowed authority is a stand-in
  for the owner, and a rank or a capability would be judged against a role nobody
  meant;
- it declares a reserved argument as anything but a `string`;
- **it declares an argument that differs from a reserved name only in case**,
  such as `accessCodeID`. The handler matches the reserved names exactly, so that
  would be an ordinary argument that renders whatever the caller sent, and a
  filter on it would read as scoped to the code while opening to any id a visitor
  names;
- it shares a name with another gated construct, under one deployable, in one DSL
  domain. The same name in another domain is another product's declaration and
  not a fault;
- it is bound to no concept, or written in a nested directory of its domain. A
  call names the construct as `namespace.name`, and a nested directory's
  namespace cannot be written that way.

A package using `@accessGate` on an engine that predates it is refused at
analysis as `dsl_refuses_boot`, so the wrong order of rollout fails safely
rather than crash-looping a node. Ship the engine first.

### Which package opens a gate

Gate, deployable and construct names are short and will collide between
products, so a gate is tied to the package that ships it:

- Package analysis refuses a gate naming a deployable the manifest does not
  declare (`dsl_access_gate_foreign_deployable`), and one bound to a concept of a
  DSL domain the package does not ship (`dsl_access_gate_foreign_domain`), before
  anything is staged. Both are in the
  [refusal catalogue](packages.md#what-the-analysis-checks-and-what-it-refuses).
- At run time the bff opens a construct only when its `deployable` equals the
  site's package deployable *and* its domain is one the site's package ships.

**The domains a package ships are `dslDomains` on the package.** A deploy
writes them, and nothing else does; no client write can name the field. A deploy
records them when it changes what a visitor is served: a run that publishes a
serving version, and a run with every app skipped that carries DSL (which is how
DSL is deployed ahead of a candidate). **A run that publishes only candidates
leaves them alone**, so a candidate of a tree that dropped a domain cannot close
the gates of the build the public is being served
([deployables.md](deployables.md#publishing-the-candidate-version-instead)). A
rollback restores the list of the deployment it returns to.

**An empty list opens nothing**, and that is also what a source deployed before
the field existed reads as. A gate that suddenly answers 404 on such a source is
repaired by deploying it again.

This stops one product's code reaching another's operations by accident. It is
not a defense against a hostile author: DSL is already cluster-wide authority.

---

## The wire

This is a documented exception to the engine's gRPC-first rule, owner-approved on
2026-10-29. A visitor's browser has no MemQL identity to open a stream with, and
only an HTTP response can set an `HttpOnly` cookie, so the other party dictates
the wire. It is one route family on the deployable's own origin, with four
method and path pairs. The edge answers them ahead of the bundle and of the
generic `/_memql/*` proxy, **whatever the site's `apiProxy` says**.

| On the site's origin | bff route |
|---|---|
| `POST /_memql/access`, body `{"code":"..."}` | `POST /access/unlock` |
| `GET /_memql/access` | `GET /access/session` |
| `POST /_memql/access/leave` | `POST /access/leave` |
| `POST /_memql/access/call`, body `{"call":"name","args":{...}}` | `POST /access/call` |

Any other method or path under `/_memql/access` is not this surface's and
reaches the generic proxy as it always did. The bff routes are never reachable
from outside: the edge is the only way in.

Every reply carries `Cache-Control: no-store` and `X-Content-Type-Options:
nosniff`. Every POST must declare `Content-Type: application/json`, **`leave`
included**, whose body is empty or `{}`. There is no CORS: the page that calls
this is the deployable's own.

### The cookie

```
memql_access=mql_acs_<43 characters>; Path=/_memql/access; HttpOnly; Secure; SameSite=Strict
```

Host-only, with no `Max-Age` or `Expires`: the server is the authority on
expiry, and an extension must not be cut short by a browser-side clock. The
`Path` means it travels with these four requests and nothing else, so arriving
from a link in an email is fine, and it is not sent to `/_memql/accessories`.
Page script cannot read it.

### The codes

Twenty-five symbols of Crockford base32 in five groups,
`K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZA`. The bff normalizes whatever was typed before it
hashes: letters are upper-cased, `O` reads as `0`, `I` and `L` as `1`, and every
other character outside the alphabet is dropped, including the en dashes and
zero-width characters that email clients introduce. **A page sends what the
person typed, as it was typed.**

### What the edge answers

These come before anything is forwarded. The checks that cost nothing -- the
content type, the `Origin`, the declared size -- come before the rate limit, so a
page that sends a malformed request cannot spend a visitor's budget. Every body
is JSON:

| Status | Body | When |
|---|---|---|
| `404` | `{"error":"not_found"}` | the site has no owner (the platform's own sites), or the edge has no API target |
| `415` | `{"error":"json_required"}` | a POST that does not declare `application/json` |
| `403` | `{"error":"forbidden_origin"}` | a POST whose `Origin` names another host. `Origin: null` is refused too; a POST with **no** `Origin` is allowed |
| `413` | `{"error":"too_large"}` | a body over 1 KiB (unlock, leave) or 64 KiB (call), by what it declares or by what is read |
| `400` | `{"error":"bad_request"}` | a body that could not be read |
| `429` | `{"error":"rate_limited"}` | a budget is spent. `Retry-After` is in whole seconds |

Three replies are **not JSON**, and a client must not assume they are:

- a **`502`** from the proxy, when the edge cannot reach the bff, is `text/plain`
  (`upstream unavailable`). It is the proxy's reply, shared by every route it
  serves;
- a deployable that is not live never reaches the surface: offline answers `503`
  and a draft or archived one `404`, both plain text;
- a `401` for a [revoked sign-in](#a-browser-with-a-revoked-sign-in), below.

The edge then strips `X-Memql-Access-Site` and `X-Memql-Access-Owner` from
everything it forwards, on every path, and sets them itself. The stamp is a
pointer, not an assertion: the bff re-reads the site under the named owner and
requires the row's own owner to equal it.

### What the bff answers

**`POST /_memql/access`** trades a code for a session.

| Status | Body | Meaning |
|---|---|---|
| `200` | `{"gate":"engagement","subject":"acme","expiresAt":"2026-10-30T18:00:00Z","remainingMs":86399000}` | in, with `Set-Cookie` |
| `401` | `{"state":"invalid"}` | unknown, removed, another site's, or superseded by a regenerate. All one reply, so it is no oracle |
| `403` | `{"state":"expired"}` | the exact code, past its expiry. Only somebody holding the code can learn this |
| `502` | `{"error":"refused"}` | the bff could not read or write the rows. Try again |

A failed attempt writes nothing. `subject` is `""` when the code has none.

**`expiresAt` and `remainingMs` are one instant**, the *session's* end: the
earlier of the code's expiry and 24 hours after it opened, stated two ways.
`remainingMs` is the one to schedule from, so a wrong clock on the visitor's
machine does not move the moment.

**`GET /_memql/access`** says whether the cookie presented is still good.

| Status | Body | Meaning |
|---|---|---|
| `200` | as above, no new cookie | still in; the remainder is as of this answer |
| `401` | `{"state":"none"}` | no cookie |
| `401` | `{"state":"ended"}` | a cookie whose session is over for any reason: expired, the 24 hours, regenerated, removed, left, or a token with no row at all |
| `502` | `{"error":"refused"}` | a failed read. **Clears nothing**: a database blip is not a visitor taken out |

Both `401`s clear the cookie.

**`POST /_memql/access/leave`** ends the session the cookie names, and no other
-- two browsers on one code are two sessions. It answers `204` and clears the
cookie, and it is idempotent. If the end cannot be written it answers `502
{"error":"refused"}` and the cookie stays, because clearing it would say the
visit is over while the row still honors the token.

**`POST /_memql/access/call`** runs a construct.

| Status | Body | Meaning |
|---|---|---|
| `200` | `{"data":[...]}` | a query's rows, or a mutation's bare id |
| `401` | `{"state":"ended"}` | the session is over, **or there was no cookie at all** (a call has one word for both). Clears the cookie |
| `404` | `{"error":"not_found"}` | see below |
| `400` | `{"error":"invalid_args"}` | a body that is not one JSON value, a key the construct does not declare, or any reserved name |
| `502` | `{"error":"refused"}` | the construct refused the call (a wrong argument type, a constraint), or a read failed. Clears nothing |

**One `404`, identical to the byte,** for an unknown construct, an undeclared
one, one on another gate, one on another deployable, one in a domain the package
does not ship, a name two of the package's domains both declare, a site no
stamped owner can read, and a request with no stamp. A holder of one gate's code
learns nothing about another's. The registry is consulted before the arguments
are looked at, so a refused argument is only ever reported for a construct the
session's gate reaches.

A query answers with its **shape's rows**, with every id bare. A query with no
`shape` clause answers with only the ids of the rows it selected, and the bff
logs a warning at the site and construct so the author can find out why a page
has no fields. A mutation answers with only the row's bare id.

**A call decides twice.** The session is read before the construct runs and
again before its rows leave. A regenerate, a removal, a shortened expiry or the
instant of expiry itself can land while the construct runs, and the answer is
then `401 {"state":"ended"}` with no rows and the cookie cleared. A write the
construct made stays made. If the session cannot be read the second time, the
rows still do not leave, but nothing has said the visit is over: the answer is
`502 {"error":"refused"}`, the cookie stays, and a database blip costs one retry
and takes nobody out.

### Rate limits

Per replica, so the cluster-wide ceiling is the rate times the number of
replicas:

| Operation | Budget | Keyed on |
|---|---|---|
| unlock | 10 a minute | the site and the address |
| unlock | 60 a minute | the site |
| session, leave, call | 600 a minute | the site and the address |

The address is the **right-most** `X-Forwarded-For` entry: the hop the ingress
itself recorded, which a caller cannot choose. The leftmost entry is the
caller's to write, and rotating it would spend the site's sixty unlocks and lock
real visitors out. Unlock draws on the address's bucket first, so one machine
can take at most ten of the sixty. `Retry-After` is how long the refusing bucket
takes to give one token back: 6 seconds for an address that spent its ten
unlocks, 1 otherwise.

**These bound load, not guessing.** What stops guessing is 125 bits.

### A browser with a revoked sign-in

A browser that also holds a `memql_auth` cookie whose session has been revoked
gets a plain-text **`401 session revoked`** on all four routes, unlock
included. The bff's session-revocation middleware answers any request that
carries a revoked token, on any route, before the access handler sees it, so the
body is not JSON. A client must treat **a `401` that is not JSON as locked**,
and a visitor in that state cannot unlock until the stale cookie is cleared.

---

## What the page does

The server refuses at once and every time, so a page that does nothing is still
safe. These three obligations are what make the *browser* leave when the
server has stopped answering, and they are a few dozen lines over `fetch`. The
client is not in the SDK in this version.

| | The page | The server beside it |
|---|---|---|
| **1. Leave on time** | Schedule the end from `remainingMs` the moment a reply arrives, and drop gated state the instant it fires. Never compare `expiresAt` with the page's own clock | Refuses after the exact instant of expiry, with no grace |
| **2. Learn of an act** | Ask `GET /_memql/access` every 5 seconds while the page is visible, and at once on `visibilitychange`, `focus` and `online`. A changed `remainingMs` reschedules the end | A regenerate, a removal or a shortened expiry is refused on the next request |
| **3. Treat a 401 as the end** | Any `401` on a call, and any `401` that is not JSON, means locked: drop gated state and show the code box | A `401` clears the cookie |

Keep gated rows in memory. Anything held in `localStorage`, `sessionStorage`,
IndexedDB or a cache is a place a drop can miss. A `5xx`, a `429`, a network
error or a plain `404` says nothing about the session: keep the state, and ask
again at the next tick. Do not read them as locked, and do not read them as in.

```js
const ACCESS = "/_memql/access";
const isJson = (response) => (response.headers.get("content-type") ?? "").includes("json");

const post = (path, body) =>
  fetch(ACCESS + path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" }, // leave included: the edge refuses any other type
    body: JSON.stringify(body ?? {}),
  });

export function accessSession({ onOpen, onLocked }) {
  let live = false;
  let endTimer;
  let poller;

  // Obligation 3, and where obligation 1 ends: stop both clocks, drop everything gated.
  function lock() {
    live = false;
    clearTimeout(endTimer);
    clearInterval(poller);
    poller = undefined;
    onLocked();
  }

  // Obligation 1: end the visit from remainingMs, never from expiresAt against this clock.
  function open(session) {
    live = true;
    clearTimeout(endTimer);
    endTimer = setTimeout(lock, session.remainingMs); // a changed remainingMs lands here again
    poller ??= setInterval(recheck, 5000);
    onOpen(session);
  }

  // Obligation 2: ask whether the session still stands. Offline or a 5xx says nothing either way.
  async function check() {
    let response;
    try {
      response = await fetch(ACCESS, { credentials: "same-origin" });
    } catch {
      return;
    }
    if (response.status === 401) lock(); // none, ended, or a plain-text "session revoked"
    else if (response.ok) open(await response.json());
  }
  const recheck = () => live && document.visibilityState === "visible" && check();
  document.addEventListener("visibilitychange", recheck);
  window.addEventListener("focus", recheck);
  window.addEventListener("online", recheck);

  return {
    restore: check, // on page load: a cookie from an earlier visit may still be good

    async unlock(code) {
      const response = await post("", { code });
      if (response.ok) {
        open(await response.json());
        return "open";
      }
      if (response.status === 429) return "wait"; // Retry-After says how long
      if (response.status === 403) return "expired"; // the exact code, past its expiry
      if (response.status === 401) return isJson(response) ? "invalid" : "blocked";
      return "unavailable";
    },

    async call(name, args) {
      const response = await post("/call", { call: name, args });
      if (response.status === 401) {
        lock();
        throw new Error("The visit has ended.");
      }
      if (!response.ok) throw new Error(`The call was refused (${response.status}).`);
      return (await response.json()).data;
    },

    async leave() {
      const response = await post("/leave");
      if (!response.ok) throw new Error("Could not leave; the session still stands. Try again.");
      lock();
    },
  };
}
```

---

## The limits, stated plainly

- **A code gates what the bff returns, not the bundle.** Anything in the bundle
  is public. Gating a path of static files is not in this version.
- **An idle tab learns of an operator's act within 5 seconds.** A regenerate, a
  removal or a shortened expiry is refused at the server immediately, and an open
  tab finds out at its next poll, at once when it becomes visible or regains
  focus or the network, or through a `401` on any call. The server has no way to
  push to a browser, and a push channel is not worth building for the
  difference. **Expiry itself is exact to the timer.** A hidden tab's timer may
  be throttled by the browser, so it re-checks the moment it becomes visible,
  and the server has already stopped answering it.
- **Ending a session cannot unsee.** It stops the next answer. Rows the page
  already received stay in the browser until the page drops them, which is why
  obligations 1 and 3 are the page's.
- **The rate limits are per replica and bound load, not guessing,** and they key
  on the right-most `X-Forwarded-For` entry. A proxy added in front of the
  ingress groups every visitor behind it into one bucket: ten unlocks a minute
  between them, and a share of the site's sixty.
- **Borrowed authority does not re-check that the site's owner is still
  active.** A construct runs as the owner, as a shopper's form does, and nothing
  per request asks whether that person is still around. **Removing the codes is
  how an operator ends access.**
- **A code is a bearer credential.** Whoever holds it can use it until it
  expires, is replaced, or is removed, and a stolen cookie is good until the
  code's expiry, a regenerate, a removal, 24 hours or `leave`. Several people can
  use one code at once; there is no per-visitor identity and no list of who is in
  now. Give one code to each person or party you may need to take out alone.

| Threat | What stands in the way |
|---|---|
| Guessing a code | 125 bits. Failures write nothing and are one reply. Rate limits bound load |
| A leaked row, backup or log | Only digests are stored. A code, a token, a digest and a call's arguments are never logged or put in an audit event |
| Cross-site request forgery | `SameSite=Strict`, JSON-only POSTs and the `Origin` check |
| Script on the deployable | Cannot read the cookie. Can call declared constructs while the page is open, which is the reach the code already grants. The edge's hash-based CSP applies |
| Forged reserved arguments | Refused from the caller, stamped last |
| Forged edge headers | Stripped on every path, and the bff compares the stamped owner with the site's own |
| Injection through a call | Argument names come from the declaration, values are JSON-encoded, and the construct name comes from the registry |
| Reaching a `@serverOnly` construct | Never internal origin, and refused at load |
| Enumerating gates and constructs | One `404` for every miss |
| Session fixation | A fresh token on every unlock; a cookie with no row is `ended` |

---

## When a gate answers 404

| Symptom | Cause | Fix |
|---|---|---|
| The panel says the app has no gates yet, and Add is absent | No construct declares `@accessGate` for this deployable's name, or the source has not been deployed since access codes existed, so its `dslDomains` are empty | Declare the gate and deploy; or, for an older source, deploy it again |
| Every call answers `404` on a source that worked before | The same: an empty `dslDomains` opens nothing | Deploy the source again |
| One call answers `404` and others work | The name is not declared, is on another gate, is `@disabled`, is in a nested directory, or two of the package's domains both declare it | Check the gate, the name and `dsl_access_gate_*` in the analysis |
| All four routes answer `404` for one site, and the bff logs "this site's codes answer 404" | The site's owner cannot read it. A site whose owner was removed from its organization reads as zero rows under that owner. The log line carries the reason | Restore the owner's membership, or give the site an owner who can read it |
| All four routes answer `404` for a platform-managed site | It has no owner to borrow | Codes are for your own deployables |
| A plain-text `401 session revoked` | The browser carries a revoked `memql_auth` cookie | Clear the site's cookies |
| A `503` or a plain `404` before any JSON | The deployable is offline, or not live | Take it live. An offline spell does not end a session: it works again if its code has not expired |

## Operating it

- **`memql_site_access_requests_total{op,result}`** counts the four operations
  by result on bff nodes (`app="bff"`), with no per-attempt rows behind it. A
  climb in `unlock`/`invalid` is somebody guessing or a code passed along
  wrongly. **`refused` on `unlock`, `session` or `leave` is the store failing
  under the surface, and is the one to alert on.** `refused` on `call` is not an
  alert: a visitor can move it by sending a construct a wrong argument type, and
  a code with no subject calling a construct that needs one counts there.
- **"Has anybody opened this?"** is answered by the newest session of the code's
  current generation, with no write to the credential itself.
- **Logs name the deployable and the construct, never an argument.** A code, a
  token and a digest are never logged. The warning about a site whose codes
  answer 404 carries the deployable, so the Logs button on its page shows it.
- **Failed attempts are not recorded.** Operator acts are in the audit log.

---

## Reference

- Edge: `component/edge/access.go` (routing, caps, budgets, the stamp),
  routed in `component/edge/handler.go` ahead of the `/_memql/*` proxy.
- bff: `component/server/access_handler.go`, mounted by
  `app/transport_access_surface.go`.
- The registry and the load rules: `component/memql/access_gate.go`,
  `component/memql/access_gate_load.go`. Codes and tokens:
  `component/memql/site_access_code.go`.
- Operator acts: `integrations/siteaccess/`; DSL in `dsl/platform/`
  (`siteAccessCode`, `siteAccessSession`, the six builtins).
- MemQL OS: `clients/os/src/apps/deployables/access/`.
- Design: `docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md`.

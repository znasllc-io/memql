# Desktop access: nobody enters MemQL OS without model access

Design record, 2026-11-24. Status: approved by the owner 2026-11-24; built
(plan `docs/superpowers/plans/2026-11-24-desktop-access.md`).
Follows the vendor accounts work
([2026-11-21-vendor-accounts-and-api-keys-design.md](2026-11-21-vendor-accounts-and-api-keys-design.md))
and sits beside the memory-search model switch
([2026-11-23-memory-search-model-switch-design.md](2026-11-23-memory-search-model-switch-design.md)).

## Intent

MemQL is an AI platform, and model access in it is bring-your-own: a person's
own machines, apps and API keys, the machines and keys other people lend them,
and the cluster's vendor account only where the cluster owner allows it. MemQL
never pays for anybody's inference on its own.

A person with none of these cannot really use MemQL OS. Ask, goals and file
summaries all run on the person's own access, so for them the desktop is a set
of features that fail one at a time. The owner's rule: such a person does not
get into MemQL OS at all. They see a page saying their access is not set up
and to ask an administrator, with a reference number. The administrator
searches that reference in Logs and sees exactly what is missing.

The people who set the cluster up are exempt: its primary owners and
developers.

The owner's decisions (2026-11-24):

1. Everyone except the primary owners and developers needs model access to
   enter MemQL OS, whatever apps their role gives them ("Everyone needs it").
   The reason: model work happens outside Ask too, on the person's own access.
2. Access that is SET UP counts even when it cannot answer right now, such as a
   lent laptop that is asleep ("Set up counts").
3. The page tells the person to ask an administrator and shows a reference. The
   reference goes into the logs with what is missing, and can be searched there.
4. The exemption is for the primary owners and developers: the people who
   configure and own the cluster, not the owners or developers of one account.
   Account-level roles are approved but not built (accounts program P27 and P28,
   [2026-11-12-accounts-program-design.md](2026-11-12-accounts-program-design.md)).
5. Some people in other accounts must have no Ask at all, whatever their model
   access.

## Goals, non-goals, anti-goals

Goals: the five decisions. The page lifts by itself once access is set up. One
search in Logs finds the reason. It answers the same on every replica.

Non-goals:

- Closing MemQL's API, the VS Code tools, the SDK or Cockpit to a held person.
  The server already refuses every model call for a person with no access, and
  the owner's rule is about MemQL OS.
- Changing who may lend machines or keys, or who may grant the cluster's
  account.
- A check an owner runs for another named person. The reference serves that
  need.

Anti-goals, each of which makes the work a failure:

- A person with nothing set up reaches the desktop. The one exception is a
  check that cannot be answered at all (section 4).
- A person whose access is set up is locked out because a machine is asleep or
  a key ran out of credit.
- An owner or developer is locked out of the cluster they set up.
- An owner or developer of one account (once that exists) is treated as exempt.
- A page or message tells a person to open an app they cannot open, as Ask's
  "Open Fleet" does today.
- The page shows a reference that Logs cannot find, or the log line says less
  than what is missing.
- The check spends a model call.

## How it works today

Verified in the code on 2026-11-24.

**The one gate.** It is `clients/os/src/chrome/CoreGate.tsx`, mounted at
`chrome/Shell.tsx`.

- It holds the desktop only while the CLUSTER's `ai` readiness verdict is
  `unconfigured`. That verdict means one door to a model anywhere plus an
  embedding model, computed as a system actor over every machine whoever owns
  it (`component/memql/readiness/inference.go`).
- It lets through a person whose own or lent key can pay (`chrome/keyLetsIn.ts`,
  reading `inferenceStatus.keyAccount`).
- It steps aside while a window opened from it is open.
- It fails open when the readiness feed has not answered within 3 seconds.
- Owners and developers see "Set up this cluster". Everyone else sees "MemQL is
  not set up yet".

So once any machine on the cluster offers a model, everyone gets in, including
people who cannot use that machine.

**Inside, for a person with no access.**

- Ask's Send button is disabled.
- The dock dot says "Ask has no usable inference. Open Fleet to connect a
  machine or check its models." (`ask/useAskReadiness.ts`).
- A turn that reaches the router fails with "No eligible model could take this
  request. Check the available models in Fleet." and an Open Fleet button
  (`ask/askFailure.ts`).
- Fleet is for owners and developers only (`dsl/rbac/seeds.memql`).

**The per-person reading that exists.** `inferenceStatus`
(`dsl/common/builtins.memql`; `inferenceDoors` in
`component/memql/fleet_catalog_read.go`) answers for the CALLER only, with no
person argument.

- `eligible` is `LocalEligible || AppEligible || CloudOpen || Federation`.
- `LocalEligible` needs an ONLINE machine, the person's own or lent to them,
  whose model does structured output with a context of at least
  `MinimumContextWindow` (8192).
- The vendor half comes from `ProviderRegistry.VendorDoorFor`: the person's own
  and lent keys, and the cluster's account where it is open to them.

**Model work outside Ask uses the person's own access.** A Library upload's
analysis (extract, summarize, chunk, embed) runs as the file's owner
(`integrations/library/analysis.go`). Goals run as their owner.

**References and logs.**

- `generateErrorId` (`component/grpc/ai_handlers.go`,
  `component/identity/http/server.go`) makes `ERR-` plus six hex characters.
- `logsSearch`'s `text` facet matches the message, the subject and the
  structured attributes (`component/logstore/reader.go`).
- Logs are readable by admin and above.
- Retention is `MEMQL_LOGS_RETENTION_DAYS`, 30 days by default.
- Precedent: the VS Code Productivity Tools show a short message and a
  troubleshooting reference to search in MemQL OS > Logs
  (`editors/productivity/README.md`).

**The primary owners and developers today.** Every role is cluster-wide.
`auth.MayManageFleet` (`component/auth/fleet.go`) is exactly "role slug owner
or developer, not scoped to an account".

**The Ask permission.**

- `read app:ask` is seeded on all five predefined roles.
- The server checks it on every path that runs Ask: `component/memql/ask.go`
  and its siblings, `component/grpc/ai_handlers.go` and `ask_voice.go`. Tests
  are in `component/grpc/ask_access_test.go`.
- Grants resolve in this order: the role, then groups (a deny wins among
  groups), then the person's own grants (`component/auth/grant_resolver.go`).
- A person's groups include account groups (`acct-<id>`), taken from their
  membership rows.
- The shell hides Ask from a person without the permission (`canAsk` in
  `ask/AskProvider.tsx`).

## Approaches

- **A. A per-person access check on the server, read by the OS at the door
  (chosen, my call).** One new read answers, for the caller, whether they are
  exempt or have model access set up. When it holds them, it mints a reference
  and logs what is missing. The OS draws the page from it.
- **B. Refuse the person's whole stream at the server (not taken).** This also
  closes MemQL's API, the VS Code tools and the SDK to them. It touches stream
  admission on every node, and still needs the page. It goes beyond the owner's
  rule, which names MemQL OS.
- **C. Refuse sign-in at the identity service (not taken).** The identity node
  cannot see machines or keys. A held person could not add an API key of their
  own either, because that needs a session; the earlier decision that a key of
  one's own lets one in relies on it.

## The design

### 1. What counts as model access set up

A person has access set up when at least one of these holds for them:

- **A machine.** A machine of theirs, or one lent to them (a share to them or
  to a group they belong to), offers a chat model meeting the minimum profile:
  structured output and a context of at least `MinimumContextWindow`. It counts
  online or not. A model that only makes embeddings does not count, and nor
  does a revoked registration.
- **An app.** Claude Code or Codex on a machine of theirs is allowed by that
  machine and signed in, and their own app routing allows it. It counts online
  or not, and "out of usage" counts (my call). "Measured signed out" does not
  count.
- **A key.** An API key of theirs, or one lent to them, that has not been
  refused, is not paused by its owner, and whose owner is still an active user.
  "Not checked yet" and "out of credit" count (my call: credit runs out and
  comes back the way a machine sleeps and wakes).
- **The cluster's account.** They hold `execute app:settings/clusteraccount`
  and the cluster's vendor account is set up.

Memory search needs nothing of the person: under the memory-search design,
searches are embedded as the indexer.

This differs from `inferenceStatus.eligible` only in counting what is asleep or
temporarily empty. Reuse its predicates with those tests dropped, so the two
cannot drift apart in anything else.

### 2. Who is exempt

The primary owners and developers: role slug `owner` or `developer`, not scoped
to an account. That is `auth.MayManageFleet`'s test; a sibling predicate named
for this purpose is fine.

When roles become per account (accounts program P26 to P28), the exemption
becomes the owner or developer role held in the primary account. An owner or
developer of another account is held like anyone else.

`admin` is not exempt, as the owner's words set it. One consequence: an admin
with no model access of their own is held too, and asks an owner like anyone
else; they cannot open Logs or Users to help others until they have access.

Exempt people enter whatever their own access. The cluster setup screen ("Set
up this cluster") stays as it is for them.

### 3. The check (server)

A new read for the caller only: no model call, and no argument naming another
person. Suggested name `desktopAccess`, a builtin beside `inferenceStatus`.

It answers `admitted`, `exempt`, and, when the person is held, a `reference`.

The first answer that holds a person on a showing of the page:

- mints a short reference in the format the engine already uses, `ERR-` and six
  hex characters (my call: one format, so one search box finds every
  reference);
- writes ONE log line. The line carries the reference as a structured
  attribute, the person's id as its subject, and each source checked with why
  it does not count, in plain words. For example: "no machine of theirs or lent
  to them offers a chat model", "the key lent by `<id>` is paused by its
  owner", "may use the cluster's account, but none is set up";
- names no one beyond what the person may already see. A lender's id appears
  only for something lent to this person.

**The line is written before the reference is returned** (my call). The log
store's normal path drops lines by design: a bounded queue, a per-second rate
limit and asynchronous batches (`component/logstore/sink.go`). So this line is
inserted directly and confirmed, and only then is the reference returned. If
it cannot be stored, the answer still holds the person but carries NO
reference, and the page shows the same words without the reference line. The
decision is about access, so a logging failure never lets anyone in, and the
page never shows a reference that Logs cannot find.

**Checking again keeps the reference** (my call). The page passes its
reference back when it checks again. A check that still holds the person
writes nothing new and returns the same reference, unless the reasons have
changed: then it writes a new line under the same reference.

An answer that admits the person writes nothing. A check that fails with an
error answers "unknown", not "held".

### 4. The page (OS)

The page is shown instead of the desktop when the check holds the person: not
exempt, and nothing set up. It replaces today's "MemQL is not set up yet" page
for everyone but owners and developers. It holds whether or not the cluster as
a whole is set up.

- **Layout:** the shared entry frame (`kit/EntryLayout`, with the theme toggle)
  and the first-run gate's shape: the mark, a title, one sentence, then the
  actions. It follows `clients/os/DESIGN.md`.
- **Words** (a draft, settled in the build): the title "Your access is not set
  up yet", then "MemQL needs a model it can use for you. Ask your administrator
  to set one up, and give them this reference." Then the reference with a Copy
  control, and "This page opens by itself once it is set up."
- **Actions:**
  - Add an API key: only where the person may add one, as the current gate
    does. The key wizard runs ON THE PAGE, inside the entry frame, not as a
    desktop window (my call). Today's gate opens Settings as a window and
    draws the whole desktop while any window is open
    (`chrome/CoreGate.tsx`), so a held person could use the desktop for as
    long as they left the wizard open. Here the desktop never draws for a held
    person.
  - Sign out.

```
               (MemQL mark)
       Your access is not set up yet
   MemQL needs a model it can use for you.
   Ask your administrator to set one up,
   and give them this reference.

          ERR-3f9a1c    [Copy]

   This page opens by itself once it is set up.

   [Add an API key]                 [Sign out]
```

How it behaves:

- **It lifts by itself** (my call). It checks again when the window regains
  focus, when the connection returns, after a key is added on the page, and
  every 30 seconds while the page is visible (paused while the tab is hidden).
  A broadcast cannot do this: none of the changes that fix a held person
  reaches that person's stream. Key rows publish no graph event, on purpose
  (`component/memql/site_access_guards.go`); a machine registration is visible
  only to its owner and the cluster's owners (`dsl/worker/concepts.memql`);
  grants are readable from admin rank (`dsl/rbac/concepts.memql`). The check
  is one cheap read, and checking again keeps the reference (section 3).
- **An "unknown" answer lets the person in** (my call), as the current gate does
  when its feed does not answer: an outage is not evidence that a person lacks
  access. The model calls are still refused on the server.
- **Access removed while inside** (my call): the desktop checks again on focus
  and when the connection returns, and a check that holds the person shows the
  page. Open windows do not keep the desk.

### 5. Inside, when access is set up but asleep

Ask and the other model features stop telling people who cannot open Fleet to
open it. The message names what is asleep or empty, and whom to ask:

- the person's own machine: "Your machine X is offline";
- a lent machine or key: "X's machine is offline; ask X", or "the key lent by
  X is out of credit";
- otherwise: "ask your administrator".

Open Fleet stays for people who can open Fleet. The router's door list already
carries a reason for each door (`component/router/doors.go`,
`component/router/fleet_refusal.go`).

### 6. Ask for people in other accounts

Use what exists (my call). In Users > Access, choose the account's group and
deny Ask. The shell hides Ask and the server refuses every Ask run. Nothing new
is built now.

When sign-in is per account (accounts program P26 and P34), Ask follows the
role held in each account, and that replaces this grant.

Known limits until then:

- The deny follows the person into every account they belong to.
- An Allow set on the person themselves overrides it.
- Their own past conversations stay theirs: readable through the API, not shown
  on the desktop. This is deliberate (`component/memql/personal_data_plane.go`).

### 7. Across replicas and restarts

The check reads only rows (registrations, shares, keys, grants) and is computed
on every call, so nothing lives on one replica. The reference lives only in the
log line and on the page. Because the page passes its reference back, checking
again on a different replica keeps it.

### 8. Order of work

The memory-search design is built first (my call). "Memory search needs nothing
of the person" (section 1) is true only once its indexer embeds everything.
Today an item is embedded as the person who wrote it, and an API key pays only
for text calls (`component/memql/vendor_account.go`). Built the other way
round, a person let in on a key alone would get a desktop whose files are not
indexed and whose search fails: the "features failing one at a time" that this
design exists to prevent. With memory search first, the access check needs no
interim rule for search.

## Tests and acceptance

- **Held, with the reference.** A person with nothing set up is held, and the
  page shows the reference. One log line carries the reference and each reason,
  and `logsSearch` with the reference finds it.
- **Set up but not answering is admitted.** This covers a lent machine that is
  offline, an app on an offline machine of the person's own, a key that is out
  of credit, and a key not checked yet.
- **Not access, each with its own reason.** A refused key, a paused lent key, a
  lender who left, a revoked registration, an embedding-only model, and the
  cluster permission with no cluster account each count as no access, and each
  appears as its own reason.
- **The exemption.** An owner or developer with nothing set up is admitted. An
  admin with nothing set up is held. A role scoped to an account is held,
  whatever it is called.
- **Lifting.** The page lifts by itself within 30 seconds of an administrator
  lending a machine while the person watches it, when the window regains
  focus, and after a key is added on the page.
- **One reference per showing.** Checking again while the page is shown
  returns the same reference and writes no new line; changed reasons write a
  new line under the same reference.
- **No desktop while held.** Adding a key on the page never draws the dock,
  the launcher or any app.
- **Failure.** A check that fails lets the person in and writes no reference.
  A log line that cannot be stored holds the person with no reference shown.
- **The cluster screen.** The cluster setup screen still holds for owners and
  developers when the cluster has no model at all.
- **Messages inside.** Ask's failure messages no longer offer Fleet to a person
  who cannot open it.
- **Across replicas.** The check answers the same on any replica (two bff
  replicas over one database).

## Left to the builder

Names; the exact words within the design rules; whether the page extends
`CoreGate` or is a sibling component; how the broadcast check subscribes; and
which log level, among those kept by default.

## Calls made without the owner (to check)

- MemQL OS only (approach A). The API is not closed to a held person.
- "Out of credit" and "out of usage" count as set up, like a machine asleep.
- A check that fails lets the person in.
- A lender's id appears in the log line.
- Ask in other accounts uses the existing deny grant; nothing new until sign-in
  is per account.
- `admin` is not exempt (your words); an admin without access asks an owner.
- The reference reuses the `ERR-` format.
- The page checks again every 30 seconds while visible, because nothing an
  administrator changes reaches the held person's stream.
- The key wizard runs on the page, so the desktop never draws for a held
  person.
- A log line that cannot be stored still holds the person, with no reference
  shown.
- Memory search is built first, then this.

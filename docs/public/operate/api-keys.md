---
title: API keys
audience: public
status: stable
area: operate
sinceVersion: 0.31.40
owner: znas
---

# API keys

A model vendor is paid through an **account**. MemQL knows two kinds:

- **the cluster's account**: the cluster's own sign-in with Anthropic or
  OpenAI, set up once by its owner in **Settings > Vendors**
  ([Anthropic](auth/anthropic-federation.md), [OpenAI](auth/openai-federation.md)).
  It bills the organization its owner connected, and it is **not a pool for
  everybody**: it pays for the cluster's own work and for the people its owner
  allows ([who the cluster's account pays for](#who-the-clusters-account-pays-for));
- **an API key**: a person's own key at one of those two vendors, added in
  **Settings > API keys**.

A key belongs to the person who added it and spends that person's money. They
may lend it to named people and groups. The cluster itself never has a key:
the work the cluster runs as itself (schedules, sweeps, seeds, anything with no
person behind it) is paid by the cluster's account or by nothing.

This page is the sibling of [Sharing a machine](shared-machines.md). A machine
lends local models; a key lends a vendor account.

---

## Who can add a key

Anybody signed in. Model access on a cluster is bring-your-own for everybody,
so adding a key is not kept to operators.

Adding is a named part of Settings:

| Capability | Lets a person |
|---|---|
| `execute app:settings/apikeys` | add a key |

It is seeded on **every predefined role**: owner, developer, admin, user and
viewer. An operator stops one person or one group from adding keys with a deny
of that part in **Users > Access**
([grants](auth/access-model.md#grants-to-people-and-groups)). A role an
operator authored holds the part only if it was given it.

**A role scoped to one organization may hold it too.** Such a role is barred
from every other part of Settings but Access, because those are cluster
configuration; a person's own key is not. Like any authored role it holds the
part only when it was given it. To let an organization's people add keys, give
their group two things in **Users > Access**, or write both into the role:
opening **Settings**, and **Settings: Add API keys**. The cluster's vendor
account stays out of an organization role's reach, whatever is granted.

**Settings > API keys** itself has no capability of its own. It opens for
everybody who can open Settings, and it is what a user or a viewer finds
there. So a person always reaches the keys they own: they see, rename, check,
lend and remove them whatever their role holds, and a person who loses the
part keeps the keys they have and can still use them. Only adding is gated.

A person a key is **lent to** needs nothing. They use it without opening
Settings.

A key is added and changed by a person in their own signed-in session. A
personal access token, a service account, an app session, access delegated to
somebody else, work continued on a person's behalf and a step of a run are all
refused: a key spends its owner's money, and a run's journal keeps whatever a
step was handed.

## Add a key

**Settings > API keys**, then the add button. The wizard asks one thing at a time:

1. **Vendor**: Anthropic or OpenAI. No other vendor takes a key.
2. **Name**: yours, 1 to 60 characters, unlike your other keys' names.
3. **Key**: pasted into a field that hides it.
4. **Check**: the cluster lists the vendor's models with the key. Listing
   models sends no prompt and is not billed.

Nothing is saved before the check passes. A key the vendor refuses is not
stored, and the wizard shows the vendor's own words with the key still in the
field for another try.

What is refused before the vendor is asked:

- a value that is not shaped like the vendor's keys (Anthropic keys start
  `sk-ant-`, OpenAI keys start `sk-`), or that has spaces or line breaks;
- an **organization admin key** (`sk-ant-admin...`, `sk-admin-...`). It manages
  an organization and cannot call a model;
- an eleventh active key at one vendor. A person holds at most **ten active
  keys per vendor**;
- any key for a vendor whose calls this cluster sends to an address other than
  the vendor's own. A key is only ever sent to `https://api.anthropic.com` or
  `https://api.openai.com`; a cluster that overrides a vendor's address uses no
  key for it. A request that carries a key follows no redirect, so the vendor's
  address cannot pass it on either.

Your first key at a vendor is your **default** there.

### A key lets you in

On a cluster with no machine, no app and no vendor account of its own, MemQL OS
opens on a setup screen instead of the desk. A key that can pay lets you past
it: yours, or one lent to you. The screen offers **Add an API key**, which
opens this wizard directly. The cluster's own setup is unchanged, and so is
everybody else's screen until they add a key of their own or an owner sets up
inference ([first run](first-run.md#the-gate)).

### A key that may not list models

An OpenAI key can be restricted so that it may call a model and may not list
them. Such a key is accepted and saved as **checked by the calls it pays
for**: the wizard says so, the daily check skips it, and it reads "Not checked
yet" until its first real call.

Only an answer that **names** a missing permission saves a key this way. Any
other refusal, a region OpenAI does not serve for instance, verifies nothing
about the key, and nothing is saved.

Not verified: that answer is recognised by the words OpenAI uses for a missing
permission ("insufficient permissions", "missing scopes"), which OpenAI does
not document as stable. If the wording changes, a restricted key is not saved,
and the wizard shows OpenAI's sentence.

## What is stored, and what is never shown

The key crosses the wire once, inside your authenticated session. The node that
receives it seals it under the cluster's master key (`MEMQL_MASTER_KEY`), the
way a package's source credential is sealed, and discards the value. After
that:

- no screen, builtin, event, log line, decision record or message between
  nodes carries the key or its sealed form;
- the key is unsealed only inside one request to the vendor, on the node
  making that request, into a value that is dropped when the request ends;
- a **fingerprint** (the last four characters) is kept so you can tell two
  keys apart. Only the key's owner and a cluster owner see it;
- a vendor's error text is stripped of anything shaped like a key before it is
  stored, logged, shown or mailed. A vendor echoes part of a refused key.

A key is two rows. `v1:router:vendorKey` holds what its owner decides: vendor,
name, the sealed value, default, the Auto switch, lending and removal.
`v1:router:vendorKeyState` holds what the engine observes: the last check, a
refusal, a hold for credit, the last use. No client reads or writes either row
directly, and neither publishes a graph event. Every path to a key is one of
the builtins [below](#builtins).

A key never reaches a machine, an app session or a workbench. Vendor calls a
key pays for are made by the cluster.

## Which account pays

A route or a rule chooses the **model**. The account is only who pays for it.
For a call resolved to a text model at a vendor, the accounts are tried in
this order:

| The call | Accounts, in order |
|---|---|
| Is the cluster's own work, and says so (a schedule, a sweep, a seed, an automation, a connector, the operator's stream, or a job the cluster runs for everyone, such as the command-safety check) | the cluster's account only |
| Says neither whom it is for nor that it is the cluster's (code that lost track of who started it, or an anonymous visitor) | none: the call is refused, and the record says it names nobody |
| Runs under a person's authority borrowed for a **visitor** (a shopper form, an access-code call, a deployed workflow acting for somebody who is not a user) | the cluster's account, when the person whose authority is borrowed may use it. Never a key |
| Was made on a route the person **chose** (a route picked in Ask, a route of their own, a policy named as a route), or on a source they pinned themselves | their default key at the vendor, then keys lent to them (the key added earliest first), then the cluster's account **when they may use it** |
| Anything else: **Auto**, and every call nobody chose a route for | the same order, counting only keys whose owner turned on **Let Auto use this key** |

A choice counts only when the acting person made it, **themselves**:

- An agent's stored pin is its owner's, and a run shared with another person
  keeps its opener's route, so neither is that person choosing.
- A choice made through a credential that acts as a person without being them
  is not theirs either: an **app session** (Claude Code or Codex on their own
  machine, which can name a source on a turn, on a goal it opens or on a step
  it re-runs), a badge, **delegated access**, or the cluster working on their
  behalf. The choice is still attributed to them, and their key pays for it
  only under the Auto rule, which is the consent for work nobody chose in
  person. A personal access token is the person's own credential and counts.

Your keys at a vendor other than your default are not spent by this order.

An account is **passed over**, with its reason on the call's decision record,
when:

- the vendor refused the key, until a check or a served call says otherwise;
- its account is out of credit or at a spending limit, for one hour;
- the key's owner is no longer an active user of the cluster;
- lending is paused, for that person or for everyone;
- for the cluster's account: the cluster has not signed in with that vendor,
  the account does not pay for this person's calls, or the call names nobody.

A reason never names a key. The decision record is readable by every signed-in
person, so it says "an API key was refused by Anthropic" and carries the key's
id, which only a person who may see the key can resolve to a name.

When no account is left, the model is unavailable for that person and the
record says why for each.

**A failing account is followed by the next account for the same model.** When
the vendor's answer is about the account (not accepted, no permission, no
credit, a spending limit), the call moves to the next account in the order
above before the route moves to its next model. A rate limit or a vendor outage
is not about the account and follows the route's ordinary fallback. Neither is
this cluster's own cost ceiling stopping a call: that is the cluster's budget,
and it is never recorded against a key.

**Text model calls only.** Speech, transcription, embeddings and vision take no
key: they are the cluster's account or nothing. Embeddings bind the cluster's
shared memory search to one model and must not follow a person's key.

## The API route

Ask offers **API** beside Auto, Local and Apps: vendor models for every job,
paid by the accounts above. It is the route to pick when you want your key to
answer. Auto is not: Auto spends a key only once its owner turned on
**Let Auto use this key**.

API is ready for you when, for each job, some vendor model that serves that
job has an account that can pay for you. With one key at one vendor it is
ready; the jobs are served by that vendor's models. While it is not ready, Ask
shows it with the reason and does not let it be chosen, and Fleet > Routing >
Routes says the same. [AI routing](ai-routing.md#routes-a-person-picks-in-ask)
has the route's rules, and how a model is chosen for a job.

## Who the cluster's account pays for

Model access on a cluster is bring-your-own: a machine, an app, a key. The
cluster's own account at a vendor is the one exception its owner can make, and
it is deliberately narrow. It pays for:

- **the cluster's own work**, when the call says it is the cluster's: an
  automation, a sweep, a seed, a connector, the operator's stream, or one of
  the cluster's jobs, such as the command-safety check;
- **a person's calls, only when that person may use it.**

**A call that names nobody is refused.** A call has to say whom it is for, or
that it is the cluster's own work. One that says neither (code that lost track
of who started it, or an anonymous visitor) is not paid by the cluster's
account, and its decision record says the call does not say whom it is for. A
call made inside a person's run is that person's, even where the run's step
carries no session of theirs: the run names its owner.

| Capability | Lets a person |
|---|---|
| `execute app:settings/clusteraccount` | have the cluster's account pay for their model calls |

It is seeded on the **owner** role and no other. The cluster owner gives it to
a person or a group in **Users > Access**
([grants](auth/access-model.md#grants-to-people-and-groups)). Everybody else
reaches a vendor with an API key, their own or one lent to them, or not at
all: for them a vendor model with no key is passed over, and the record says
the cluster's account does not pay for this person's calls.

- **The person judged is the one the call is for**, by what the cluster holds
  about them now: their role, their groups and their grants. Work continued on
  a person's behalf (a run, an agent, an app session) is judged by that
  person, never by the stand-in role such work runs under. A visitor's call is
  judged by the person whose authority it borrows. A person who is no longer
  an active user holds nothing.
- **It is asked again when the call is made.** A permission taken away between
  choosing a model and calling it is not spent.
- **It covers every kind of vendor call MemQL routes for a person**, not only
  text: a picture, speech, transcription and an image are the cluster's
  account or nothing, so for a person it is closed to they are served by a
  local model or not at all. That includes a picture described when a file is
  uploaded to the Library, and a vendor reached only because a local model or
  an app failed when it was called.
- **Embeddings are the exception.** The memory space is the cluster's, one
  model for everybody, and a person whose writes were not embedded would be
  missing from everybody's search. Embeddings are paid by the cluster's
  account for every caller.
- **Not covered:** a cluster whose operator set `MEMQL_STT_PROVIDER` to one of
  the direct OpenAI transcription paths sends everybody's live transcription
  to OpenAI on the cluster's account. That variable is the operator's own
  choice of who pays; leave it unset to have transcription follow this rule.

A cluster with no vendor account connected is unaffected: nothing is asked.

## Auto

A new key is **off for Auto**. Choosing a route in Ask, using a route of your
own, or pinning a vendor model is you telling MemQL to spend your key. Auto is
not: under Auto the cluster decides where a call goes, so it spends a key only
after that key's owner turns on **Let Auto use this key** on the key's page.

The switch is per key and covers everybody the key serves: you, and the people
it is lent to.

## Lending a key

On the key's page, **Lend this key**:

| On the page | Stored as | Who the key pays for |
|---|---|---|
| Off (the default) | `owner` | its owner |
| On, with people and groups | `people`, with `userIds` and `groupIds` | its owner, the listed people, and the **active** members of the listed **active** groups |

There is no way to lend a key to everyone on the cluster. A key is one
person's money.

Each person and group on the list can be **paused** without being removed, and
the whole list can be paused at once. A pause and a change to the list take
effect on that person's next call, on every replica. Group membership is read
the way a lent machine reads it: leaving a group, or archiving one, ends its
members' use at once where the change is heard and within 30 seconds at the
latest. A share names at most 50 people and groups.

A person a key is lent to:

- pays with it for their own calls, after their own default key and before the
  cluster's account;
- sees a short card of it under **Settings > API keys**: its name, its
  vendor, whose it is, and whether it works. Never the
  key, its fingerprint, its settings, or who else has it;
- cannot lend it on, rename it, check it or remove it.

You are billed by the vendor for what they use. The key's page shows this
month's calls and estimated cost for each person
([what it cost](#what-it-cost)).

Only the key's owner can lend it. A cluster owner cannot.

## Checks, and what they cost

A key is checked without spending a token:

- **When it is added**: the vendor's models are listed with it.
- **By every real call it pays for**: a served call marks it working (stamped
  at most once an hour); an answer about the account marks it **Refused** or
  **Out of credit**. A rate limit or an outage says nothing about the key.
- **Once a day**: at 04:40 UTC the cluster lists the vendor's models with every
  active key that has had no served call and no check for a day, refused keys
  included, so a key the vendor accepts again recovers by itself. Keys that may
  not list models are skipped, and so are keys held for credit, because a
  listing is answered whether or not the account has credit.
- **Check now**, on the key's page, for its owner or a cluster owner. On a key
  that is out of credit a successful check ends the one-hour hold, so the next
  call tries the key, and does not mark it working: a vendor lists its models
  whether or not the account can pay.

The daily check is the cluster using a person's key. It sends no prompt and is
not billed, which is what lets the cluster do it.

| On the screen | Stored as | Means |
|---|---|---|
| Working | `good` | the vendor served a call or a check with it |
| Refused | `refused` | the vendor does not accept it. Passed over until a check or a call succeeds |
| Out of credit | `credit` | its account is out of credit or at a spending limit. Passed over for one hour, then the next real call tries it |
| Not checked yet | `unchecked` | nothing is known yet. It pays like a working key |

## When a key stops working

A key that stops working does not stop anybody's work: the call moves to the
next account that can pay. So the key's owner is told by email, **once per
fall**:

- to the key's owner only, at their address on the cluster;
- within about ten minutes: a pass on the cluster looks every ten minutes for
  active keys that are refused or out of credit and whose owner has not been
  told since the key last worked;
- naming the key by its name and vendor, quoting the vendor's answer with
  anything key-shaped and any web address removed, and saying where to go in
  words (Settings, then API keys). The message carries **no link**, and nothing of
  the key;
- not again until the key has worked and then stopped again. A key refused on
  every call for a week is one message; so is a key that goes from refused to
  out of credit.

No message is sent for a key that works again before the pass reaches it, for
a key its owner removed, or for a key whose owner is no longer a user.

If the message cannot be sent it stays owed and is tried again half an hour
later, for as long as the key is active and still not paying. The key was
passed over from the moment the vendor refused it, whatever happens to the
mail.

Every fall and every recovery is also an audit event
([audit](#audit)).

## What it cost

The key's page shows **this month's** calls and estimated cost, in total and
for each person who used it. The month is the calendar month in UTC.

The cost is **MemQL's estimate** from each model's listed price in this
cluster's catalog. It is not the vendor's invoice. A model with no price
listed reads "Not priced" and is left out of the total.

A key's spend is metered spend. The process cost ceiling, a run's ceilings and
the loop caps count a call a key paid for exactly as they count one the
cluster's account paid for ([LLM cost control](../ai/llm-cost-control.md)). A
person's monthly budget counts the calls that person made, whoever's key paid.
There is no cap per key.

## Remove a key

Removing is one-way. A removed key stops paying at once, is never unsealed
again, and cannot be restored: add a new one.

- The row stays, as the record of what paid for the calls that name it.
- **Removing a key here does not revoke it at the vendor.** Delete it in the
  vendor's console too. The sealed value remains in the row's history.
- A key also stops serving when its owner stops being an active user of the
  cluster.
- **A key never changes owner.** Transferring a person's rows to somebody else
  (`transferRowOwnership`, used when somebody leaves) moves neither their keys
  nor the engine's record of them, and the engine refuses any write that would
  change a key's owner, a cluster owner's included. A leaver's keys stay
  theirs and stop serving.

## What a cluster owner can do

A cluster owner can stop any key on their cluster, and cannot spend one:

| | Its owner | A cluster owner | A person it is lent to |
|---|---|---|---|
| See its facts (never the key) | yes | yes, by choosing "Every key on this cluster" | a short card |
| Check it now | yes | yes | no |
| Remove it | yes | yes | no |
| Rename it, change the default, switch Auto | yes | no | no |
| Lend it, pause lending | yes | no | no |
| Read who it is lent to | yes | how many, not who | no |
| Read what it cost | yes | no | no |
| Pay with it | yes | only if it is lent to them | yes |

## Reading which account paid

Every model call writes a decision record (`v1:router:call`;
[reading what the router decided](ai-routing.md#reading-what-the-router-decided)).
For a vendor call it says who paid:

| Field | Says |
|---|---|
| `accountKind` | `cluster` or `key` |
| `accountKeyId` | the key's id, for `key` |
| `accountKeyOwnerUserId` | the key's owner |

It never carries a key's name or fingerprint. Each entry of `considered` that
was an account says which kind it was and why it was passed over, and a
`fallback_used` row names the account that failed.

**When a call a key paid for fails, the record says why in MemQL's own words,
never the vendor's.** Its `errorMessage` is a code and one sentence, for
example `account_credit: the account that paid is out of credit or at its
spend limit` or `rate_limit: rate limited`. The vendor's own sentence is about
the key owner's account: it can name their organization, their plan or a part
of their key, and the record is readable by every signed-in person. The key's
owner reads the vendor's words on the key's own page. The same holds for the
refusal a caller gets when the cost ceiling stops the next account from being
tried. A call the cluster's own account paid for keeps the error's text, as
before.

## Across replicas

- Which account pays is decided per call from rows read at that moment, on
  whichever replica makes the call. Nothing about a key is remembered in a
  process: a removal, a pause or a refusal applies everywhere by the next call.
- The node that makes the vendor call decides which account pays and unseals
  the key there. A call forwarded between nodes carries nothing about an
  account, and never a key.
- What a call learns about a key is written under a lock on that key and
  dated when the request was sent, not when the answer arrived, so a long call
  that began before a key was revoked cannot report it working afterwards.
- The daily check and the pass that mails owners each run on one bff replica.

## Builtins

All on the stream. None returns a key.

| Builtin | Does | Who |
|---|---|---|
| `vendorKeyAdd { vendor, label, key }` | checks, seals and stores a key | `execute app:settings/apikeys` (every predefined role), in a person's own session |
| `vendorKeysForActor { all }` | your keys, short cards of keys lent to you, and for a cluster owner who asks, every active key | any signed-in person, whatever their role holds |
| `vendorKeyUpdate { keyId, label, isDefault, autoUse }` | the owner's settings | the key's owner |
| `vendorKeyCheck { keyId }` | lists the vendor's models with the key | the key's owner, or a cluster owner |
| `vendorKeyRemove { keyId }` | removes it | the key's owner, or a cluster owner |
| `vendorKeyShareDirectory { keyId }` | who it can be lent to, and who it is lent to now | the key's owner |
| `vendorKeySetSharing { keyId, mode, paused, userIds, groupIds, pausedUserIds, pausedGroupIds }` | lends it, pauses, takes it back | the key's owner |
| `vendorKeyActivity { keyId }` | this month's calls and estimated cost, per person | the key's owner |

Two more are the cluster's own and refuse every caller but the engine's
scheduled automation of the matching name: `vendorKeysSweep` (the daily check,
`vendorKeysDailyCheck`) and `vendorKeysTellOwners` (the mail,
`vendorKeysOwnerNotices`).

## Audit

Each of these writes an audit event with `targetType: vendorKey`:
`vendor_key_added`, `vendor_key_renamed`, `vendor_key_default_changed`,
`vendor_key_auto_changed`, `vendor_key_removed`, `vendor_key_lent`,
`vendor_key_lending_paused`, `vendor_key_lending_resumed`,
`vendor_key_lending_stopped`, `vendor_key_went_bad` and
`vendor_key_recovered`. None carries the key, its sealed form or its
fingerprint.

## What is not built, and what was not verified

Not built:

- a key for any vendor but Anthropic and OpenAI;
- a key paying for speech, transcription, embeddings or vision;
- lending a key to everyone on the cluster;
- a spending cap per key, or a budget per person the key is lent to;
- revoking a key at the vendor from here;
- naming one of your non-default keys in a route. Until that exists, a
  non-default key is never spent.

Not verified:

- **No part of this was run against a real vendor with a real key.** The
  vendors are stood in for by test servers that answer as their documentation
  describes. The first real key added on a cluster is the first real check.
- The restricted-key answer ([above](#a-key-that-may-not-list-models)) is
  matched on wording.
- The mail was read in a local cluster's captured mail, not delivered through
  a real mail system.

## Related

- [AI routing](ai-routing.md): how the model is chosen before the account is
- [LLM cost control](../ai/llm-cost-control.md): the ceilings every call is counted against
- [Sharing a machine](shared-machines.md): the lending model a key reuses
- [Access model](auth/access-model.md): parts, roles and grants
- [Anthropic federation](auth/anthropic-federation.md) and
  [OpenAI federation](auth/openai-federation.md): the cluster's own account

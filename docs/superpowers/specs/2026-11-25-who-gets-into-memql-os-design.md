# Who gets into MemQL OS: the cluster decides entry, a person's model access decides Ask

Design record, 2026-11-25. Status: built on branch `wip/who-gets-in-fix-nov25`,
not released. The rule is the owner's, from a recorded call on 2026-11-25. The
points the call left open were decided by default and are listed under
[Taken by default](#taken-by-default-for-the-owner-to-confirm) for the owner to
confirm or overrule.
The operator's guide is `docs/public/operate/auth/access-model.md`, "Who gets
into MemQL OS".

**Replaces** these parts of
[2026-11-24-desktop-access-design.md](2026-11-24-desktop-access-design.md):
owner's decisions 1 and 3, the anti-goal "A person with nothing set up reaches
the desktop", section 4 (the page) and the acceptance tests "Held, with the
reference", "Lifting" and "No desktop while held". Everything else in that
record stands, and this record relies on it: what counts as model access set up
(section 1), who is exempt (section 2), the engine's check (section 3), Ask's
messages for access that is asleep (section 5), and the rest.

## Why

Release v0.31.41 was built from the 2026-11-24 record. It decides entry per
person: anybody who is not a primary owner or developer and has no model access
of their own gets a page in place of the whole desktop, on a cluster that is
fully set up too. Ask is shown to everybody who holds the Ask app, whatever
their model access.

On 2026-11-25 the owner described the rule he had asked for, and it is a
different one. His words, from the recording of the call that started at
12:35 PM, with the time into the recording:

- 0:57 "Carlos no va a agarrar nada de inference. He's not paying for any of
  that. He's not getting the Ask app."
- 1:37, asked whether a person without a model is blocked from entering: "No,
  no es correct. What it does, it lets you in, or at least that's what I told
  it. It should let you in, but if you have no inference access, then you don't
  even get the Ask app."
- 2:39 "Si no hay un modelo embedding, nadie debe tener acceso except primary
  owners y developers. So, if the cluster is not configured yet correctly, they
  will get this message saying that they need to contact their admins."
- 3:15 "Once it's configured and good, people should be able to then access the
  app. However, if they don't have LLM inference, they won't get the Ask app."
- 3:43 "The Ask button itself should be gone... it shouldn't even show up
  there. Or we completely disable it."

The first person the built rule would have kept out is a client's moderator who
is to get no inference and needs Files. Under 0.31.41 he could not reach Files
at all.

## The rule

| Situation | What happens |
|---|---|
| The cluster is set up. Any signed-in person | Enters MemQL OS, whatever their own model access |
| A person with no model access, inside the desktop | No Ask: no button in the dock or the phone bar, no Ask widget, and nothing else opens it |
| The cluster is not set up. A person who is not a primary owner or developer | Kept out, on the page "MemQL is not set up yet" |
| A primary owner or developer | Always enters, to set the cluster up, and keeps Ask |

Two questions, asked separately:

1. **Is the cluster set up?** This alone decides who enters. It is the
   cluster's `ai` readiness verdict, the one the gate has read since 0.21.
2. **Does this person have model access?** This alone decides whether they get
   Ask. It is the engine's `desktopAccess` answer, built for the 2026-11-24
   record and unchanged.

## What changes from the 2026-11-24 record

| 2026-11-24 | 2026-11-25 |
|---|---|
| Decision 1: everyone except the primary owners and developers needs model access to enter | Nobody needs model access to enter. It is needed for Ask |
| Decision 3: the person sees a page telling them to ask an administrator, with a reference | No page for a person. The page is for a cluster that is not set up, and it shows no reference |
| Anti-goal: a person with nothing set up reaches the desktop | That is now the rule |
| Section 4: the held page replaces "MemQL is not set up yet" and holds whether or not the cluster is set up | The held page is removed. "MemQL is not set up yet" is back for everybody but the primary owners and developers |
| An unconfigured cluster holds only owners and developers; anybody else with access of their own enters | An unconfigured cluster keeps everybody else out, whatever access of their own they have |
| Ask is shown to everybody who holds `read app:ask` | Ask also needs model access set up |

## What stays

- **What counts as model access** (2026-11-24, section 1 and decision 2): a
  machine, an app, an API key, or the cluster's account where it pays for the
  person. Access that is set up and cannot answer right now still counts: a
  lent laptop that is asleep, a key out of credit, an app out of usage. Such a
  person keeps Ask, and Ask says what is asleep, as it does today.
- **Who is exempt** (section 2 and decision 4): the primary owners and
  developers, by their stored role. `admin` is not exempt, and nor is an owner
  or developer of one account.
- **The engine's check** (section 3): the `desktopAccess` builtin, its row, the
  line it writes to the log store with the reasons, the reference and digest a
  re-check passes back, and the bound on writes. None of it changes. Only its
  meaning for MemQL OS does: an answer with `admitted: false` used to keep the
  person out, and now takes Ask away.
- **A check that fails** answers "unknown" and takes nothing away: the person
  keeps Ask, and the server still refuses their model calls.
- **The grant that denies Ask** (section 6 and decision 5) still works and is
  still the way to take Ask from somebody who does have model access.
- **The API, the SDK, the VS Code tools and Cockpit** are not closed to anybody
  by this rule. It is about MemQL OS.

## Taken by default, for the owner to confirm

The call left these open. Each was decided the way the owner's words lean, so
the work could be finished. Any of them can be overruled.

1. **Ask is gone, not disabled.** His first and repeated wording was "you don't
   even get the Ask app" and "the Ask button itself should be gone". "Or we
   completely disable it" was offered once as an alternative.
2. **"No inference access" is the engine's existing answer.** A person has no
   inference access when `desktopAccess` finds no source of any kind for them.
   A source that is asleep or out of credit counts as set up, so Ask stays,
   with its existing message. No new check was built.
3. **On a cluster that is not set up, an API key of one's own does not let
   anybody in** except a primary owner or developer. His words were "nadie...
   except primary owners y developers". Release 0.31.40 let a person in on a
   key of their own; this does not.
4. **"The cluster is not set up" is the readiness verdict the gate already
   reads**, `ai` = `unconfigured`. No new readiness check was built. See
   [what the verdict checks](#what-cluster-not-set-up-checks): it is more than
   "no embedding model".
5. **The page is the one release 0.31.40 had**, with its words: "MemQL is not
   set up yet", "An owner or developer has to set up inference before anyone
   can use it.", "This page opens by itself once they have.", and Sign out. No
   new screen was designed. The page does not say "contact your admin" in those
   words.
6. **The page shows no reference**, and offers no "Add an API key". Whether the
   `ERR-` reference and its Logs line are wanted on this page is the owner's to
   say.

## What "cluster not set up" checks

The `ai` verdict is `configured` when BOTH hold
(`readiness.InferenceConfigured`, `component/memql/readiness/inference.go`):

- **one way to a chat model**: a machine that is not revoked and advertises a
  model with structured output and a context of at least 8192, or a machine
  with Claude Code or Codex allowed and signed in, or the cluster's own vendor
  account (workload identity federation);
- **an embedding model**: a bound embedder, or a machine that advertises an
  embedding model.

It is `unconfigured` when every node that voted found one of the two missing.
So the owner's "si no hay un modelo embedding" is half of it: a cluster with an
embedding model and no way to a chat model is also not set up. A machine that
is asleep still counts as configured. Nobody's API key counts: a key is one
person's, and the verdict is the cluster's.

Three other answers do not keep anybody out, as before: the feed has not
loaded, no live node reported (`unreported`), and nodes disagree (`partial`).

## How it is built

**The gate** (`clients/os/src/chrome/CoreGate.tsx`) asks one question, the
cluster's. On `unconfigured` it shows the setup screen to a primary owner or
developer, as before, and the page "MemQL is not set up yet" to everybody else.
On any other verdict everybody enters. The role is the one the session read
from the cluster (`canConfigure`), as in 0.31.40.

- An owner or developer whose own API key can pay still passes the setup
  screen, and the screen still steps aside for a window it opened. Both are
  unchanged.
- The page for everybody else does not step aside for open windows: nothing on
  it opens one. A cluster that stops being set up while such a person works
  shows them the page at once.
- While the connection is away the session's identity is cleared. With a
  window open the desk stays as it is until the identity is back, so an owner
  working in a window is not shown the other page for the length of a
  reconnect. With no window open nothing is drawn, as before.
- The opening frame still waits up to three seconds for the person's first
  access answer, for anybody not known to be an owner or developer. It no
  longer decides entry. It is kept so the desk is first drawn knowing whether
  Ask belongs on it, and a person with no model access does not see Ask appear
  and vanish.

**The access answer** (`useDesktopAccess`) is read once, above Ask
(`DesktopAccessScope`, mounted in `Shell.tsx`), where the gate, Ask's provider
and the roster all read it. It asks again on focus, when the connection
returns, and every 30 seconds while the person has no model access and the tab
is visible, so Ask appears by itself within 30 seconds of an administrator
lending a machine or a key. It still passes its reference back, so those
re-checks write no new log line.

**Ask** has one switch, `canAsk` (`ask/AskProvider.tsx`): the person holds
`read app:ask` AND the engine did not answer that they have no model access.
The dock's button, the phone bar's button, the sheet, every "Ask about this"
control and the links from Nexus all follow it already. The Ask widget is a
registry entry, not a child of that provider, so the roster
(`ShellRoster` in `Shell.tsx`) leaves it out of the registry for such a
person: the desk draws none and neither the desk menu nor the launcher offers
one. `holds()` is not touched: it holds the engine's answer about grants and
nothing derived.

**Removed:** the held page (`chrome/AccessHeld.tsx`), its style rule, and the
key wizard's mode for running on that page.

**The engine** is not changed in behavior. Its log line's message no longer
says the person was held: it says no model access is set up and that MemQL OS
shows them no Ask. The wire field `admitted` keeps its name; renaming it would
touch the builtin, the concept, both generated SDKs and the concept snapshot
for no change in behavior.

## Known limits

- **The reference is written and not shown.** The engine still mints the
  `ERR-` reference and writes the line; no page shows it now. An administrator
  finds the line in Logs by component `desktopAccess` and the person's user
  id. If the owner wants no reference at all, removing it is engine work this
  record does not do.
- **A key added in Settings takes up to 30 seconds to bring Ask**, or the next
  return to the window. The held page asked again at once after a key was
  added on it; nothing asks at once now.
- **Profile > Ask** (the Ask preferences page) is still listed for a person
  with no Ask, as it already was for a person denied Ask by a grant.
- **A person who is let in for three seconds of silence** (the access answer
  did not come within the bound) sees Ask until the answer lands.
- **Enforcement is the server's, as before.** MemQL OS hides Ask; the server
  refuses every model call for a person with no access. A call made outside
  MemQL OS is refused there, not hidden.

## Tests

- **The four rows.** `clients/os/test/chrome/coreGate.test.tsx`: a person with
  nothing set up enters a cluster that is set up; everybody but an owner or
  developer is kept out of a cluster that is not, with access of their own or
  without; an owner and a developer get the setup screen and enter a cluster
  that is set up.
- **Ask.** `clients/os/test/system/readinessReactivity.test.tsx`, through the
  real shell: no Ask button and no Ask widget for a person with no model
  access, on the desktop and on a phone; Ask for an owner with nothing set up;
  Ask for a person whose only source is asleep or out of credit.
  `clients/os/test/ask/access.test.tsx`: no command, link or sheet opens Ask
  for a person with no model access.
- **The access answer.** `clients/os/test/chrome/desktopAccess.test.tsx`: the
  wait at the door, an unknown answer, asking again on focus, on reconnect and
  every 30 seconds, the reference passed back, and a change of person.
- **The engine.** `component/memql/desktop_access_hold_test.go`,
  `desktop_access_db_test.go` and `desktop_access_test.go` pin the answer and
  are unchanged; `component/logstore`'s
  `TestRecordAccessHoldWritesTheDocumentedLine` pins the line's new message.

Not verified: any of this on a running cluster. The tests above run against
doubles and, for the engine's database cases, a throwaway database.

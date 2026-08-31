# MemQL OS -- the Roaming Desktop -- Design

- **Date:** 2026-08-31
- **Status:** approved (epic memql#4746; the engine-side record the epic asked
  for before decomposition, written alongside the implementation)
- **Scope:** a new `dsl/os/` namespace (`v1:os:desktop`), one mesh routing
  rule, one narrow fix in the mutation-template evaluator, and the client half
  in `clients/os/` (`GraphDesktopStore`, document adoption, id minting, the
  dock's report). No proto changes, no wire-contract changes, no frontend-team
  ping -- this IS the frontend, and the engine additions are new constructs
  rather than changes to existing ones.
- **Depends on:** epic memql#4710 (the foundation), which defined
  `DesktopStore` (spec D11) and the desktop document, and deliberately did not
  mint a concept for it.
- **Issues closed:** #4746 (epic), #4759 (engine), #4760 (store), #4761 (shell).

## Why

The foundation persists the desktop per browser, in `localStorage`. MemQL is a
memory graph; the desktop of its OS should be a row the owner's other machines
resolve. Arrange a desk on the laptop, sign in from the desktop machine, and
find the same desks, items, pins and theme.

## The shape

```
  browser A                     cluster                    browser B
  ---------                     -------                    ---------
  DesktopStore.save() --debounce--> saveMyDesktop -----> graph.node.created
        |                        (v1:os:desktop)                |
   LocalDesktopStore                                     re-read myDesktop
   (always, first)                                              |
                                                        adopt + report once
```

## D1 -- one row per person, by a derived id

`saveMyDesktop` is a single `insert{}` whose id is `hash(actor.userId)`.
`insert{}` is create-or-upsert at the engine's one write chokepoint
(memql#1709), so the first save creates and every save after overwrites.

The alternative -- a caller-minted id with a create/update pair, which is what
`v1:worker:routingPolicy` does -- holds "one row per owner" on the WRITE side,
with an editor that read first and a comment explaining why that is safe. It
is safe there. Here it would not be: two tabs opening at once both read "no
row" and both create, and there is nothing to choose between the results.

The id is unprefixed per authoring-rules.md section 20. It is hashed rather
than used directly because an actor id is canonical
(`v1:identity:user:<slug>`), and `core/id.ValidateShortId` refuses a canonical
id carrying a foreign concept's prefix.

## D2 -- the plain owner tier, without `clusterOwner`

Every other owner-tier concept in this tree takes the composite, because an
operator has to support the person: "why is this workbench node full", "why
did this call route there". A desktop layout answers no such question. The
composite would let a cluster owner read where somebody put their icons and
buy nothing.

## D3 -- last writer wins, on a revision the client stamps

A save writes `held + 1`. Two machines saving from revision 5 both write 6 and
the second lands. The conflict UX is a cue, not a merge editor -- stated in the
epic and not revisited.

The client could not have used a server-side counter: the number only means
something relative to the document the caller was looking at, and "the write
happened" is not the fact the reader needs.

**The echo test is the DOCUMENT, not the revision.** Our own save comes back as
an event; adopting it would push it into the shell, whose state change would
save it again -- a write per machine per round trip, forever. Comparing
documents ends that exactly, and stays right when two machines mint the same
revision.

Both sides are sanitized and canonicalised with sorted keys, because a
document that has been through JSONB has no key order and has been through
`sanitizeDocument`; comparing raw would differ on every save.

## D4 -- the active desk rides along with a save but never causes one

This is the second half of the same loop, and it is not obvious.

The shell KEEPS the desk it is showing when it adopts a document (teleporting
somebody because another machine paged is hostile), so the document it rebuilds
differs from the row in exactly `activeDeskId`. Compared whole, each machine
saves the other's arrival straight back and the pair ping-pong a revision per
round trip with nothing having happened.

So the write test excludes `activeDeskId`. It is still STORED, because a cold
sign-in on a new machine has no position of its own and landing where you left
off beats landing on desk one. What it cannot do is BE the change.

## D5 -- nothing is written until the first read resolves

A browser signing in for the first time has no local document, so the shell
seeds one and saves it. If that save reached the cluster it would overwrite the
desktop this person built on their other machine with an empty one, in the
first hundred milliseconds, before they saw anything.

Saves are therefore buffered until the read answers, and the answer decides:

- **a row exists** -- it is adopted and the buffered document is dropped with
  it. This is the epic's "thereafter graph wins", applied to the boot window.
- **no row exists** -- the buffered document is written. That IS the
  first-sign-in migration: not a special path, just the first ordinary save,
  held until we knew.

## D6 -- a row this bundle cannot read stops the writer

A newer document version in an old tab: `sanitizeDocument` rejects it, and the
session stops writing to the graph rather than replacing it with a downgrade.
Local keeps working, and the dock says the tab is out of date. The safe
direction is the one where the newer desktop survives to be read by the reload.

## D7 -- adopting is not loading

`stateFromDocument` builds a shell with no windows, because at boot there are
none. Applied to a running shell it closes everything the person has open -- in
response to something that happened on a different computer. `adoptDocument`
keeps the windows (a window is session state the document has never carried,
spec D11) and keeps the desk on screen when the arriving document still has it.

## D8 -- ids minted against the document

`nextId` is one module counter, shared by every prefix, that starts at 0 on
every page load. The document that loads with the page does not. So a fresh
session seeds `desk-1` then `item-2`; after a reload the counter hands out
`item-1`, then `item-2` -- and items live in a map keyed by id, so the second
folder created after any reload REPLACED the seeded Ask widget, with no error
anywhere.

That was a live defect in the foundation. Roaming turns it from a reload bug
into the normal case: every machine mints from the same low numbers, so every
document arriving from elsewhere is full of ids this session is about to hand
out again. `nextIdAvoiding` skips what the desktop already holds; the counter
stays, so `desk-1` in a failure message is still `desk-1` on a fresh session.

## D9 -- the report is not a toast

This app says so twice in its own source (`ask/AskSurface.tsx`,
`ask/sdkTransport.ts`): a report belongs where the thing it reports on is
shown.
The dock's status cluster already carries the connection dot, which describes
the same relationship between this desktop and the cluster, so the roaming
report goes beside it -- muted, small, self-clearing, `role="status"` mounted
before there is anything to announce.

## What the engine gained on the way

`actor.userId` rendered at the top of a value slot and died inside a call.
`id: actor.userId` lowers to `ArgRefExpr`, which the mutation-template
evaluator has resolved since memql#2840; the same reference as a function
ARGUMENT lowers to `SpecReferenceExpr`, which had no case at all. So an
actor-derived id passed memqllint, passed strict boot, and failed at render on
every call with `unsupported expression in mutation template`.

That is the memql#2909 / memql#2925 class exactly -- lint-clean in the `id:`
position and unrenderable there -- and the third time this evaluator has been
found missing a node the grammar accepts. The fix handles the `actor.` prefix
only; a `SpecReferenceExpr` naming a row field still refuses, because a
mutation template has no row to read it from.

**It was found by a database test, and could not have been found without one.**
Every DB-free test around a mutation hands the engine a Go map. The engine
takes MemQL TEXT, and the SDK renders it -- so the gate here builds a desktop
document carrying quotes, backslashes, newlines and astral characters (a folder
is named by its owner; a file is titled by whatever was uploaded), renders it
through the shipped `SaveMyDesktopBuild`, and executes the string.

## Known residual: a gap across a reconnect

The store re-reads on a CDC event and on nothing else. The SDK owns reconnect
and replays subscriptions (memql#4537), so a dropped socket costs no
subscription -- but an event that fired DURING the outage is simply missed, and
this machine keeps the document it holds until the next event or the next save.
Its next save then wins by last-writer-wins, over a document written while it
was away.

That is the declared model rather than a defect -- the epic scopes out
"offline-first reconciliation beyond last-writer-wins" -- and the fix, if it is
ever wanted, is one line in the shape the rest of the codebase already uses:
re-read on the connection's status returning to `connected`, the way
`LiveCollection` re-seeds on a gap. It is not built here because it would put a
connection-status dependency through the gateway seam for a window whose cost
is one desktop layout.

## Not in scope

Per-device desktops; sharing a desktop between users; offline-first
reconciliation beyond last-writer-wins; a scheduler for anything. All named as
out of scope by the epic and unchanged by this record.

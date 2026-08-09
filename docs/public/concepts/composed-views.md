---
title: Composed Views
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# Composed Views

**Status:** authoritative reference

A **composed view** is a screen somebody built for a concept nobody
designed a screen for. They pick a concept, the system works out which
elements can render it, an arrangement appears, they adjust it, and they
save it. No code is written and none is deployed.

It is the third and last tier of the view system, and the one the other
two exist to make possible:

| Tier | Who chose the arrangement | Where it lives |
|---|---|---|
| The concept browser | nobody -- one generic rendering | `clients/portal/src/concepts/` |
| A [predefined view](view-elements.md#7-predefined-views-and-the-line-they-may-not-cross) | a designer, in code | `clients/portal/src/views/` |
| **A composed view** | **a person, at runtime** | **a row in `v1:portalviews:view`** |

All three render through the same [element library](view-elements.md) and
the same [display-card contract](display-cards.md). The only thing that
differs is who decided which elements go where.

---

## 1. The deterministic path, which is the whole product

**A composed view works before any model is involved, and it keeps
working when none is available.** That is not a fallback; it is the
main path, and the AI is layered on top of it.

```
   rows ──profileConcept──▶ ConceptProfile
                                  │
        ElementSpec.requires ─────┤ fitElements
                                  ▼
                          ranked, explained candidates
                                  │
                                  │ proposeArrangement
                                  ▼
                            Arrangement  ──▶ a rendering view
```

Every step is a pure function of the concept's rows:

1. **Profile** the rows -- per field, its kind, how many rows carry it,
   how many distinct values it takes, and which display-card slots name
   it.
2. **Fit** every element in the library against that profile. Each
   element declares what it needs (`a datetime field to plot events
   on`), so the answer is a match rather than a guess, and each
   unfitting element carries the element author's own sentence saying
   why.
3. **Arrange**: take the best-fitting element in each band, in band
   order.

The bands are the same grammar the predefined views follow by hand,
because a person arriving at an unfamiliar row set asks the same three
questions in the same order:

| Band | Question | Typically |
|---|---|---|
| `reading` | How many are there? | stat tiles |
| `shape` | How does that divide? | a proportion rail, a pie, a line |
| `roll` | Which ones, specifically? | a table, a timeline, a board, a list |

**A band is declared by the element, not by the composer.** `ElementSpec.band`
is metadata like `requires` is, and for the same reason: a list held by
whatever lays a page out would need editing every time an element was
added, and the element added would be the one nobody remembered to list.
An element written tomorrow takes its place in a composed view -- and in
the deterministic proposal -- the day it is written. Omitting the
declaration means `roll`, which is what all but a handful of elements do.

**The roll band always fills.** If nothing in it fits -- the honest case
is an empty row set, where every element is below its minimum -- the
library's universal fallback is used anyway, and rendering it produces
view-kit's own explanation of why it is empty. That is a better empty
state than a page with no elements, and it means an arrangement composed
against an empty concept is still correct the day rows arrive.

---

## 2. An arrangement is plain data

```ts
{
  conceptId: "v1:example:order",
  elements: [
    { element: "statTile", band: "reading", bindings: { metric: [] } },
    { element: "chart.proportion", band: "shape" },
    { element: "table", band: "roll", title: "Every order" },
  ],
}
```

That is the whole value. No behaviour, no positions, no pixel sizes:

- **`element`** is an `ElementSpec` id, resolved against whatever library
  the reader has. An id the reader does not carry is reported, not
  skipped -- a view outliving an element should say so.
- **`band`** is a question, not a place. A host decides what a band looks
  like, so a saved arrangement survives a redesign of the surface that
  renders it. Storing a grid position would pin every saved row to one
  release's layout.
- **`bindings`** is the caller-override half of the [fitness
  contract](view-elements.md#resolution-order): slot -> the field names
  to use. **An empty list declines a slot** -- which is how a view says
  "count the rows, do not total anything", and why an empty list has to
  survive storage rather than being tidied away.
- **`title`** overrides the band caption when the element's own name is
  not what the screen is about.

**The three producers all emit this same value.** The deterministic
match, a model's proposal and a person's edit are interchangeable
by construction, and that is what makes the AI optional rather than
structural: nothing downstream can behave differently depending on which
one ran, because nothing downstream can tell.

---

## 3. Where a saved view lives

A composed view is a row in `v1:portalviews:view` (`dsl/portalviews/`),
holding the concept selection, the arrangements, and its owner. Three
properties fall out of that and out of nothing else:

- **It is authorization-gated by the same per-row rules as everything
  else.** The concept declares `@rowAuthz(owner="ownerUserId")` and the
  field is `@serverSet`, so the engine stamps the owner from
  `actor.userId` on every write, ANDs `ownerUserId == actor.userId` into
  every read, and refuses an update whose target row the caller does not
  own. A client cannot compose a view owned by somebody else because
  there is no argument through which to try.
- **It is inspectable in the portal's own concept browser.** A composed
  view is browsable at `/concepts/v1:portalviews:view` like any other row
  set, with its own `@displayCard`, and the saved view page links
  straight to it.
- **It has version history**, because a write in memQL is an append.
  Editing a view keeps the previous arrangement.

**Sharing is not modelled, and the reason is honest rather than
temporary.** The granted authorization tier has no expressible predicate
today: there is no per-(view, person) grant row and no engine mechanism
to gate a read on one, so a "shared with" list would be documentation
that no filter enforces. A composed view is a single-owner record, which
is the shape the isolation model handles well.

**The stored arrangement is repaired on read, not trusted.** Rows change
shape, elements are removed from libraries, concepts stop being
published. Each section re-validates its stored arrangement against the
live rows before rendering: an entry that cannot render is dropped, and a
section left with nothing falls back to the deterministic arrangement for
those rows. The row is not rewritten by that repair -- what is stored is
what the person saved, and re-opening the view in the composer is where
they choose to make the repair permanent.

---

## 4. What the model does, and what it may not

A model is asked exactly one question: **given these already-fitting
elements, which of them belong on the screen and in what order?**

**It never decides candidacy.** The candidate list is computed
deterministically and handed to the model; an element it names that is
not on that list is dropped when the reply is parsed. It cannot widen the
space a person could have reached by hand.

**It never sees row values.** The request carries field names, kinds and
cardinalities -- not data. A layout decision has no business reading
somebody's records, and a payload of rows would put whatever the concept
holds in front of a provider for the sake of picking a chart.

**It is shown the answer that already works.** The deterministic
arrangement goes in the request as a baseline, so the model is improving
on something rather than inventing from nothing, and "the obvious
arrangement is the correct one" is a cheap, good reply.

**The reply is an object, not prose.** The call goes through the engine's
structured-output path, so the provider enforces a schema. Even then it
is read as untrusted: every entry is coerced, checked against the live
profile, and put through the identical validation a hand-built
arrangement gets. A proposal that survives is a view a person could have
built themselves.

**Declining costs nothing.** A proposal is rendered ABOVE the working
view rather than replacing it, so the thing it offers to replace stays
visible while it is considered. "Keep mine" is not an undo -- nothing was
applied.

### When there is no provider

Every failure -- no provider configured, the suggest domain not
registered, the call erroring, a reply that parses to nothing usable --
lands in one place and means one thing: there is no second opinion today.
The composer says which failure it was, because an operator can act on
"no such domain" and cannot act on "something went wrong", and then
carries on with the arrangement it already had.

This is a tested state rather than a claimed one:
`clients/portal/test/compose.test.tsx` runs its entire default harness
against a cluster whose suggest surface refuses, and reaches a saved,
rendering view through it.

---

## 5. Several concepts, and what that honestly means

A composed view may cover several concepts, as **independent stacked
sections**: one concept per section, each with its own row set, its own
profile, its own candidates and its own arrangement.

"Orders and customers on one page" works. **"Orders joined to their
customers" does not, and is not attempted.** A join needs the engine to
correlate two concepts in one read, and every query binds exactly one
concept in its signature -- so there is no construct a client can reach
that would produce a correlated row set. Doing it in the browser would
mean paging both concepts in full and joining them there, which is a
different and much worse thing wearing the same word.

---

## 6. The composer's addresses

```
/compose                        what you have composed, and what you could
/compose/new?concept=<id>&…     the composer, over a selection
/compose/<viewId>               a saved view, read
/compose/<viewId>/edit          ...reopened in the composer
```

The selection is a repeated query parameter because it is a list; a saved
view is a path because it is a single durable thing somebody will
bookmark. Both are URLs rather than component state, so a half-built
composer is a link you can send.

---

## 7. What makes a new concept work for free

Nothing in this system enumerates concepts. The picker lists whatever the
cluster's registry publishes; the profile comes from the rows; candidacy
comes from what each element declared it needs; the display card is the
concept's own or view-kit's documented inference. A concept declared
today is composable today, and the claim is pinned by a test that
synthesises a concept this repository has never seen, runs it through
profiling, fitting, arranging and rendering, and asserts a usable view
comes out -- with no code change and no entry in any list
(`sdk/ts-viewkit/test/arrangement.test.ts`, and end to end in
`clients/portal/test/compose.test.tsx`).

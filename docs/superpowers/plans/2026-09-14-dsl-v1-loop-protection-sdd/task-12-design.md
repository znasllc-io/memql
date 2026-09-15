# Design: Cluster > Automations (Task 12)

Produced with the frontend-design skill, inside the MemQL OS's own language:
`clients/os/DESIGN.md`'s twelve rules, the `kit`, and the brand tokens. Nothing here
introduces a new hex value or a new typeface. The OS is themed by tokens, and a
section that painted its own palette would break the theme packs.

## Subject, audience, job

- **Subject.** The automation graph of one MemQL cluster: which automation's writes
  start which other automation, in what order (strata), and where the loops are.
- **Audience.** The person who changes automations (developer, admin, owner) and the
  one asked "why did this run 16 times?".
- **Primary job.** Show the chain of reactions in order, and point at the loops:
  - the ones the load permitted
  - the edges the load could not decide
  - the chains the run time stopped

## Tokens (existing ones only)

| Role | Token | Use |
|---|---|---|
| Ground | `--os-plate`, `--os-raised` | Map canvas; alternate stratum columns banded with `--os-raised` at low contrast, so columns read without rules |
| Ink | `--os-ink`, `--os-muted` | Automation names; trigger sentences, captions |
| Lines | `--os-line`, `--os-muted` | Card borders; edge strokes at 1.25px |
| Accent | `--os-accent`, `--os-accent-soft` | The selected automation and its edges; a PERMITTED `@loop` cycle's enclosure |
| Caution | `--os-warn` | An UNDECIDED edge (dashed): the load could not read a filter against a write |
| Error | `--os-error` | The stop mark on a stopped chain; a refused cycle (only reachable under the break-glass) |
| Data voice | `--os-font-mono` with the brand data tints (`--memql-data-string` for concept ids, `--memql-data-number` for depths and counts) | Graph data, never chrome |
| Display | `--os-font-display` (Squada One) | THE ONE MOMENT: the stratum numerals heading each column |

## Type

- Inter (`--os-font`) carries every sentence.
- JetBrains Mono (`--os-font-mono`) carries identifiers: automation names on the cards
  (they are DSL construct names, read character by character) and concept ids.
- Squada One appears only as the stratum numerals, at `--os-text-xl`, in `--os-muted`.
  The strata ARE a sequence (stratum k reacts to earlier ones), so numbering them is
  information, not decoration.
- Sizes come only from the OS scale: `--os-text-xs`, `-sm`, `-base`, `-md`, `-lg`,
  `-xl`. Sentence case everywhere, and no all-caps labels.

## Layout

At `>= 900px`, the map and the detail sit side by side, each with its own scroller
(rule 11):

```
+ Head: Automations   "31 automations react in 3 strata. Read at 16:42."   [Read again] +
+------------------------------ map (flex: 1) -----------------+-- detail (340px) --------+
|  0                     1                     2              |  routeRequest            |
|  Fired from outside    Reacts to stratum 0   Reacts to 1    |  dsl/forge/automations   |
|  +-----------------+   +-----------------+                  |                          |
|  | routeRequest    |---| recordTransition|                  |  Runs when               |
|  | any write to .. |   | an update to .. |                  |  Stratum                 |
|  +-----------------+   +-----------------+                  |  Writes                  |
|                                                              |  Starts / Started by     |
+-- Stand alone (27) ------------------------------------------+  Calls the map cannot see|
|  bootstrapCluster  registerNode  pruneStaleClusterNodes ...  |  Loop / Mode             |
+-- Loops stopped ---------------------------------------------+                          |
|  16:02  a  b  a  b  ...  12 more  a  b  [stop] depth 17, past the cap of 16            |
+--------------------------------------------------------------+--------------------------+
```

Below 720px, the detail REPLACES the map, with a quiet `<- Automations` in the Head
(rule 11's second form). Nothing is squeezed into two cramped columns.

## The map

- **The layout is a PURE function** in `layout.ts`, from rows to
  `{columns, cards, edges, groups, size}`. It is tested with no DOM, and it SORTS ITS OWN
  INPUT, so a re-read that returns the same graph draws the same picture (the
  Deployables precedent).
- **Only automations with at least one edge are on the map.** The rest go to the
  "Stand alone" band below it, because a wall of 60 unconnected cards hides the
  structure the picture exists to show.
- **Columns.** One per stratum. Column width 220, gap 88 for the curves.
  - Each column is headed by its numeral (display face), with a caption under it:
    stratum 0 is "Fired from outside" (by a person, a schedule, or a topic nothing
    here publishes), and any other is "Reacts to stratum N-1".
  - Alternate columns get the `--os-raised` band, full height.
- **Cards.** 196 x 56 at `--os-radius-sm`, one hairline `--os-line` border on `--os-plate`.
  - Line 1 is the name, mono `--os-text-sm`, `--os-ink`, ellipsized with the full name
    in `<title>`.
  - Line 2 is the trigger as a sentence: `any write to request`, `an update to deployment`,
    `topic instance.provisionRequested`, `schedule 0 0 2 * * *`, and after Phase B
    `before create of request`. The concept's short name is in the data-string tint;
    the full id is in `<title>`.
  - A `@loop` card carries a small accent arc on its top-right corner.
  - A `@mode` card carries a quiet chip (`single`, `queued 10`).
- **Order within a column.** Cycle group first, then one barycenter pass (each card at
  the mean y of the cards that start it), with ties broken by name. It is deterministic,
  and it cuts crossings without an optimiser.
- **Edges.**
  - A cubic Bezier from the source card's right edge to the target's left edge.
  - The label (the concept's short name) sits at the midpoint on a small `--os-plate`
    plate, so it reads over crossing lines.
  - Two concepts on one pair share one curve with stacked labels.
  - An edge inside one stratum (a cycle) is an arc below its cards, and a self-edge is
    a small loop arc on the card.
- **Undecided edge.** A dashed `4 3` stroke in `--os-warn`, labelled `request, undecided`.
  The reason sentence (the analysis's `reason`) is in `<title>` and in the detail. Colour
  is never the only carrier: the word says it.
- **Permitted cycle.** Its members sit inside one rounded enclosure: `--os-accent-soft`
  fill, accent hairline, radius `--os-radius`. The caption along its bottom edge is
  "Permitted: stops when <until>, at most N runs".
- **Selecting a card** (click, or Enter on the focused card):
  - Its incoming and outgoing edges go to `--os-accent` at 2px, and every other edge
    and card fades to 35% opacity.
  - The transition is `--os-duration-fast` on `--os-ease`: it answers a person's act,
    so motion is welcome.
  - Under reduced motion there is no transition. Escape clears the selection.
- **Keyboard.** Cards are one roving tab stop. Up and down move within a column; left
  and right move to the nearest card by y in the next column. Focus is a visible
  2px `--os-accent` outline offset 2px.
- **Pan and zoom: none.** The SVG is sized to its content inside a native scroller. With
  the in-repo graph (a few dozen connected automations, three or four strata) it fits
  a window, and native scrolling is accessible for free. Do not import the
  Deployables viewport unless a real graph measures too big.
- **No load animation.** It is a read, not a live feed: nothing rises and nothing rings.

## Stand alone

- A `Subhead` "Stand alone", with its count in the Head-like meta slot of the Subhead
  if the kit offers one, otherwise in the caption.
- The caption is one sentence: "Nothing these write starts another automation, and
  nothing another automation writes starts them."
- Below it, a wrapped list of mono names. Each is a button that selects it and fills the
  detail pane.

## Loops stopped

- A `Subhead` "Loops stopped". Each row carries:
  - the time (`--os-muted`, mono, xs)
  - the chain: mono names joined by a 10px hairline connector drawn in CSS, not a text
    arrow
  - the refused link last, in `--os-error`, with a 2px stop bar before it
  - the sentence, for example "depth 17, past the cap of 16", or "the @loop on
    advanceTicket allows 4 runs"; depth numbers are in the number tint
- A chain longer than 6 links shows the first two, then "12 more" as a quiet button
  that expands the row in place, then the last two.
- **Empty state:** "No loop has been stopped in the runs this cluster still keeps."
  Nothing else: an empty list here is good news, and it says so plainly.
- **Absent is not zero.** When the stops read fails, the band shows the Notice in the
  server's words and never "0 stopped".

## The detail pane

Built in `Panel` + `Subhead` + `Field` grammar (rule 8). It is read-only, so there is no
ActionBar (rule 12 does not apply).

- **Name:** mono `--os-text-md`. **Origin file:** muted mono xs.
- **Runs when:** the trigger sentence, then the filter source in a mono block when there is one.
- **Stratum:** "1, after routeRequest", with the number in the number tint.
- **Writes:** concept chips, mono, in the string tint.
- **Starts** and **Started by:** each entry is the other automation's name as a button that
  selects it, "through <concept>", the reason sentence, and for an undecided edge the
  word "undecided" with the reason.
- **Calls the map cannot see:** builtin and action names, and the caption "The load cannot
  see what a builtin or an action writes; the run-time depth cap still stops a loop
  that runs through one."
- **Loop:** "Permits the cycle with X and Y. Stops when <until>. At most N runs of this
  automation in one chain."
- **Mode:** "Parallel: every fire runs." / "Single: a fire while one runs is refused." /
  "Queued, up to 10 waiting." / "Restart: a new fire cancels the one in flight."
- **With nothing selected**, the pane explains how to read the map in three short sentences
  (columns, dashed edges, enclosures). That makes the empty state an invitation rather than
  a blank.

## Reading states

- **Reading:** the Head's "Read again" shows busy, and the body says "Reading the
  automations this cluster loaded."
- **Failed:** a `Notice` with tone error, the section's own sentence, and the server's
  words as detail. A refused capability reads as the server sends it.
- **None loaded:** "This cluster loaded no automations."

## Copy rules

- Sentence case.
- No middle-dot meta strings: the meta is a sentence.
- No arrow glyphs appended to text.
- Name things by what a person understands:
  - "Starts", not "out-edges"
  - "Undecided", not "conservative"
  - "Stand alone", not "isolated vertices"

## Acceptance

1. Screenshots in both themes, populated and empty, at 1280 and 600 wide.
2. A keyboard-only pass: select, move, clear.
3. A reduced-motion pass.
4. The layout's fixture tests:
   - columns by stratum
   - stable order across input permutations
   - an SCC enclosed
   - no overlapping cards
   - every edge endpoint on a card edge

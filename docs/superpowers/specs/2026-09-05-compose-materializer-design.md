---
title: The Materializer -- composing the memory graph into a file
date: 2026-09-05
epic: memql#4977
program: memql#4961
status: accepted
---

# The Materializer

Sub-project C of the work-spine program (memql#4961). A new app and a new
namespace, from the owner's brief in the 2026-09-05 brainstorm and the design
round that followed it.

**The Materializer is where a person and the model compose data from the memory
graph into a file.** Sources are concept rows, Library files and a template;
the output is a real file of a real type, landing in the Library with a record
naming everything that went into it and every model that touched it.

The one-line test of whether this shipped: *a person can hand a client a PDF
that the cluster can prove it made, from rows the cluster can name, and make
the same PDF again next quarter without a model being asked to think twice.*

---

## A. The decisions

Eight, four of them taken by the owner in the design round.

| | Decision | |
|---|---|---|
| **D1** | The namespace is **`compose`** | owner |
| **D2** | The mark is a **concept annotation**, `@composable`, carried on the wire beside `@displayCard` | design |
| **D3** | The shipped formats are **markdown, html, txt, csv, json, docx, pdf**; audio and video are declared and unoffered | owner |
| **D4** | A format with no metadata channel is written **clean**, and the surface says so. No sidecar, no invented channel | owner |
| **D5** | **The draft leads.** The conversation is a column beside it, not the middle of the app | owner |
| **D6** | A materialization **is a `v1:work:goal`**, opened through the existing `createGoal` with `requestedVia="materializer"` | design |
| **D7** | A template is an ordinary **Library file**, not a new storage form | design |
| **D8** | Materializing a deployable produces a **package source zip in the Library**, which the existing pipeline deploys unchanged at `sourceKind="artifact"` | epic brief |

---

## B. D1 -- the namespace is `compose`

The epic forbade `materializer`: that word already names the engine's boot
seeder (`component/memql/seed_materializer.go`), and a namespace sharing it
would make `system:seedMaterializer` and `v1:materializer:*` two unrelated
things one word away from each other in every log line.

`compose` was chosen over `render`, `press` and `deliverable`. `render` loses
to the OS's own vocabulary -- this shell says "renders in surface", "the
rendered pass", "a render error" -- and a namespace that collides in speech
with the word for drawing to a screen is one that will be misread aloud in
every design conversation this app ever has. `press` is unclaimed and vivid and
nobody would guess what `v1:press:impression` is. `deliverable` is the
client-facing word and sits one letter from Deployables, an app in the same
dock.

So: **`v1:compose:composition`**, **`v1:compose:template`**,
**`v1:compose:recipe`**. `dsl/compose/`, `component/compose/`,
`integrations/compose/`.

**The SURFACE keeps the name Materializer**, and that is not an inconsistency
-- it is the split the epic asked for. `v1:work:goal.requestedVia` already
carries the string `"materializer"` (`dsl/work/concepts.memql:39`), written
before this epic began; the OS app id is `materializer`; the dock says
Materializer. What changed is only the name of the rows, which is the thing
that had a collision.

---

## C. The rows

Three concepts, all on the composite owner tier
(`@rowAuthz(owner="ownerUserId", clusterOwner)`) -- a person reads their own,
a cluster owner reads the instance's, which is what makes "everything
materialized in this instance" a read that can honestly return everything.

### `v1:compose:composition` -- the record

One row per materialization. It names, in the epic's own words, *the sources,
the template, the user, and every model that contributed*.

The three fields worth arguing about:

- **`sources` is a list of typed references, not a list of ids.** A source is
  `{kind, ref, label, capturedAt}` where `kind` is `concept_row | library_file
  | query`. Two of those are rows and one is a READ, and flattening them to ids
  would lose which is which -- "this was made from the 4 invoices matching
  status=open" and "this was made from these 4 invoice ids" are different
  claims about repeatability, and only the first one survives next quarter.

- **`modelsUsed` is a list, and it is append-only within a run.** The brief
  says *every model that contributed*, not "the model". A composition that was
  drafted by one model and tightened by another has two entries, each
  `{provider, model, calls, tokens}`. One field would have made the second
  contribution invisible, which is exactly the fact a provenance record exists
  to carry.

- **`provenanceEmbedded` is a boolean AND `provenanceNote` is a sentence.**
  D4 below is why. A `false` with no account of itself reads as a failure; the
  note says which it is ("CSV carries no metadata channel").

The output is `outputFileId` -> `v1:library:file`. The composition owns no
bytes, which is the same division `v1:library:artifact` already makes: the
index row owns no content and the file row does.

### `v1:compose:template` -- the repeatable part

The epic: *a template (a file or a zip of files) that makes the output
repeatable for a customer*. **D7: a template is an ordinary Library file**,
pointed at by `fileId`, not a new storage form. It gets uploads, versions,
chunked transfers, archive-to-Bin and row authz for free, and the alternative
-- a second byte-bearing concept -- would have needed all of that again.

The row is the BINDING, not the bytes: which file, which format it produces,
which account it is for, what its placeholders are called. `placeholders` is a
list of `{name, description, required}` read out of the template at bind time,
so the app can show what a template will ask for before somebody picks it.

### `v1:compose:recipe` -- the replayable part

A composition that worked, promoted to something you can run again. It holds
the source SELECTORS rather than the resolved rows (a query, not its answer),
the template id, the format, and the goal template the run replays. Running one
opens a goal exactly as a fresh materialization does, with the catalog hit that
makes it reach no model.

**This is deliberately thin, because the work spine already owns replay.**
`forkRun`, `replayRun` and the catalog's `GoalSignature` are built
(`component/work/`), and a recipe is a NAME for a signature plus its bound
inputs -- not a second execution model. The epic's "Nexus can capture it as a
replayable recipe" is that sentence.

---

## D. D2 -- the mark on composable concepts

The epic: *concepts relevant to materializing should be marked so both the
model and the person find them.*

**It is a concept annotation, `@composable(...)`, and it rides the wire beside
`@displayCard`.** Two precedents already do exactly this shape: `@displayCard`
gives concept-agnostic clients their rendering slots, and the data-origins
`data_state` gives them their writability. Both are declared on the concept,
resolved into `memorynodes.ConceptDefinition`, and copied onto `ConceptInfo`
by `component/grpc/concepts_handlers.go`. A third is the same edit three times.

```memql
@composable(as="invoice", fields="number,issuedAt,total,customerName")
concept invoice { ... }
```

- `as` is what a person and a prompt call it, when the concept id is not that.
- `fields` is the projection worth composing FROM -- not the whole row, because
  a composition prompt handed forty fields spends its context on ids.

**Why an annotation rather than a row.** A row needs somebody to write it, and
the concepts most worth marking arrive in a **product DSL bundle** at
`MEMQL_DSL_PATH` -- a tree no Go test in this repo walks and no seeder knows
about. An annotation travels with the concept it is about, so a product that
ships an `invoice` concept ships its composability in the same file. A registry
row would have made every product's most important concepts invisible to this
app until an operator remembered to register them.

**Both consumers read the same one thing.** The app's Sources column lists
composable concepts first and everything else behind "show all"; the compose
prompt is handed the same list. There is one derivation, so the model and the
person cannot be looking at different sets -- which is the failure mode the
front-door hosts section of the root CLAUDE.md records about having two.

**Absent is not "no".** An unmarked concept is *unmarked*, not *forbidden*: a
person can still pick rows from it. The mark is a ranking and a hint, never a
gate -- a gate would mean a product that forgot the annotation has a
Materializer that cannot see its own data.

---

## E. D3/D4 -- formats and embedded provenance

### What ships

| Format | Written by | Provenance channel |
|---|---|---|
| `markdown` | `component/compose` | YAML front matter |
| `html` | `component/compose` | `<meta name="memql:*">` block |
| `docx` | `component/compose` (hand-written OOXML zip) | `docProps/core.xml` + `docProps/custom.xml` |
| `pdf` | `component/compose` via `github.com/go-pdf/fpdf` | XMP packet + Info dictionary |
| `txt` | `component/compose` | **none** |
| `csv` | `component/compose` | **none** |
| `json` | `component/compose` | **none** |

**DOCX needs no library and PDF needs exactly one.** A `.docx` is a zip of
XML at known paths, and `archive/zip` is in the standard library -- the same
observation `component/packages/source.go` and `component/sitepublish` already
lean on. A PDF is not: a correct one needs a cross-reference table, an object
graph and font metrics, and hand-rolling that is how you ship a file that opens
in one reader and not another. `github.com/go-pdf/fpdf` is pure Go, no CGO,
maintained, and the maintained continuation of the `gofpdf` every Go project
that has ever written a PDF used.

**Audio and video are DECLARED AND UNOFFERED, and the surface says why.** The
format enum carries no value for either. The brief names both, so an app that
simply lacked them would read as one somebody had not finished; the Target
column names them with one sentence -- audio wants a compose-then-speak
pipeline with a cost ceiling of its own, video wants a generation provider this
cluster has none of. An absent control with no account of itself reads as
something nobody got round to building; this is the same rule the Bin states
about its missing retention control and Domains states about its missing
re-check button.

### D4 -- where there is no channel

`txt`, `csv` and `json` are written **exactly as composed**. Nothing is
prepended, nothing is appended, no companion file appears.

The record is then the only provenance that file has, and **the row says so and
the app says so**: `provenanceEmbedded=false` and a `provenanceNote` reading
"CSV carries no metadata channel -- the record here is the only provenance this
file has."

The two rejected alternatives, and why:

- **A sidecar** (`Q3.csv.provenance.json`) doubles the row count of every such
  materialization forever, and the second row is noise in every folder listing
  and every picker in the product. Provenance that travels is worth something;
  it is not worth two files where somebody asked for one.
- **Refusing the formats** buys one clean invariant -- every output
  self-describing -- and costs "export these rows as CSV", which is a thing
  people will come to this app for on day one.

**The invariant we keep instead is honesty, not universality:** every
composition carries complete provenance in its RECORD, and every file states
truthfully whether it carries any of its own.

---

## F. The run template -- what replays without a model

A materialization is four deterministic steps around one reasoning step. The
split is the whole product claim, and it is what makes the second quarter's
report cost nothing.

```
  gather      deterministic   resolve every source ref to rows/bytes
  compose     REASONING       the one model call: rows + template -> the draft
  render      deterministic   draft + template -> the format's bytes
  stamp       deterministic   embed provenance, per D3's table
  file        deterministic   write v1:library:file + v1:compose:composition
```

- **`gather` is deterministic even though it reads live rows.** It resolves
  refs; it makes no choices. A source that has changed since last time yields
  different bytes and the same step -- which is the point of re-running a
  recipe.
- **`compose` is the only step that reaches a provider**, and on a catalog hit
  it does not (`component/work`'s compile order: exact match on
  `GoalSignature`, then near match, then triage). A recipe re-run with the same
  template and the same source SHAPE is an exact match.
- **`render` and `stamp` are separate steps on purpose.** Stamping is where
  D4's "this format has no channel" is decided, and folding it into `render`
  would bury that decision inside a format writer instead of putting it on a
  step with its own receipt.

Each step's `kind` is what the Work app's spine draws: four hollow nodes on a
hairline and one filled node on solid ink. **A person can see, without reading
a word, that this cost one thought.**

---

## G. D5 -- the app

`clients/os/src/apps/materializer/`. Five sections: **Composer**,
**Materialized**, **Templates**, Logs, Settings.

### The Composer, and why the draft leads

```
┌──────────┬───────────────────────────┬──────────┐
│ SOURCES  │  THE DRAFT                │ TARGET   │
│          │                           │          │
│ ▸ rows   │  # Q3 Report              │ format   │
│ ▸ files  │  Revenue rose 14%...      │ template │
│ ▸ templ. │                           │ folder   │
│          ├───────────────────────────┤          │
│          │  Ask: tighten the opening │          │
└──────────┴───────────────────────────┴──────────┘
      state in words          [ Materialize ]
```

**The draft is the page and the conversation is a column beside it.** The
alternative -- a chat transcript in the middle with the draft one click away --
is what every AI writing tool does and it is wrong here for one reason: the
file is the deliverable, and a surface where the deliverable is never on screen
cannot answer "which version am I about to send". It is also the shell's own
rule 11 read forward: a list and its detail never share a scroll column, and a
transcript and its draft are that pair.

Three columns is a shape this shell has not used, and it earns it: the three
are **what goes in**, **what it is**, **what comes out**, which is the
composition's own structure rather than a layout. Below a pointer-and-hover
breakpoint they stack in that order, and on the phone the draft is the section
with the other two behind the Refine affordance.

The act bar is `kit/ActionBar` and follows rule 12 -- the state in words, then
the acts legal from it, **an illegal act absent rather than disabled**. A
composition with no sources offers no Materialize; one that is running offers
Cancel and no Materialize; one that finished offers Open in Files and Save as
recipe.

### Materialized -- the view

Everything materialized in this instance, one `LiveList` over
`v1:compose:composition`, with its authoring and origin metadata: what was
made, from how many sources, through which template, by which models, and
whether the file carries its own provenance.

**The arrival-cue fingerprint is `name | status | format | outputFileId |
provenanceEmbedded`.** It excludes `spent` and `modelsUsed`, which move while a
composition is running and would ring the row somebody is already watching --
the heartbeat rule at its usual sharpest point. The counters re-render live and
never ring, which is the pair `test/campaigns/app.test.tsx` pins and this app
pins again.

### The seam with Files

Agreed with the Files-places epic (memql#4981) and recorded in both places:
**a composition has one record and its output has one file.** This app's
Materialized section answers *what was made, from what, by which models*; the
Files rail's Materializer place answers *where is the file*. Files carries
"Open in Materializer" and one sentence; it never edits a composition and this
app never shows a file tree.

---

## H. D8 -- materializing a deployable

The output is a **package source zip**, written to the Library as an ordinary
`v1:library:file`, which the existing Deployables pipeline consumes unchanged
at `sourceKind="artifact"` (`dsl/platform/concepts.memql:542`). Offered for the
kinds the cluster actually serves: `spa`, `static`, `shopify_storefront` --
which is `OFFERED_KINDS` in the Deployables app, held equal to
`v1:platform:site.kind` by `site_kind_os_parity_test.go`, so this app reads
that list rather than keeping one.

**Nothing new is built in the deploy path, and that is the design.** The hand-
off is: materialize -> a zip in Files -> the Deployables compose flow opened at
Source with that artifact already chosen. The alternative -- this app calling
`packageDeploy` itself -- would put a second deploy entry point in the product,
with its own idea of addresses, accounts and confirmations, beside a rail the
Deployables epic spent two passes making the one place a deploy is composed.

---

## I. Live feeds and routing

`v1:compose:composition`, `template` and `recipe` get broadcast routing rules
in `component/node/routing.go`. Without them the Materialized list is correct
on load and frozen after -- which looks like it is working, and is the exact
failure the Fleet app's README bullet records.

They are one row per thing a person authored, so the volume argument that
excludes `v1:worker:invocation` and `v1:campaigns:delivery` does not apply.

---

## J. What it deliberately does not do

- **No document editor.** The draft is markdown in a textarea with a live
  preview. A rich-text editor is a product of its own, and half of one is worse
  than none.
- **No template authoring.** A template is a file somebody uploads. Making
  templates is a job for the tool that made the template.
- **No scheduled materialization.** A recipe re-run is a goal, and goals are
  scheduled by the work spine's own machinery when that lands -- not by a
  second scheduler in this app.
- **No delete.** A composition is the evidence a file was made honestly;
  archiving is the Bin's, like every other Library row.

---

## K. Issue map

| Issue | Section |
|---|---|
| #4978 -- the record, rows and run template | B, C, D, E, F |
| #4979 -- the app and the materialized view | G |
| #4980 -- the deployable, the rendered pass, cleanup | H, plus the rendered-pass record |

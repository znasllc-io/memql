# Deployables Run -- Design

- **Date:** 2026-09-03
- **Status:** approved for build, and built. The owner asked for the epic end
  to end in one pass ("I need everything finished up end to end"), so this
  round ran as a recommendation pass rather than a question-by-question
  brainstorm: every fork below records the choice, the reason, and what was
  rejected. One decision DEPARTS from the program record and is marked so.
- **Scope:** `dsl/platform/` (the settings field, its mutation, the traffic
  builtin), `dsl/observability/` (the aggregate's row shape),
  `component/memql` (the settings write guard), `component/edge` (the
  runtime-config key and the per-request record), a new
  `component/sitetraffic` (the writer, the read and the retention),
  `component/database` (one migration), `component/metrics` (two counters),
  `clients/os/` (the two readings on the Live stop and the list row's
  figure), the operator docs and the env registry.
- **The wave this belongs to:** Epic 4 of
  [the Deployables program](2026-09-02-deployables-program-design.md) (P7,
  P8) -- the maintain side. Its neighbours are
  [Compose](2026-09-02-deployables-compose-design.md), whose Live stop this
  builds on, and [Logs](2026-09-03-logs-design.md), whose store this
  deliberately does not use (D1 below).

## Why

Two things a person running a deployable needs after it is live, and neither
existed. **Settings a bundle reads at runtime**, so one bundle can serve two
deployables against different endpoints without a rebuild -- today the only
way to change an API base is to rebuild and republish. And **a truthful
answer to "is anybody using it, and is it healthy"**: the program record's
own verification found that the edge emits no per-site metrics at all, so
this half is new measurement rather than the surfacing of something already
counted.

## What was verified before any of this was decided

- **The runtime-config document is already the mechanism.**
  `component/edge/runtimeconfig.go` serves `GET /runtime-config.json` for
  every live site, and the storefront binding already rides it -- including
  the one convention for a value that must not be public, a `...Ref` field
  naming a `globalSecret` the edge resolves at serve time.
- **`codeMetricsInWindow` is the cautionary case, not the precedent.** It is
  a DSL `query` over a dedicated hypertable with no Go provider behind it and
  no concept-to-table mapping, so it answers nothing. The working shape for a
  read served from Go is the `@sdk` builtin whose executor returns synthetic
  nodes (`dataOrigins`, `modelCatalog`) -- the same finding the Logs round
  made independently.
- **The Logs store is uncommitted.** `component/logstore`, the `log_line`
  table and the `logLine` concept live on a sibling branch and are on neither
  `main` nor this epic's base.
- **The Logs store's writer is rate-limited and drops on pressure** (its L5:
  a per-node bucket of 2000 lines a second, overflow dropped and counted).
- **`update{}` is a read-merge** (memql#1628), so an object field written
  through it inherits what it does not name.
- **A dedicated table passes through neither the parser nor the row-authz
  filter path**, so nothing the engine injects reaches it -- the
  `integrations/CLAUDE.md` rule about a capability that reads rows directly.
- **`v1:platform:site` broadcasts BOTH created and updated**
  (`component/node/routing.go`), so a settings save reaches every open OS
  window with no engine work. `v1:observability:*` broadcasts nothing, and
  nothing here wants it to.
- **`httputil.ReverseProxy` reaches Flush and Hijack through
  `http.ResponseController`**, which walks `Unwrap()`.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| R1 | Where the request log lives | **Its own `edge_request` hypertable**, NOT the log store. A DEPARTURE from the program record, argued below |
| R2 | The read's shape | **An `@sdk` builtin served from Go** (`siteTrafficInWindow`), never a `query`: a DSL query reads the graph and these rows are not in it |
| R3 | Where the builtin is declared | **`dsl/platform/`**, with the row shape in `dsl/observability/`. The authorization is the SITE's, and authorization decides where a capability belongs |
| R4 | Authorization | **The deployable's own**, asked of the engine under the caller's actor. An unreadable id is DROPPED, not refused |
| R5 | Unmeasured | **No row**, all the way from the aggregate to the sentence. Never a zero-filled row |
| R6 | Settings write | **Replace, not merge**; the whole object is the argument |
| R7 | Settings and secrets | **A key ending in `Ref` is refused** |
| R8 | Aggregate retention | **The same window as the raw rows**, unlike `code_invocation` |
| R9 | The Live stop's shape | **Two panels inside the existing stop**, not a sixth stop and not stat-tile cards |
| R10 | Refresh | **A timer on the window's own cadence**, never the arrival cue |

---

## R1. The request log is the edge's own table

The program record says the edge writes its request log "into the log store
with the site as its subject". Three facts moved it, and they are recorded
here so the departure is a decision rather than a drift.

1. **A rate-limited writer cannot carry a figure.** The store drops lines
   under pressure by design -- which is right for logs and wrong for a
   denominator. A busy site is exactly when the bucket empties, so the figure
   would dip precisely when traffic peaked, and it would look like a healthy
   quiet spell. A measurement whose error correlates with the thing measured
   is worse than no measurement.
2. **A continuous aggregate needs typed columns.** The store carries
   everything but its fixed fields as `attributes jsonb`; folding status
   class, bytes and duration out of JSON on every refresh is both slower and
   a schema nobody declared.
3. **The store is a sibling epic's table.** An aggregate over `log_line` is a
   migration that refuses to apply wherever that epic has not landed, and the
   two are in flight at once.

**What is kept from the program's intent.** The site is still the subject:
`edge_request.site_id` is the deployable's bare id, which is the same join
key a log line's `subject` would carry, so a Logs deep link from the Live
stop lands on the same identifier with no second join invented. **What would
change the decision**: if the store grows a class of line exempt from the
bucket, and typed columns for a request, this table folds into it and the
aggregate re-points -- the reader is one package and the concept is one
declaration.

**What is deliberately NOT written.** Nothing that identifies a visitor: no
address, no user agent, no path, no referrer. "Is anybody using it, and is it
healthy" needs counts and outcomes; a table with per-visitor detail is one an
operator has to reason about under data-protection law in order to run a
dashboard.

### The aggregate's columns

The short design pass issue memql#4908 asked for.

| Column | Why |
|---|---|
| `site_id` | the grouping key, and the authorization key |
| `window_start` | `time_bucket`'s own; the reader derives the span from the bucket |
| `request_count` | "is anybody using it" |
| `error_count` (5xx) | "is it healthy" |
| `client_error_count` (4xx) | counted APART: a 500 is the deployable failing and a 404 is somebody asking for a page it does not have. Folding them makes a healthy site with a broken inbound link look unhealthy |
| `bytes_total` | the only cost signal available without new measurement. Carried to a client and deliberately NOT shown: the panel's budget is four facts, and bytes is the one a person asks for least often |
| `last_served_at` | what a LIST row shows; the single most useful figure per deployable |

Rejected columns: a p50/p95 duration pair (`duration_ns` is on the raw rows
and can grow an aggregate later; a latency figure invites a latency
conversation this epic is not having), and a per-path-class breakdown (six
more columns for a question the raw rows already answer).

`materialized_only` is left false, so the newest bucket is answered live from
the raw rows -- which is what lets "last served" say seconds ago rather than
minutes ago.

**Retention keeps the aggregates in step with the raw rows (R8), which is the
opposite of what `code_invocation` does.** There the aggregate is a trendline
worth keeping after the forensic detail is gone. Here the aggregate IS the
product, and a figure with no rows behind it is a figure nobody can check --
so "unmeasured" means the same thing at every horizon.

**The relations exist without TimescaleDB.** The migration creates continuous
aggregates where the extension is present and ordinary views where it is not,
under the same two names. The reader therefore has one query rather than a
branch, and a plain-Postgres box answers honestly from the raw rows instead
of erroring on a missing relation -- which the reader would have had to
translate into "unmeasured", the one answer that must mean *nothing measured
this* and nothing else.

## R4. Authorization is the deployable's own

A caller reads a deployable's traffic exactly when they may read the
deployable: the reader resolves the requested ids through `sitesAll` (one
call for a list) and `siteById` (which also covers an ARCHIVED deployable,
whose traffic is exactly what somebody deciding whether to restore it wants),
under the caller's own actor, and keeps what comes back.

No second authorization model. The concept's composite owner-or-cluster-owner
tier is the whole of it, so there is nothing here that can drift from it.

**An unreadable id is dropped rather than refused**, because a refusal naming
the id would answer "does this deployable exist" for anybody who can spell
one. Dropping it gives the same answer a deployable with no traffic gives.

**The per-id fallback is bounded.** `sitesAll` answers the common case in one
read; the fallback exists for the ARCHIVED case, which is a detail surface
asking about one id. Unbounded it would also be a way to make one call cost
two hundred engine reads by naming ids nobody can read, so past sixteen
unknown ids the rest are treated as unreadable -- the answer they were
overwhelmingly going to get, since a list only ever shows active deployables
and `sitesAll` has already covered those.

**The client PAGES rather than validating.** The server refuses a call past
its cap instead of truncating one, which is the honest server behaviour; a
list longer than the cap therefore makes several calls. A client that
trusted the cap to be generous enough would show a whole list with no figures
on the day a cluster outgrew it.

## R5. Unmeasured is not zero, at every layer

The rule is `campaignStats`', carried the whole way:

| Layer | Unmeasured | Zero |
|---|---|---|
| aggregate | no row | a row with `error_count` 0 |
| reader | no `Reading` | a `Reading` with `ErrorCount` 0 |
| builtin | no node | a node with `errorCount` 0 |
| OS reader | `null` | `requests: 0` |
| the stop | a sentence naming both causes | the strip, and "0" in the Errors fact |
| the list row | nothing | a "served ..." chip |

The sentence names both causes -- "either nobody visited, or this cluster was
not recording traffic" -- because a person looking at their own app cannot
tell them apart and the wrong guess is expensive: one is a business fact and
the other a cluster fact.

**Gaps INSIDE a measured window are zeroes**, and that distinction is the
other half. Once a window has any traffic, a bucket with no row is a bucket
that served nobody, and the strip draws it as a gap -- a quiet night has to
look quiet, and a series that closed up its gaps would draw two busy hours as
continuous traffic.

## R7. Settings are public by construction, and `Ref` is how that stays true

Everything in `settings` is served to every visitor, unauthenticated, in a
document their browser fetches. The guard therefore refuses a key ending in
`Ref` -- not because the string is dangerous, but because `...Ref` is already
this platform's convention for the opposite kind of value: the storefront
binding's `storefrontTokenRef` names a `globalSecret` the edge resolves at
serve time for exactly one site kind.

A settings key spelled that way would LOOK like that convention and be
honoured by nothing. The natural mistake (`"apiTokenRef": "my-secret"`)
publishes a secret's name; the natural next mistake is somebody teaching the
edge to resolve it. Refusing the suffix closes both, and the refusal names
the binding, which is where a reference belongs.

Rejected: allowing `Ref` and documenting the hazard (documentation does not
reach the person typing), and resolving `...Ref` keys through the secret
store (that is the storefront binding, which is kind-gated for a reason --
generalising it would make every site's settings a secret-read surface).

## R9 / R10. The Live stop's two readings

**Two panels inside the Live stop, not a sixth stop.** The program's seam
says the Rail renders from the target and Run changes what the Live stop
SAYS. A stop is a target's decision.

**The shape is the picture and the numbers are words beside it.** One strip
of bucket columns says what a table cannot -- steady, spiky, or stopped three
hours ago -- and the totals go in the shell's own `Facts` grammar, where
every other panel in the app puts a labelled value. Not a row of stat-tile
cards: four numbers about one deployable are four facts, and a card grammar
beside `Panel` is what DESIGN.md rule 8 exists to remove.

**One series, one hue; errors are a STATUS.** Requests are the accent and
need no legend, because the heading names what is plotted. Errors wear the
shell's error token in a band of their own beneath the baseline rather than
as a stacked segment: at a strip thirty-four pixels tall the 2px surface gap
a stack requires would be wider than the marks it separates. Colour is never
the only carrier -- the Errors fact, each column's title and the series
summary a screen reader gets all say it too.

**One label, the peak.** A number on every column goes unread; without any,
a reader can see one hour was busier than another and not whether the tallest
column is nine requests or nine hundred.

**The facts are the SUM of the strip.** A row off the bucket grid contributes
to neither, so the picture and the numbers beneath it cannot disagree -- the
client-side half of the property the continuous aggregate gives the server
side.

**It refreshes on a timer, never through the arrival cue** (R10). The cue
announces a change to a row somebody is looking at; a figure that moves on a
clock would fire forever on nothing anybody did, which is the strobe the
heartbeat rule exists to prevent. The cadence widens with the WINDOW rather
than following the bucket -- an hour's figure moves visibly on one more
request and a week's does not -- and nothing is read more than once a minute.

**The list row reads a WEEK** where the stop defaults to a day, because the
questions differ: the stop asks how it is doing and a row asks whether
anybody is using it at all. A day-wide row would call a weekly app abandoned.

## Testing

- **db-gated** (`component/sitetraffic`, on the db-tests lane): a request
  lands, the aggregate reflects it, and the read returns it for the owner and
  for a cluster owner; a third user gets zero rows as an empty ANSWER rather
  than a refusal; an unmeasured window returns no row and a no-error window
  returns a zero; the summary agrees with the buckets it folds; the window is
  half-open; an unreadable id narrows rather than fails. Also
  `component/memql`: every settings refusal on the REAL mutation path, the
  replace-not-merge property, survival across an unrelated write, and the
  cross-user refusal.
- **Pure**: the sink never blocks against a stalled insert and counts what it
  dropped; a failed flush counts as dropped; `Stop` flushes; the two
  spellings of the path-class set agree; the response wrapper exposes Flush
  and Hijack through `ResponseController`.
- **Measured**: serving a bundle asset with and without the log, recorded in
  the PR.
- **OS**: the reading's arithmetic on fixtures, and the SENTENCES rendered --
  unmeasured, zero and populated, the window picker changing the bucket, the
  system-owned note, a refused read and a refused save verbatim, and the list
  row agreeing with the stop.
- **Rendered and looked at**, both modes, empty and populated, which is what
  `clients/os/DESIGN.md` asks for. It caught three things 1394 green tests
  could not: a strip drawn a thousand pixels wide read as lollipops rather
  than a series; a baseline at eight percent alpha was invisible, taking the
  strip's quiet-stretch meaning with it; and the settings rows stacked
  full-width on a desktop window because their media query keyed on
  `hover: none` beside width.

## Out of scope

Alerting, uptime probes from outside the cluster, per-path analytics,
geographic breakdowns, latency percentiles, and any retention longer than the
log store's. The Logs deep link from the Live stop belongs to the Logs epic
(memql#4896), which owns the app it would open.

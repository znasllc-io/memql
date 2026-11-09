// Package emailstats keeps the email statistics: the facts the Email
// Overview's figures are counted from, the scheduled walk that fills them
// (docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md,
// section 5, D10 and D18), and the read that counts them for a caller
// (section 7, statistics.go). It is the `emailStats` plug-in, a component: it
// reads this cluster's own campaign rows and writes this cluster's own
// tables, and calls nobody's API.
//
// # Why a side table
//
// The figures are counts grouped by day and by organization, campaign, rule,
// journey and audience. The query language counts in the database but cannot
// group counts by time; a daily trend across an organization's campaigns would
// cost hundreds of count queries per page load. Counters bumped by each writer
// were the other choice, and they drift: a delivery's outcome is written on
// three different paths (the drain worker, the single-send path, the provider
// feedback ingester), any of which can run on any node. So the rollup records,
// in email_stat_fact, one row per countable fact, and the read groups those
// rows with ordinary SQL.
//
// # Why through the engine
//
// The campaign rows are graph rows, and hand-written SQL over the graph is the
// measured hazard the design rules out: the latest-version collapse, the id
// spellings and the row admission live in the engine. The walk asks seven
// @serverOnly queries under the rollup's maintenance principal and internal
// origin, and only this package's own tables are written with SQL
// (rollup.go). Each of the seven projects only what the facts read -- no
// address, no address digest, no owner; the one reference to a recipient is
// the id a consent event is judged and counted under -- because the walk
// reads every organization's recipients and a figure needs none of the rest
// (TestTheWalkReadsNoAddressAndNothingTheFactsDoNotUse).
//
// # One fact per source record and kind
//
// A fact is keyed by the record it is about and its kind -- a delivery's send,
// an engagement event's open, a consent event's unsubscribe -- and every write
// is ON CONFLICT DO NOTHING. That one rule is what makes a rerun, the
// five-minute overlap, a new version of a delivery and two replicas' runs all
// count each fact once (facts.go).
//
// # Why the five-minute overlap
//
// Each source is walked from a watermark, the newest row.createdAt a run has
// written past, less five minutes. A row's time is stamped by the clock of
// the node that wrote it and the row commits after that, so a row can arrive
// with a time before a watermark another run already moved past; reading five
// minutes behind picks it up, and reading a row twice costs nothing. The
// watermark never passes the clock of the run that writes it, so one row
// stamped ahead cannot hide the rows after it, and the run's clock never sets
// it, so a cron leader whose clock runs fast cannot shrink the overlap
// (rollup.go, "Never past the clock"). How far the read counts is a second
// instant beside it, how far the source has been walked: the newest row too,
// or, once a walk has read its last page, the clock it asked that page by,
// rows or none. A figure whose sources' walks have not reached a day is null
// there, never zero (statistics.go).
//
// # Attribution (D18)
//
// A rule's send carries its rule; a journey's send names its enrollment in its
// request id, and the enrollment names the journey, which the send counts
// under only when the enrollment is in the send's own organization -- a
// request id is a string the sender chose (rollup.go, attribute); a subscribe
// or an unsubscribe names a recipient, whose audience it is counted under.
// The two lookups run once per page through the engine, in batches. Opens and
// clicks exist for campaign mail only, because only campaign mail is tracked.
//
// # Test runs are not statistics
//
// A test run is a campaign of its own, sent to the organization's testing
// audience. Nothing of it makes a fact -- its sends, opens and clicks, and the
// consent events it gave rise to -- and neither do the consent events of the
// testing audience's people, who are the organization's own staff. Two more
// lookups a page, made after the page is read, say which of its campaigns are
// test runs and which of its audiences are testing audiences (rollup.go, "Test
// runs are not statistics").
//
// # The read, narrowed to the caller's organizations (D11)
//
// builtin emailStatistics groups the facts with ordinary SQL over this
// package's own table, for the organizations the caller's grant reaches
// (MemQLEngine.ResolveAccessGrant, the row gate's own resolution). An
// organization the caller does not reach is dropped, not refused, so naming
// another client's organization answers every figure null; a figure nothing
// measured is null, never zero; and the open and click rates are of the
// campaign mail sent in a period, by send-day cohort (statistics.go).
package emailstats

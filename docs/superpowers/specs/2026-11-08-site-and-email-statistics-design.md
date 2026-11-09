---
title: Website and email statistics, scoped to the client a person belongs to
audience: internal
status: approved
area: operate
sinceVersion: 0.31.25
owner: znas
---

# Website and email statistics

Owner brief, 2026-11-08. Clients are promised basic statistics about their
websites, and deployables are those websites. Today a deployable's **Traffic**
page is one strip of request columns and four facts (requests, errors, not
found, last served): no visitors, no pages, no sources, nothing over a useful
span, and the owner's word for the presentation was "terrible". Email has an
Overview of five figures and per-campaign results, with nothing over time and
no view for one client. A client's own people (a "moderator") must see their
client's websites and email figures and nothing of any other client's, without
a way to switch clients. A few options on a deployable still open a pop-up; the
owner wants none.

**Approvals.** The owner approved this design in conversation on 2026-11-08
("Looks right"), and approved the written spec the same day ("Approve spec").
The same day the owner approved four additions in a forked conversation and
confirmed them here ("Yes, fold them in"):
- visits, views per visit and bounce rate for websites (D17)
- four more Email Overview sections, with the attribution they need (D18)
- Sources and Countries on the Deployables Overview's Websites section
- no previous period for twelve months (D4) Each fork in section 3 marked *owner* was the owner's explicit
choice. The new HTTP surface in section 6 (`/_memql/view.js` and `/_memql/view`
on a deployable's own origin) was approved as an HTTP exception under the
gRPC-first policy in the same conversation. Forks the owner did not settle
personally are marked *default* and can be overruled.

**Delivery.** One branch, `feat/site-and-email-statistics`, pushed to the
remote with no pull request; the owner merges it into `main`.

## 1. What the owner asked for

| # | Requirement, in the owner's terms |
| --- | --- |
| R1 | A deployable's Traffic becomes its **statistics**: much more detail, diagrams, unique visits, and the rest of what a client expects from website statistics. |
| R2 | The page follows the interface rules: clean, minimal, shared UI pieces reused where viable. |
| R3 | No pop-ups. Every option on a selected deployable opens a page in the app, as Access codes and Traffic already do. Check for hidden options that still open one. |
| R4 | Email gets statistics too: for the campaigns overall, or for one client. |
| R5 | What a person sees depends on their access. A client's people see only their client's deployables and email figures, and cannot choose another client. Nothing is specific to any one client. |
| R6 | Thorough. |

## 2. What exists, and what it constrains

- **The edge records one row per served request** (`component/edge/requestlog.go`,
  `component/sitetraffic`): site, node, time, status, path class, bytes and
  duration, into the `edge_request` hypertable, folded by two continuous
  aggregates (`edge_request_1m`, `edge_request_1h`). All three are kept 30 days
  (`RETENTION_DAYS`, applied by `sitetraffic.ApplyRetention`). The record
  carries **no IP address, user agent, path or referrer by design**; this spec
  reverses that decision deliberately (section 3, D1-D4) and the package
  comment changes with it.
- **The traffic read narrows to what the caller may read**
  (`sitetraffic.Reader.readableSites`): it asks the engine for `sitesAll` and
  `siteById` under the caller's own actor and silently drops anything else, so
  a request naming another client's site returns nothing rather than a refusal
  that would confirm the id exists.
- **Client scoping already exists in the engine.** `v1:platform:site` and every
  campaigns concept declare `@rowAuthz(owner="ownerUserId", clusterOwner,
  account="accountId")`. Members of an organization's group read its rows;
  developer rank and above are standing members of every account-kind group as
  a rule (access-model.md, "Staff are a rule, not rows").
  `MemQLEngine.ResolveAccessGrant` reports a caller's organizations
  (`EveryAccount`, or `AccountIDs`) from `resolveAccountScope`, the same
  resolution the row gate uses. The OS reads it as `access.everyAccount` /
  `access.accountIds`.
- **Campaign figures are exact counts through the engine** (`campaignStats`,
  `component/campaigns/stats.go`), one campaign at a time. Unique opens and
  clicks are folded from a read bounded at 5,000 and reported as unmeasured
  past it. The query language counts in the database (`count`) but cannot group
  counts by time; its `groupBy` runs in memory over rows already read. A daily
  trend across an organization's campaigns would cost hundreds of count queries
  per page load.
- **Raw SQL over the graph is a measured hazard.** The latest-version machinery
  (`component/memql/latest_scan_*.go`) records production plans that took 29.6
  seconds when a query's shape was wrong. This design never aggregates graph
  rows with hand-written SQL.
- **The kit has no chart.** Traffic's strip and Email's send band each draw
  bars of their own. `kit/Overview` gives a figure row and a distribution
  (`OverviewBreakdown`), and `kit/measure` gives absent-versus-zero figures.
- **Remaining pop-ups on a deployable:** the storefront Visit chooser
  (`page/VisitSiteButton.tsx`, Testing or Production), a version's details
  (`page/Versions.tsx`, where Roll back lives) and Remove source
  (`sources/RemoveSource.tsx`). All three use `page/DetailDialog.tsx`.
- **Roles.** Member and Viewer already hold `read app:deployables` and
  `read app:email`.

## 3. Decisions

| # | Decision | Why, and what was rejected |
| --- | --- | --- |
| D1 | *Owner.* **Unique visitors are counted without cookies, per UTC day.** A visitor is `HMAC-SHA256(dailySecret, siteId \| clientIp \| userAgent)`, truncated to 16 bytes. The secret is random, created for one UTC day, shared by every edge replica through one database row, and deleted when the day ends. The IP address and user agent never reach storage. | A first-party cookie counts returning visitors across days but needs a consent banner on every client site for EU and UK visitors. Counting nothing about visitors fails R1. The price of the daily secret is that a person visiting on three days counts three times in a weekly total, which the page says once in its help. |
| D2 | *Owner.* **A page view is a browser navigation, plus an opt-in page-change ping for single-page apps.** Server-side, a page view is a `GET` with `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document`, answered 200 or 304, that is not a prefetch (`Sec-Purpose` / `Purpose`). Without `Sec-Fetch-*` headers it is a `GET` accepting `text/html` whose path class is `document` or `fallback`. In-app page changes arrive through the ping (section 6). | Counting every HTML response double counts sites that fetch page fragments (one storefront fetches a view shell for each product page). Counting navigations only misses a single-page app's in-app pages, which the ping covers without the edge rewriting anybody's HTML. |
| D3 | *Owner.* **Five figure families:** visitors, visits, page views, views per visit and bounce rate over time; top pages and sources; devices, browsers and operating systems; countries; site health (requests, errors, not found, response time, data served, bot requests). | All four optional families were chosen; visits and bounce rate were added on 2026-11-08 (D17). |
| D4 | *Owner.* **History is 13 months of daily totals.** Request detail stays 30 days; hourly totals 30 days; daily totals and daily top lists 400 days. Ranges: last 24 hours (hourly), 7, 30 and 90 days (daily), each compared with the period before it, and **12 months with no comparison**: its chart shows the current month and the 12 before it (13 monthly buckets), so this month stands beside the same month last year. | 30 days cannot answer "this month against last month". The 12 months before a 12-month range are past the 400-day retention, so the first version of this decision promised a comparison the storage cannot hold (corrected 2026-11-08). Twenty-five months of daily totals would add it at about twice the rollup storage; the owner kept 13. |
| D5 | *Default.* **Days are UTC.** The daily secret, the daily totals and the chart's days all turn over at 00:00 UTC. | A per-site time zone would need per-zone secrets and per-zone totals for a difference most readers will not notice; it can follow without a schema change to the hourly tier. |
| D6 | *Default.* **Rollups are computed by a scheduled job, not by continuous aggregates.** | A continuous aggregate cannot `COUNT(DISTINCT ...)`, and visitors are exactly that. The two existing aggregates stay for the list row's "served 3 minutes ago". |
| D7 | *Default.* **Response time is a fixed log-scale histogram** (64 buckets from 0.5 ms, each about 19% wider than the last) on every hourly and daily row, so the median and 95th percentile of any range are merged exactly from the buckets and read to within about 10%. | Medians cannot be combined from medians, and percentiles over 30 days of raw rows at read time would be a slow query on every page load. |
| D8 | *Owner.* **Country comes from DB-IP's IP-to-Country Lite database** (CC BY 4.0), **downloaded once a month by the cluster** and kept in its database (`edge_geo_country`); every edge replica loads it from there. Only the ISO country code is stored on a request. Its attribution ("IP Geolocation by DB-IP", linked to db-ip.com) stands under the Countries list. | The spec first pinned one monthly file into the edge image. DB-IP serves only its current and previous month (an older month answers 404, checked 2026-11-08), so a pinned build would break within about two months; the owner chose the monthly download over mirroring a copy as a repository release asset or downloading whatever month exists at build time. MaxMind's free database needs an account key and a stricter licence, and the embedded-data Go modules carry share-alike data. City-level location is never stored. |
| D9 | *Default.* **Bots are kept out of visitors and page views and counted under Health.** A request is a bot when its user agent is empty or names a known crawler, preview fetcher, monitor or headless browser (a closed list in one file). | A figure a client reads as "people" must not be search crawlers. Bots that imitate browsers are still counted, and the help says so. |
| D10 | *Default.* **Email facts are a narrow side table filled through the engine.** A scheduled job reads new or changed deliveries, engagement events and consent events through the engine, using `@serverOnly` queries ordered by row creation time from a stored watermark, and records one fact per source record in `email_stat_fact`, keyed so a re-run never counts twice. | Grouped counts over graph rows cannot be pushed down, and hand-written SQL over the graph is the measured hazard in section 2. Counters incremented by each writer would drift across the three node paths that write these rows. |
| D11 | *Default.* **Reads narrow through the caller, never through the picker.** `siteStatistics` keeps the site narrowing `readableSites` already does. `emailStatistics` intersects the requested organizations with `ResolveAccessGrant` for the caller, and a failed resolution answers nothing. | The client picker is presentation. A person who edits a request to name another client's site or organization gets an empty answer, indistinguishable from a client with no activity. |
| D12 | *Owner.* **Client members use the existing Member role in their organization's group.** No Moderator role. | The group decides whose rows a person reaches; the role decides what they may do with them. Member already reads and writes an organization's data plane. |
| D13 | *Default.* **The client control appears only when there is a choice.** Staff (`everyAccount`) get All clients plus each organization. Someone in more than one organization gets All my clients plus each of theirs. Both open on All. A member of one organization never sees it; their scope is their organization. | R5. A control with one option is furniture, and a control that lists other clients would disclose them. |
| D14 | *Owner.* **Email's Overview grows into the statistics page** and keeps its name. Deployables' Overview gains a **Websites** section with every website the person may see added together. | One place for each app's figures; Overview is the shell's summary destination. |
| D15 | *Default.* **The deployable page is called Statistics**, with a chart icon, replacing Traffic. | The owner's word. |
| D16 | *Default.* **System-owned sites stay unrecorded**, as today. | Measuring the console somebody reads a figure in would be measuring the act of looking. |
| D17 | *Owner.* **A visit is one visitor's page views with no gap over 30 minutes**, counted in the hour and the day it started, so hourly and daily visits add up without double counting. A bounce is a one-page visit. Views per visit is page views over visits; bounce rate is bounces over visits. A visit that crosses midnight UTC splits in two, because the visitor hash changes. | The standard session definition; computed from page views and the visitor hash, with no cookie. Because a visit is segmented over a whole day's page views, every rollup run recomputes every hour of today. |
| D18 | *Owner.* **Email facts are attributed to the rule, journey and audience they belong to.** A rule send carries `delivery.emailRuleId`. A journey send carries `delivery.requestId` = `journey-<enrollment>-<step>` (`component/campaigns/journey.go`, `journeyRequestID`); the enrollment's `journeyId` is read through the engine. A subscribe or unsubscribe is a consent event naming a recipient, and the recipient's `audienceId` is read the same way, in batches; an event with no recipient counts in its organization's totals only. **Opens and clicks are counted for campaign mail only**: `engagementEvent.campaignId` is required, so rule and journey sends have no engagement rows, and the open and click rates are of campaign sends. | Each Overview section the owner chose needs one of these attributions. Tracking opens and clicks for automated mail is an owner-agreed follow-up, out of scope here. |

## 4. What the edge records

`edge.RequestRecord` and `sitetraffic.Record` gain the fields below. The handler
computes them after the response, in memory, and the IP address and user agent
are dropped before the record reaches the sink. Nothing new blocks the serving
path: classification is string matching, the country lookup is an in-memory
database, and the secret is read from an atomic held by a keeper goroutine.

| Field | Value |
| --- | --- |
| `PageView` | D2. False for bots. |
| `Path` | For a page view only: the request path with no query string or fragment, a trailing slash removed except at the root, at most 256 bytes. Empty otherwise. |
| `Source` | For a navigation page view only: the landing URL's `utm_source` (lowercased, at most 64 bytes); otherwise the `Referer` host with `www.` removed, when it is not the site's own host or one of its client domains; otherwise `direct`. Empty for a navigation from the site's own pages and for every ping, and empty values stay out of the Sources list, so it counts arrivals rather than clicks inside the site. A short closed map folds common hosts (`l.facebook.com` to `facebook.com`, `t.co` to `x.com`, any `google.<tld>` to `google`). |
| `Country` | ISO 3166-1 alpha-2 from the loaded D8 database, or empty when no database is loaded or the address is not in it. |
| `Device` | `desktop`, `mobile`, `tablet` or empty. |
| `Browser` | `Chrome`, `Safari`, `Firefox`, `Edge`, `Opera`, `Samsung Internet`, `Other`, or empty. |
| `OS` | `Windows`, `macOS`, `iOS`, `iPadOS`, `Android`, `Linux`, `ChromeOS`, `Other`, or empty. |
| `Bot` | D9. |
| `Visitor` | D1, hex. Empty for bots and whenever no secret for the request's UTC day is held. |

**The client address** is the right-most `X-Forwarded-For` entry, falling back
to the peer address: the same rule `access.go` uses (`accessRightMostForwarded`),
promoted to one helper both use. The right-most entry is the one the front door
wrote; entries to its left are whatever the client sent.

**The visitor secret** lives in `edge_visitor_secret (day DATE PRIMARY KEY,
secret BYTEA NOT NULL)`. Each edge replica runs a keeper that, at start and at
every UTC midnight, inserts the day's row if it is missing (`ON CONFLICT DO
NOTHING`), reads it back, swaps it into memory and deletes every row for an
earlier day. Two replicas therefore hash one visitor identically, and after
midnight no stored secret can link a stored hash to an address. A keeper that
cannot read the day's secret records requests with no visitor, so visitors
read low rather than reusing a secret across days. The table is not a concept
and no query reaches it.

The migration adds the columns to `edge_request` with empty defaults, so rows
already recorded read as unmeasured for visitors and pages and keep counting
for health.

## 5. Storage and rollups

Three new plain tables, small by construction (one row per site per hour or
day, plus at most 101 rows per site, day and dimension). The rollup run deletes
the rows past each table's retention, so the same code serves TimescaleDB and
plain Postgres (changed during planning, 2026-11-08, from hypertables with
retention policies):

| Table | Grain | Kept |
| --- | --- | --- |
| `edge_stats_hourly` | site, hour | 30 days |
| `edge_stats_daily` | site, UTC day | 400 days |
| `edge_stats_daily_top` | site, UTC day, dimension, value | 400 days |

The hourly and daily rows carry `visitors`, `visits`, `bounces`, `page_views`,
`requests`, `errors` (5xx), `not_found` (4xx), `bytes`, `bot_requests` and
`duration_hist` (D7). `visitors`, `visits`, `bounces` and `page_views` are
NULL for a period that has request rows but no visitor fields, which is every
period before this ships. A visit is counted in the hour and day it started
(D17). The top table holds, per dimension (`page`, `source`, `country`,
`device`, `browser`, `os`), the 100 values with the most visitors that day plus
one `(other)` row for the rest, each with `visitors`, `visits` and
`page_views`. A visit counts under the page and the source it started on, and
under its first page view's country, device, browser and OS.

**The job.** A scheduled automation in `dsl/platform/automations.memql` runs
every five minutes on the cron leader and calls the builtin
`siteStatisticsRollup` (`component/sitetraffic`). For every site with request
rows in the window, it recomputes every hour of today and today itself, and
yesterday's hours and yesterday until 02:00 UTC, from `edge_request`, as
upserts. Every hour of today, because a visit is segmented over the day's page
views (D17). A recompute is the
whole computation for that period, so running twice changes nothing. On its
first run it backfills every day still in `edge_request`. The run is bounded
(sites per run, statement timeout), logs what it did, and a period it could not
finish is recomputed on the next run.

**The country database.** `edge_geo_country (id SMALLINT PRIMARY KEY, release
TEXT NOT NULL, fetched_at TIMESTAMPTZ NOT NULL, data BYTEA NOT NULL)` holds one
row. A scheduled automation runs daily on the cron leader and calls the builtin
`geoCountryRefresh` (`component/sitetraffic`). It does nothing when the stored
release is the current month. Otherwise it downloads
`https://download.db-ip.com/free/dbip-country-lite-YYYY-MM.mmdb.gz` for the
current month, falling back to the previous month on a 404, decompresses it
(refusing anything over 64 MiB), checks that it opens as an MMDB country
database, and replaces the row in one statement. A failed download keeps the
stored release. Each edge replica loads the row at start and checks the stored
`release` every hour, swapping a newer database in atomically. A replica with no
database records no country. This is the cluster's only new outbound call: one
HTTPS request a day at most, and a download once a month.

**Email facts.** `email_stat_fact (kind TEXT, source_id TEXT, account_id TEXT,
campaign_id TEXT, rule_id TEXT, journey_id TEXT, audience_id TEXT, day DATE,
PRIMARY KEY (kind, source_id))`, indexed on `(account_id, day)` and kept 400
days. `rule_id`, `journey_id` and `audience_id` come from D18. Kinds and their
source records:

| Kind | Source | Day |
| --- | --- | --- |
| `sent` | delivery | `sentAt` |
| `delivered`, `undelivered` | delivery's provider status: `delivered` and `expanded` are delivered; `bounced`, `suppressed`, `quarantined`, `filtered-spam` and `failed` are undelivered | `providerAt`, else the version's time |
| `failed`, `skipped` | delivery | when it reached that status |
| `suppressed` | a skipped delivery whose `skipReason` is `hard_bounce`, `complaint`, `manual`, `bounced` or `complained`, the set `campaignStats` calls suppressed | when it reached that status |
| `open`, `click` | engagement event | `occurredAt` |
| `open_unique`, `click_unique` | delivery (first open, first click) | `occurredAt` of the first one |
| `unsubscribe`, `bounce`, `complaint`, `subscribe` | consent event (`withdraw`, `bounce`, `complaint`, `grant`) | `occurredAt` |

A scheduled automation in `dsl/campaigns/automations.memql` runs every five
minutes and calls `emailStatisticsRollup` (`component/campaigns`). For each of
the three source concepts it pages through rows created since that concept's
watermark (`email_stat_watermark`), less a five-minute overlap for clock skew
between writers, inserting facts with `ON CONFLICT DO NOTHING`. The first run
starts 400 days back and catches up a bounded number of pages per run. Facts
with an empty `account_id` (rows older than organization attribution) are only
ever counted for staff under All clients.

## 6. The page-change ping (HTTP exception)

Two routes on every non-system deployable's own origin, answered by the edge
itself and never proxied. **Changed during planning (2026-11-08):** the edge
already inserts its own `/_memql/site-refresh.js` into every HTML document it
serves (`component/edge/site_refresh.go`), so the page-change script rides the
same insertion instead of being a tag each site adds. Clients change nothing,
and the Statistics page needs no "copy this tag" notice.

- `GET /_memql/view.js`: a static script of about 1 KB, served from the edge
  binary as `text/javascript` with a long cache lifetime. It wraps
  `history.pushState` and listens for `popstate`. When `location.pathname`
  changes it sends `navigator.sendBeacon("/_memql/view", path)`. It never
  reports the first load, which the edge already counted (D2), reads no cookie
  and stores nothing in the browser.
- `POST /_memql/view`: the body is the path as plain text (so `sendBeacon` sends
  it without a CORS preflight), at most 1 KiB. The edge accepts it only when
  `Origin` equals the site's own origin, or `Sec-Fetch-Site` is `same-origin`
  when `Origin` is absent. It applies a per-address rate limit keyed on the same
  right-most address as section 4, records a page view with `Path` taken from
  the body (normalized as in section 4) and visitor, country and device fields
  from the request, and answers 204. Anything else answers 204 and records
  nothing, so the route discloses nothing about a site's configuration. It reads
  no cookie and forwards nothing to the bff.

Both routes fit the existing site content policy (`script-src 'self'`,
`connect-src 'self'`, `component/edge/csp.go`). The edge inserts
`<script src="/_memql/view.js" defer></script>` after `<head>` in every HTML
document of a live, non-system deployable, next to the site-refresh tag, and
extends the document's ETag with a `view-v1` marker so caches do not keep a copy
without it. On a multi-page site the script never fires, because every page is a
navigation. CLAUDE.md's Allowed HTTP Exceptions table gains a row for this
surface, dated 2026-11-08, beside the access-code and shopper routes.

## 7. Reads

**`siteStatistics`** (`@sdk`, `integration.siteTraffic.statistics`, declared in
`dsl/platform/builtins.memql`):

- Takes `siteIds` (at most 200, the existing cap), `accountIds` and `period`
  (`24h`, `7d`, `30d`, `90d`, `12m`). Empty `siteIds` means every non-system
  deployable the caller may read, kept to the named organizations when
  `accountIds` is given; the Overview's Websites section asks this way, so it
  never pages through ids (changed during planning, 2026-11-08).
- Narrows `siteIds` through `readableSites`.
- Returns one object:
  - the bounds of the period and the period before it, and the bucket size
  - `totals` and `previous`: visitors, visits, bounces, page views, views per
    visit, bounce rate, requests, errors, not found, bytes, bot requests, and
    the median and 95th-percentile response times
  - `series` and `previousSeries`: one entry per bucket (visitors, visits,
    bounces, page views, requests, errors, not found, 95th-percentile response
    time), aligned by offset, so the previous period can be drawn under the
    current one
  - `top`: the 50 leading values of each dimension, summed over the period's
    days, each with visitors, visits and page views, and an `other` remainder
  - `sites`: per-site visitors, visits and page views, for the Websites list
  - `measuredSince`: the first day with visitor data, so the page can say where
    the line starts instead of drawing zeroes before it
- Any figure nothing measured is absent, never zero. A 24-hour period reads the
  hourly table for its series, and its visitor total is distinct over the
  request rows themselves, because hourly visitor counts do not add up. The
  other periods read the daily tables, and 12 months sums days into months:
  the current month and the 12 before it. The current partial bucket is
  included. A 12-month period returns no `previous` and no `previousSeries`
  (D4).

**`emailStatistics`** (`@sdk`, `integration.campaigns.statistics`):

- Takes `accountIds` (empty means every organization the caller reaches) and
  `period` (`7d`, `30d`, `90d`, `12m`).
- Narrows `accountIds` by `ResolveAccessGrant` (D11).
- Returns one object:
  - `totals` and `previous`: sent, delivered, undelivered, failed, skipped,
    suppressed, opens, clicks, unique opens, unique clicks, unsubscribes,
    bounces, complaints and new subscribers
  - the open rate and click rate: of the **campaign** messages sent in the
    period, the share opened (clicked) at least once, joined on the delivery id
    (D18)
  - `series` and `previousSeries`: sent, unique opens, unique clicks,
    unsubscribes, new subscribers, bounces, complaints, failed and suppressed
    per bucket, plus each bucket's open and click rate by send-day cohort
  - `campaigns`: each campaign with sends in the period, with sent, unique
    opens, unique clicks and both rates, the top 50 by unique opens, then
    unique clicks
  - `automations`: each rule and journey with sends in the period, with its
    kind (`rule` or `journey`), sent, failed and suppressed
  - `audiences`: each audience with subscribes or unsubscribes in the period,
    with new subscribers, unsubscribes and the net change
  - `accounts`: per-organization sent and rates, only when more than one
    organization is in scope
- The two rates are absent when no campaign mail was sent, and every figure is
  absent for a period the facts table does not yet cover. A 12-month period
  returns no `previous` and no `previousSeries` (D4).

Both reads are re-read by the page on `AutoRefresh` (five minutes for 24
hours, fifteen otherwise) while the page is visible. Neither is cached: the
engine answers a top-level builtin before its result cache is consulted, so
that cadence is what bounds their load (corrected at the final review,
2026-11-09; this record first said both were cached for 60 seconds).

## 8. Shared pieces in the kit

New, in `clients/os/src/kit/statistics/`, and the only way any app draws
statistics:

- **`PeriodControl`**: a quiet menu button at the right of a Head or Subhead
  reading "Last 30 days", in the manner of `SortControl`. It lists the periods
  the read offers and is remembered in the app's settings document
  (`DeployablesSettings.statisticsPeriod`, replacing `trafficWindow`, and the
  Email settings store).
- **`ScopeControl`** and `useClientScope()`: the client picker beside it, drawn
  only by the rule in D13. The hook reads `access` and the account options the
  page already holds and returns the scope a read sends. The choice is
  remembered per app.
- **`StatFigures`**: the figure row. Each figure shows its value, its change
  from the previous period ("+12%", "New", or nothing when the previous period
  is unmeasured) and its tone, and is one option of a `radiogroup` that decides
  what the chart draws. Rates and averages are shown but are only selectable
  when the series carries them.
- **`TimeChart`**: one SVG chart.
  - The current period is a line with a soft fill in the theme accent; the
    previous period is a faint dashed line.
  - The value axis has three labelled steps and the time axis has labels the
    width allows.
  - Hover, focus and arrow keys move a readout (date, value, previous value).
  - A `figcaption` gives screen readers the whole series in one sentence.
  - Absent buckets are gaps, never zeroes. Colours come from theme tokens in
    light and dark, and motion stops under reduced motion.
- **`RankedList`**: a label, a figure and a faint proportional bar behind each
  row, showing the top 8. Its "All pages" (sources, countries...) opens a full
  list as a page in the app under the trail, never a pop-up.
- **`changeOf()`** in `kit/measure.ts`: the change between two figures, absent
  when either is absent.

DESIGN.md gains a **Statistics** section saying that these pieces are the
statistics language, where the controls stand, how change and absence read,
and that a chart is drawn only from a real time series. The `Overview` rule
"only draw activity history when a real time series exists" stays true; these
are those series.

## 9. The pages

**Deployable, Statistics** (`detail: "statistics"`, replacing `"traffic"`):

- Head: Statistics, with `PeriodControl` at the right.
- Figures: Visitors, Visits, Page views, Views per visit, Bounce rate.
- The chart of the selected figure.
- Pages and Sources side by side on a wide page and stacked on a narrow one,
  then Countries and Devices. Devices uses `LocalTabs` for Device, Browser and
  OS, and the DB-IP attribution stands under Countries.
- **Health** (`Subhead`): its own figure row (Requests, Errors, Not found,
  Response time p95) and chart. Data served, bot requests and the median
  response time are `Facts`.
- States:
  - while reading: the shapes of figures, chart and lists
  - a period with request rows and no visitor fields: says where visitor
    counting starts
  - a draft: nothing, as today
  - a refused read: the engine's sentence and automatic retry

**Deployables, Overview:**

- The existing figures, status breakdown and map stay.
- A **Websites** `Subhead` carries `ScopeControl` and `PeriodControl`, then
  Visitors, Visits and Page views over every website in scope, the chart, and
  a `RankedList` of websites; a row opens that deployable's Statistics. Below
  it, Sources and Countries as `RankedList`s summed over the websites in scope.

**Email, Overview:**

- Head: Overview, with `ScopeControl` and `PeriodControl`.
- Figures: Sent, Open rate, Click rate, Unsubscribes, New subscribers. The
  current Subscriptions and Active automations stay as `Facts` beneath, because
  they are about now, not the period.
- The chart.
- **Delivery problems** (owner, 2026-11-08): its own `StatFigures`, Bounces,
  Complaints, Failed and Suppressed, and a `TimeChart`. Delivered is a `Fact`
  beside them only where the provider reports outcomes (some delivered or
  undelivered fact in scope). Bounces are consent `bounce` events, the source a
  campaign's results page counts, so the two pages agree.
- **Audience growth** (owner): each audience as a `RecordRow` with new
  subscribers, unsubscribes, net change and current size (the existing
  subscriber count) as quiet text; a row opens the audience.
- **Top campaigns** (owner): a `RankedList` by unique opens, then unique
  clicks, showing the open and click rates; a row opens the campaign.
- **Automations** (owner): each rule and journey as a `RecordRow` with sent,
  failed and suppressed; a row opens it.
- **Campaigns**: the existing status breakdown.
- Under All clients only, **Clients**: a `RankedList` of organizations by sent,
  with open rate.
- The first-use empty state stays.

Copy follows DESIGN.md: say it once, no standing explanations. Help text goes in
`PageHelp`: what a visitor, a visit and a bounce are, UTC days, what counts as
a page view, bots, and the DB-IP source; and on Email, that automations show no
opens or clicks because automated mail is not tracked for them (D18).

## 10. No pop-ups

- **Visit, on a store deployable**, opens a **Visit** page (`detail: "visit"`)
  holding today's Testing and Production rows, refusals and fallback link. Every
  other kind keeps its direct link to the live site.
- **A version's details** open a **Version** page (`detail: "version"`, with the
  version's ref) under `Deployables > <name> > Version <label>`. While it is
  open, the page's `ActionBar` offers **Roll back to this version** when that is
  legal and omits it otherwise (DESIGN.md rule 12). The refusal stands in place.
- **Remove source** confirms in place beside its trash icon (`InPlaceAsk`), with
  the consequence sentence, the confirmation hold, and focus returned on cancel
  (DESIGN.md, "Edits together").
- `page/DetailDialog.tsx` is deleted. A test fails the build if anything under
  `apps/deployables` imports `kit/Dialog` again.

## 11. Which node holds what

| State | Where | How it crosses |
| --- | --- | --- |
| The day's visitor secret | Postgres (one row), plus memory on every edge replica | Each keeper reads the same row; no event bus. |
| The country database | Postgres (one row), downloaded by the cron leader, plus memory on every edge replica | Each replica re-reads the stored release hourly and swaps in a newer one. |
| Request rows | The edge replica that served them, then Postgres through the sink | Unchanged. |
| Rollups and email facts | Written by the cron leader's scheduled automation | One writer per run; idempotent upserts survive a leader change mid-run. |
| Reads | Any bff replica | Pure reads of shared tables, narrowed under the caller's actor. |
| The ping's rate limit | Per edge replica, in memory | Bounds load per replica, as the access-code limits do; not a guarantee across replicas. |

Nothing here needs a new routing rule: no new event-bus topic is introduced.

## 12. Testing

- **Edge, unit:**
  - user-agent classification, with a table of real strings for every family
    plus bots
  - page-view detection across the `Sec-Fetch-*` combinations, prefetch and the
    fallback rule
  - path and source normalization
  - right-most address selection
  - the visitor hash: stable within a day for one site, different across days
    and across sites
  - the keeper: two keepers on one database agree, a missing secret yields no
    visitor, and old days are deleted
  - the ping: origin and same-origin rules, size cap, rate limit, 204 in every
    case, a page view recorded only when accepted, and `view.js` content type
    and caching
  - the insertion: the `view.js` tag in a live site's HTML documents and not in
    a system-owned site's, and the ETag marker
- **Country database:** against a local HTTP server, the refresh
  - downloads the current month and falls back to the previous one on a 404
  - refuses a body that is not a country MMDB or is over the size cap
  - keeps the stored release when a download fails
  - does nothing when the current month is stored
  - and the edge loader swaps in a newer release and keeps serving when none
    exists
- **Rollups, db-gated (TimescaleDB image):**
  - hourly, daily and top rows from fixture requests
  - re-running changes nothing
  - backfill
  - NULL visitors for pre-feature rows
  - the `(other)` remainder
  - histogram percentiles against known durations
  - retention
  - visits: a 30-minute gap starts a new visit, a one-page visit is a bounce, a
    visit counts in the hour it started, a visit across midnight UTC splits in
    two, and top rows count a visit under the page and source it started on
- **`siteStatistics`, db-gated:**
  - each period's buckets, the previous period and absent figures
  - 12 months: 13 monthly buckets and no previous period
  - a client member reads their organization's sites and gets nothing for
    another organization's site even by id
  - staff read all
- **Email facts and `emailStatistics`, db-gated:**
  - facts from fixture deliveries, events and consent rows
  - idempotent re-runs
  - the watermark overlap
  - cohort rates over campaign mail only
  - attribution: a rule send to its rule, a journey send to its journey through
    its request id, a subscribe or unsubscribe to its recipient's audience, a
    consent event with no recipient to its organization's totals only, and a
    suppressed skip as `suppressed`
  - 12 months has no previous period
  - narrowing by organization, including a member asking for another
    organization by id
  - All clients for staff, and empty-organization facts only for staff
- **Cross-node:** two edge replicas against one database produce one visitor
  for the same request on the same day (an in-process hop test, in the way of
  `forward_hop_test.go`).
- **OS (under `clients/os/test`):**
  - pure modules: periods, change, scales, scope rules
  - `StatFigures` keyboard and selection, `TimeChart` gaps and summary,
    `RankedList` and its full-list page
  - the Statistics page's states
  - Overview Websites
  - Email Overview for staff, for a multi-organization member and for a
    single-organization member, who sees no client control
  - Visit and Version pages, Roll back in the bar, Remove source in place
  - no dialog under Deployables
- **Gates:** `make test`, the db-gated trees with `MEMQL_REQUIRE_DB=1`, the OS
  typecheck and tests, and the documentation, front-door and HTTP-surface gates.
- **Rendered:** screenshots of every new surface in light and dark, at desktop
  and narrow widths, empty and populated, through the supported local path
  (`make dev`). The owner is told before the local cluster is touched.

## 13. Documentation changed with the code

- CLAUDE.md: the Allowed HTTP Exceptions row for section 6.
- `clients/os/DESIGN.md`: the Statistics section, and the Deployables lines
  that still mention Traffic or a dialog.
- `component/sitetraffic` and `component/edge/requestlog.go` package comments:
  what is now recorded, why, and what is still never stored.
- `docs/public/operate/deployables.md`, "Traffic and health", becomes
  "Statistics and health": what is recorded, retention per tier, the ping and
  its tag, the `siteStatistics` read, and a paragraph an operator can adapt for
  a client's privacy policy, and the monthly country download (the cluster's
  one new outbound call). The request-log section of
  `docs/public/operate/site-hosting.md` changes with it.
- `docs/public/operate/campaign-sending.md`: the Overview's figures, their
  definitions and `emailStatistics`.
- The root `NOTICE`: the DB-IP attribution the CC BY 4.0 licence asks for.

## 14. Known limitations

- **Visitors are per day.** A person visiting on several days counts once per
  day in a longer period. People behind one address with the same browser
  count as one; one person on two networks counts as two.
- **Days are UTC** (D5).
- **Imitating bots count.** Bots that imitate browsers are counted as visitors.
- **In-app pages need JavaScript.** A single-page app's in-app pages are
  counted by the inserted script, so a visitor with scripts disabled counts only
  the page they landed on.
- **Older days lack visitors.** Days recorded before this ships have health
  figures and no visitors or pages.
- **Opens are approximate.** Mail privacy proxies fetch tracking pixels, so
  email opens are an upper bound, as they are everywhere. Clicks are not
  affected.
- **Countries are approximate.** Country accuracy is DB-IP Lite's.
- **Offline clusters have no countries.** A cluster that cannot reach
  download.db-ip.com shows countries as not measured.
- **Visits split at midnight UTC.** The visitor hash changes, so one sitting
  across midnight reads as two visits.
- **Automated mail has no opens or clicks.** Rule and journey sends carry no
  engagement tracking, so their rows show sent, failed and suppressed only.
- **Twelve months has no comparison** (D4).

## 15. Out of scope

Scheduled or emailed reports, export, custom date ranges, per-site time zones,
goals and conversions, a live "visitors now" figure, and open and click
tracking for automated mail (an owner-agreed follow-up).

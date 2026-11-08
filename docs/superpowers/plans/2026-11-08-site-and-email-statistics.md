# Website and Email Statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace a deployable's Traffic page with website statistics, grow Email's Overview into per-client statistics, scope both to the organizations a person belongs to, and move the last Deployables pop-ups into pages.

**Architecture:**
- **Edge.** It classifies each request in memory (page view, path, source, country, device, bot, daily-secret visitor hash) and writes only derived fields to `edge_request`. It also inserts and answers an approved page-change ping.
- **Scheduled builtins.** Run on the cron leader every five minutes, they fold requests into hourly and daily tables. A third fills an email facts table by walking campaign rows through the engine.
- **Reads.** Two `@sdk` builtins read those tables, narrowed under the caller's actor.
- **OS.** Shared kit pieces draw both apps' pages.

**Tech Stack:**
- Go: bun, TimescaleDB/Postgres, `github.com/oschwald/geoip2-golang`, and `github.com/maxmind/mmdbwriter` for tests only.
- MemQL DSL.
- React + TypeScript with Vitest, SVG for the chart.

**Spec:** `docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md`. Every task argues from it. Section numbers (§) below refer to it.

## Global Constraints

- **Worktree.** Work in `/Users/znas/projects/memql/.claude/worktrees/site-email-statistics` on `feat/site-and-email-statistics`. Never switch branches in `/Users/znas/projects/memql`, because another session uses it.
- **Commits.**
  - Stage explicit paths only.
  - Write commit messages to a file and use `git commit -F`.
  - End every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
  - `git push` the branch at the end of each phase. Never open a pull request; the owner merges.
- **Public repository.** No client or person names in code, tests, docs or commits. Never describe MemQL as a database (`TestNoDatabaseProductClaims`).
- **Visitor hash.** `HMAC-SHA256(secret, siteId + "|" + addressKey + "|" + userAgent)`, first 16 bytes, lowercase hex (32 chars).
  - The secret is 32 random bytes per UTC day, in `edge_visitor_secret`. Rows for earlier days are deleted at rotation.
  - The IP address and user agent are never written anywhere.
- **`addressKey`.** The right-most valid `X-Forwarded-For` entry, else the peer address. Unmapped; IPv6 keys on its /64. This is today's access-code rule, moved to `visit.AddressKey`.
- **Page view (D2).** Spelled once in Task 1 and everywhere else by reference.
  - A `GET` with `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document`, answered 200 or 304, where neither `Sec-Purpose` nor `Purpose` mentions `prefetch` or `prerender`.
  - Without `Sec-Fetch-*` headers: a `GET` whose `Accept` contains `text/html`, with path class `document` or `fallback`.
  - Bots are never page views. Accepted pings are.
- **Retention.**
  - `edge_request`: unchanged (`MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS`, default 30).
  - `edge_stats_hourly`: 30 days.
  - `edge_stats_daily`, `edge_stats_daily_top` and `email_stat_fact`: 400 days.
- **Periods.** Days are UTC.
  - Sites: `24h | 7d | 30d | 90d | 12m`. Email: `7d | 30d | 90d | 12m`.
  - Buckets: `24h` is hourly, `7d`/`30d`/`90d` daily, `12m` monthly.
  - The previous period is the same length, immediately before, for every period except `12m`. `12m` has no previous period and spans the current month plus the 12 before it, 13 monthly buckets (spec D4). Reads return no `previous`/`previousSeries` for it, figures show no change, and the chart draws no previous line.
  - Defaults: Deployables `7d`, Email `30d`. The client scope defaults to `""` (All).
- **Visits (spec D17).** A visit is one visitor's page views with no gap over 30 minutes. It is counted in the hour and day it started, and it splits at midnight UTC because the visitor hash changes.
  - A bounce is a one-page visit.
  - Views per visit is `page views / visits`; bounce rate is `bounces / visits`.
- **Email attribution (spec D18).**
  - Rule sends attribute through `delivery.emailRuleId`.
  - Journey sends attribute through `delivery.requestId` = `journey-<64-hex enrollment>-<step>`, then the enrollment's `journeyId`.
  - A subscribe or unsubscribe attributes through its recipient's `audienceId`.
  - Open and click rates count campaign mail only.
- **Absent is not zero.** An unmeasured number is JSON `null` in every payload. The OS draws it with kit `Measure` (em dash plus reason) and shows no change badge.
- **Reads narrow under the caller.** Sites go through `readableSites`, email through `ResolveAccessGrant`. A caller with nothing in scope gets an answer with every figure `null`, never an error.
- **System-owned sites** are never recorded and never get statistics.
- **Country data.**
  - Source: `https://download.db-ip.com/free/dbip-country-lite-YYYY-MM.mmdb.gz`.
  - Caps: compressed body 32 MiB, decompressed 64 MiB.
  - Attribution: the text "IP Geolocation by DB-IP", linked to `https://db-ip.com`.
- **New HTTP surface:** only `GET /_memql/view.js` and `POST /_memql/view`, with a body of at most 1024 bytes. The POST always answers 204.
- **No new event-bus topics or routing rules.**
- **UI.**
  - Follow `clients/os/DESIGN.md` and use kit components only.
  - No dialogs under `apps/deployables`.
  - Loading is shapes, never words.
  - Email: no disabled `Button` anywhere (`test/email/corrections.test.ts`).
  - No emojis.

## Review Focus

1. **Forged forwarding header.** A client-written left-most `X-Forwarded-For` must change neither the visitor nor the country. Tested in Task 1 (`TestClientAddressIgnoresAForgedLeftMostEntry`) and Task 3 (`TestANavigationIsRecordedAsAPageViewWithItsFields`).
2. **UTC midnight.**
   - `23:59:59.999` and `00:00:00` hash under different secrets, and no keeper ever reuses yesterday's.
   - Yesterday's rollup is recomputed after midnight.
   - Tested in Task 2 (`TestARequestFromYesterdayGetsNoVisitorAfterRotation`) and Task 6 (`TestYesterdayIsRecomputedUntilTwoAfterMidnight`).
3. **Cross-client reach.** A client member who names another organization's site id or account id gets an empty answer: neither a refusal nor the other organization's figures. Tested in Task 8 (`TestAClientMemberGetsNothingForAnotherClientsSiteEvenById`), Task 10 (`TestAMemberGetsOnlyTheirOrganization`), Task 16 and Task 17.
4. **Ping abuse.** Each of these answers 204 and records nothing:
   - a flood from one address
   - an oversized or non-path body
   - another origin
   - no `Origin` and no `Sec-Fetch-Site`

   Tested in Task 4.
5. **History before this ships.** A period with request rows but no visitor fields shows visitors as not measured, with the date counting started, never 0, and shows no change badge. Tested in Task 6 (`TestRequestsWithoutVisitFieldsRollUpToNullVisitors`), Task 8 (`TestAPeriodBeforeVisitorCountingIsNullNotZero`), Task 11 (`changeOf` with an absent previous) and Task 15 (`TestBeforeVisitorCountingTheFiguresSaySoInsteadOfZero`).

## Working setup (once)

```bash
cd /Users/znas/projects/memql/.claude/worktrees/site-email-statistics
make identity-tailwind          # root-module tests embed the generated identity CSS
make os-install                 # builds sdk/ts, installs clients/os
```

Database-gated tests run against the CI image on port 55432. Port 5432 is the k3d load balancer, which accepts and then drops connections.

```bash
docker run -d --name memql-stats-db -p 55432:5432 -e POSTGRES_USER=memql -e POSTGRES_PASSWORD=memql_dev -e POSTGRES_DB=memql ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c9cc49bdbf1e935234e8c87ab64b6d6d195fd2ecedff7b00185557631
```

```bash
export MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:55432/memql?sslmode=disable'
```

If the image pull is refused, build the local operand with `make db-image`, run the tag it prints with the same flags, and say so in the task report.

**Go tests always name the module path**, e.g. `go test -count=1 github.com/znasllc-io/memql/component/edge/...`. A relative `./...` silently misses workspace modules.

**One OS test file** (after `make os-install`):

```bash
cd clients/os && npm test -- test/deployables/statistics.test.tsx
```

**SDK regeneration** after an `@sdk` builtin changes:

```bash
make sdk-gen && bash scripts/os/build.sh deps
```

## File structure

| Path | Responsibility |
| --- | --- |
| `component/edge/visit/` (new) | Pure request classification: agent, page view, path, source, client address. |
| `component/edge/view.go`, `view.js` (new) | The page-change script and ping. |
| `component/sitetraffic/visitors.go`, `countries.go`, `countryrefresh.go` (new) | Daily secret keeper; country database loader and monthly refresh. |
| `component/sitetraffic/histogram.go`, `rollup.go`, `statistics.go` (new) | Response-time histogram; hourly/daily rollup; the `siteStatistics` read. |
| `core/statsperiod/` (new) | Period parsing and bounds, shared by both reads. |
| `component/emailstats/` (new plug-in `emailStats`) | Email facts rollup and the `emailStatistics` read. |
| `clients/os/src/kit/statistics/` (new) | `period.ts`, `scale.ts`, `TimeChart`, `StatFigures`, `RankedList`, `PeriodControl`, `statistics.css`. |
| `clients/os/src/apps/accounts/clientScope.ts`, `ScopeControl.tsx` (new) | The client-scope rule (D13) and its control. |
| `clients/os/src/apps/deployables/statistics/` (new) | Read, hook, Statistics page, full-list page, Websites section. |
| `clients/os/src/apps/email/overview/statistics.ts` (new) | Read and hook for the Email Overview. |
| `clients/os/src/apps/deployables/page/VisitPage.tsx`, `VersionPage.tsx` (new) | The two former dialogs as pages. |

---

## Phase 1: what the edge records

### Task 1: Request classification package

**Files:**
- Create: `component/edge/visit/agent.go`, `pageview.go`, `source.go`, `address.go` and a `_test.go` for each
- Modify: `component/edge/access.go`. Replace the bodies of `accessClientAddress`, `accessRightMostForwarded` and `accessAddressKey` with calls to `visit.ClientAddress` / `visit.AddressKey`. Behaviour is identical, so `access_test.go` passes unchanged.

**Interfaces:**
- Produces (package `visit`):
  ```go
  type Agent struct{ Device, Browser, OS string; Bot bool }
  func Classify(userAgent string) Agent
  func IsPageView(r *http.Request, pathClass string, status int) bool
  func NormalizePath(p string) string
  func Source(r *http.Request, ownHosts ...string) string
  func ClientAddress(r *http.Request) (netip.Addr, bool)
  func AddressKey(a netip.Addr) string
  ```
- Closed sets:
  - Device: `desktop`, `mobile`, `tablet`, `""`.
  - Browser, in match order: `Edge` (`Edg/`, `Edge/`), `Opera` (`OPR/`, `Opera`), `Samsung Internet` (`SamsungBrowser`), `Firefox` (`Firefox/`, `FxiOS`), `Chrome` (`Chrome/`, `CriOS`), `Safari` (`Safari/` with `Version/`), `Other`, `""` (empty UA).
  - OS, in match order: `iPadOS` (`iPad`), `iOS` (`iPhone`, `iPod`), `Android`, `Windows`, `macOS` (`Macintosh`), `ChromeOS` (`CrOS`), `Linux`, `Other`.
- Bot: an empty UA, or a case-insensitive substring from this list, kept in one `var botTokens`:
  - crawlers and fetchers: `bot`, `crawl`, `spider`, `slurp`, `facebookexternalhit`, `embedly`, `preview`
  - browser automation: `headless`, `phantomjs`, `selenium`, `puppeteer`, `playwright`, `lighthouse`
  - monitors: `pingdom`, `uptime`, `monitor`, `memql-site-health`
  - HTTP clients: `curl/`, `wget/`, `python-requests`, `go-http-client`, `java/`, `libwww-perl`, `httpie`, `axios/`, `node-fetch`
- `NormalizePath`:
  - Drops anything from `?` or `#`.
  - Strips a trailing `/` except at the root.
  - Truncates to 256 bytes on a rune boundary.
  - Returns `""` unless the input starts with `/`.
- `Source`, in order:
  1. `utm_source` from `r.URL.Query()`, lowercased and truncated to 64 bytes.
  2. Otherwise the `Referer` host: lowercased, `www.` removed, then folded by a closed map: `l.facebook.com` and `m.facebook.com` become `facebook.com`, `t.co` becomes `x.com`, and `google.<any tld>` or `www.google.<tld>` become `google`.
  3. `""` when that host equals `r.Host` or any of `ownHosts`.
  4. `direct` when there is no `Referer`, or it doesn't parse.

- [ ] **Step 1: Write the failing tests.**
  - `TestClassifyKnownBrowsers` (table):
    - Chrome 131 on Windows 10 is `{desktop, Chrome, Windows, false}`
    - Safari 18 on macOS is `{desktop, Safari, macOS}`
    - iPhone Safari is `{mobile, Safari, iOS}`
    - iPad is `{tablet, Safari, iPadOS}`
    - Firefox on Linux is `{desktop, Firefox, Linux}`
    - Edge on Windows is `{desktop, Edge, Windows}`
    - Samsung Internet on an Android phone is `{mobile, Samsung Internet, Android}`
    - Chrome on an Android tablet (no `Mobile`) is `{tablet, Chrome, Android}`
  - `TestClassifyBots`: Googlebot, bingbot, `curl/8.4.0`, HeadlessChrome and `""` all give `Bot: true`.
  - `TestIsPageView` (table), true for:
    - navigate+document with 200, and with 304
    - no Sec-Fetch, `Accept: text/html`, class `fallback`

    False for:
    - 404
    - `Sec-Fetch-Dest: iframe`
    - `Sec-Purpose: prefetch`, and `Purpose: prefetch`
    - POST, and HEAD
    - no Sec-Fetch with `Accept: */*` and class `asset`
  - `TestNormalizePath`:

    | Input | Output |
    | --- | --- |
    | `/about/` | `/about` |
    | `/` | `/` |
    | `/a?b=c` | `/a` |
    | `/a#x` | `/a` |
    | `about` | `""` |
    | 300-byte path | 256 bytes |
  - `TestSource`:
    - `?utm_source=Newsletter` gives `newsletter`
    - Referer `https://www.google.com/search?q=x` gives `google`
    - `https://l.facebook.com/l.php` gives `facebook.com`
    - `https://t.co/abc` gives `x.com`
    - the same host gives `""`
    - an own-hosts entry gives `""`
    - no Referer gives `direct`
    - a malformed Referer gives `direct`
  - `TestClientAddressIgnoresAForgedLeftMostEntry`: `X-Forwarded-For: 203.0.113.9, 198.51.100.7` gives `198.51.100.7`. Also:
    - invalid right-most falls back to the peer
    - two header lines use the last entry of the last line
    - `AddressKey` of `2001:db8:1:2:3:4:5:6` is `2001:db8:1:2::/64`
    - an IPv4-mapped address is unmapped

- [ ] **Step 2: Confirm they fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/edge/visit/...
  ```

  Expected: FAIL (undefined: Classify, ...).
- [ ] **Step 3: Implement the four files to those rules.** Move the address code from `access.go` and leave thin delegating functions there.
- [ ] **Step 4: Pass.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/edge/...
  ```

  Expected: PASS, including `access_test.go`.
- [ ] **Step 5: Commit** `component/edge/visit/*`, `component/edge/access.go` as "feat(edge): classify a request's visit fields in one pure package".

### Task 2: The visitor secret and the country database, on the edge

**Files:**
- Create the migration: `component/database/memory-nodes/migrations/20261108090000_edge_visitor_secret_and_geo.up.sql` and `.down.sql`
  - `edge_visitor_secret (day DATE PRIMARY KEY, secret BYTEA NOT NULL)`
  - `edge_geo_country (id SMALLINT PRIMARY KEY CHECK (id = 1), release TEXT NOT NULL, fetched_at TIMESTAMPTZ NOT NULL, data BYTEA NOT NULL)`
  - The down migration drops both.
- Create: `component/sitetraffic/visitors.go`, `countries.go`, `geodb_test_helpers_test.go`, `visitors_test.go`, `visitors_db_test.go`, `countries_db_test.go`
- Modify: `go.mod`, `go.sum`. Make `github.com/oschwald/geoip2-golang v1.13.0` a direct require, and add `github.com/maxmind/mmdbwriter` (test only).

**Interfaces:**
- Produces:
  ```go
  func visitorHash(secret []byte, siteID, addressKey, userAgent string) string
  type VisitorSecrets struct{ /* logger, provider func() *bun.DB, now func() time.Time, current atomic.Pointer[daySecret] */ }
  func NewVisitorSecrets(logger *slog.Logger, provider func() *bun.DB, now func() time.Time) *VisitorSecrets
  ```
- `VisitorSecrets` is a `common.Dependency` with ComponentName `sitetraffic.visitors`. Start rotates now, then schedules a rotation at every next UTC midnight.
  ```go
  func (v *VisitorSecrets) Visitor(siteID, addressKey, userAgent string, at time.Time) string // "" unless the held secret's day == at's UTC day
  func (v *VisitorSecrets) rotate(ctx context.Context, day time.Time) error // INSERT ... ON CONFLICT (day) DO NOTHING; SELECT; swap; DELETE WHERE day < $day
  type CountryLoader struct{ /* reader atomic.Pointer[geoip2.Reader], release string */ }
  func NewCountryLoader(logger *slog.Logger, provider func() *bun.DB) *CountryLoader
  ```
- `CountryLoader` is a Dependency with ComponentName `sitetraffic.countries`. It loads at Start and re-checks `SELECT release` hourly.
  ```go
  func (c *CountryLoader) Country(addr netip.Addr) string // "" when none loaded / not found
  func (c *CountryLoader) refresh(ctx context.Context) error
  ```
- The test helper:
  ```go
  func writeCountryDB(t *testing.T, dbType string, entries map[string]string) []byte // CIDR -> ISO code, via mmdbwriter
  ```

- [ ] **Step 1: Write the failing tests.**
  - `TestVisitorHashIsStableWithinADayAndDiffersAcrossSitesAndDays` (pure).
  - `TestNoSecretMeansNoVisitor`: an unstarted keeper returns `""`.
  - DB tests:
    - `TestTwoKeepersAgreeOnTheDaysSecret`: two keepers on one database return equal `Visitor`.
    - `TestRotationDeletesEarlierDays`.
    - `TestARequestFromYesterdayGetsNoVisitorAfterRotation`: rotate to day D, then `Visitor(..., D-1 23:59:59.999)` is `""`.
    - `TestLoaderSwapsInANewerRelease`: release `2026-10` maps `203.0.113.0/24` to `NL`; storing `2026-11`, which maps it to `DE`, and refreshing gives `DE`.
    - `TestLoaderWithNoRowRecordsNoCountry`.
- [ ] **Step 2: Confirm they fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/sitetraffic/ -run 'Visitor|Keeper|Rotation|Loader'
  ```

  Expected: FAIL.
- [ ] **Step 3: Implement.** Use `crypto/rand` for the secret, `geoip2.FromBytes` with `reader.Country(ip).Country.IsoCode`, and `time.AfterFunc` for midnight. The keeper never blocks the request path.
- [ ] **Step 4: Pass** with the database env set. The migration applies through the package's existing `TestMain` (`dbtest.EnsureSchema`).
- [ ] **Step 5: Commit** the migration, the sitetraffic files, `go.mod` and `go.sum` as "feat(sitetraffic): keep a daily visitor secret and the country database on the edge".

### Task 3: Record the visit fields

**Files:**
- Create the migration: `component/database/memory-nodes/migrations/20261108090100_edge_request_visit_fields.up.sql` and `.down.sql`
  - Up: `ALTER TABLE "edge_request"` adds `measured BOOLEAN NOT NULL DEFAULT false`, `page_view BOOLEAN NOT NULL DEFAULT false` and `bot BOOLEAN NOT NULL DEFAULT false`.
  - It also adds `path`, `source`, `country`, `device`, `browser`, `os` and `visitor`, each `TEXT NOT NULL DEFAULT ''`.
  - Down: drops them.
- Modify `component/edge/requestlog.go`:
  - `RequestRecord` gains the fields below.
  - Append `pathClassView = "view"` to the class constants and to `PathClassesForTest`.
  - `recordingWriter` gains `viewPath string`.
- Modify `component/edge/handler.go`:
  - `Options` gains `Visitors VisitorHasher` and `Countries CountryLookup`, both interfaces declared here.
  - `ServeHTTP` fills the fields.
- Modify `component/sitetraffic/record.go`:
  - `Record` mirrors the fields.
  - Append `PathClassView`.
  - Rewrite the package comment and the `Record` comment that promise nothing about visitors (§13).
- Modify `component/sitetraffic/sink.go`: `Row` columns and the `write` mapping.
- Modify `app/transport_edge.go`:
  - Construct `NewVisitorSecrets` and `NewCountryLoader` and append them to `a.Dependencies`.
  - Pass them as `Options.Visitors` / `Options.Countries`.
  - Map every new field in `edgeRequestRecorder.Record`.
- Test: `component/edge/requestlog_test.go`, `component/sitetraffic/sink_test.go`, `component/sitetraffic/sink_db_test.go` (new)

**Interfaces:**
- Consumes: Task 1 `visit.*`; Task 2 `Visitor`, `Country`.
- Produces:
  ```go
  // edge
  type VisitorHasher interface{ Visitor(siteID, addressKey, userAgent string, at time.Time) string }
  type CountryLookup interface{ Country(addr netip.Addr) string }
  type RequestRecord struct{ /* existing six */ Measured, PageView, Bot bool; Path, Source, Country, Device, Browser, OS, Visitor string }
  // sitetraffic
  type Record struct{ /* existing */ Measured, PageView, Bot bool; Path, Source, Country, Device, Browser, OS, Visitor string }
  ```
- Rule in `ServeHTTP`, after `serve`:
  ```text
  agent    = visit.Classify(ua)
  Measured = true
  Bot      = agent.Bot
  PageView = !agent.Bot && (rec.viewPath != "" || visit.IsPageView(r, pathClass, status))
  ```
- Only when `PageView` is true:
  - `Path` is `rec.viewPath` or `visit.NormalizePath(r.URL.Path)`.
  - `Source` is `""` for a ping and `visit.Source(r, site.Hostname)` otherwise.
  - `Country` is `Countries.Country(addr)`.
  - Device, Browser and OS come from `agent`.
  - `Visitor` is `Visitors.Visitor(site.ID, visit.AddressKey(addr), ua, servedAt)`.
- A nil `Visitors` or `Countries` leaves those fields empty.

- [ ] **Step 1: Write the failing tests.**
  - `TestANavigationIsRecordedAsAPageViewWithItsFields`:
    - Fake hasher and lookup; a Chrome-on-Windows UA; Referer google.
    - `X-Forwarded-For: 203.0.113.9, 198.51.100.7`.
    - The record's `Visitor` and `Country` were computed from `198.51.100.7`, with `Source` `google` and `Path` `/products/a`.
  - `TestAnAssetIsMeasuredButNotAPageView`.
  - `TestABotNavigationIsNotAPageView`: Bot is true, PageView false, Visitor `""`.
  - Keep `TestHealthProbesDoNotInflateVisitorTraffic` passing.
  - `TestSinkWritesTheVisitFields`, through the Insert seam.
  - Update `TestPathClassesMatchTheEdgesOwnSpelling`: `view` is last in both lists.
  - DB: `TestRequestRowsWithVisitFieldsRoundTrip`, inserting through `insertBatch` then selecting the columns.
- [ ] **Step 2: Confirm they fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/edge/... github.com/znasllc-io/memql/component/sitetraffic/...
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, plus `go build -tags edge ./...` from the worktree root.
- [ ] **Step 5: Commit** the listed files as "feat(edge): record page views, sources, devices, countries and a daily visitor hash".

### Task 4: The page-change ping and its insertion

**Files:**
- Create:
  - `component/edge/view.go`
  - `component/edge/view.js` (embedded with `//go:embed view.js`)
  - `component/edge/view_test.go`
- Modify `component/edge/handler.go`:
  - Dispatch both paths in `serveResolved` right after the `siteRefreshPath` branch.
  - `NewHandler` builds `viewRate: newShopperBucket(viewPingPerMinute)`.
- Modify `component/edge/site_refresh.go` and the HTML insertion at `handler.go:~508`:
  - Insert `<script src="/_memql/view.js" defer></script>` after `<head>` in every HTML document when `site.Status == "live" && !site.SystemOwned && site.ResourceParentHost == ""`, independent of the site-refresh condition.
  - When it is inserted, add `"view-v1"` to the `strongETag` parts.
- Modify `component/edge/doc.go`: add both routes to its route list.
- Modify `CLAUDE.md`: add an Allowed HTTP Exceptions row after "Deployable access codes".
- Modify `docs/public/operate/site-hosting.md`: add both routes to the edge route list (lines ~783-796).

**Interfaces:**
- Produces:
  ```go
  const viewScriptPath = "/_memql/view.js"
  const viewPingPath = "/_memql/view"
  const viewPingMaxBytes = 1024
  const viewPingPerMinute = 120
  func serveViewScript(w http.ResponseWriter, r *http.Request) string // GET/HEAD; text/javascript; charset=utf-8; Cache-Control: public, max-age=86400; returns pathClassConfig
  func (h *Handler) serveViewPing(w http.ResponseWriter, r *http.Request, site *Site) string // always 204 + no-store; returns pathClassView
  func viewPingFromSite(r *http.Request) bool // Origin host EqualFold r.Host; or Origin absent && Sec-Fetch-Site == "same-origin"
  ```
- Acceptance, all required:
  - The request is a POST.
  - `viewPingFromSite` holds.
  - The rate bucket allows `site.ID + "|" + visit.AddressKey(addr)`.
  - The body, read through `http.MaxBytesReader(viewPingMaxBytes)`, normalizes to a non-empty path.
- On acceptance, set `rec.viewPath` (`w.(*recordingWriter)`). Every refusal still answers 204.
- `view.js`, exact:
  ```js
  (() => {
    if (window.__memqlView) return;
    window.__memqlView = true;
    let last = location.pathname;
    const report = () => {
      const p = location.pathname;
      if (p === last) return;
      last = p;
      try { navigator.sendBeacon("/_memql/view", p); } catch {}
    };
    const push = history.pushState;
    history.pushState = function (...args) { const out = push.apply(this, args); report(); return out; };
    addEventListener("popstate", report);
  })();
  ```
- The CLAUDE.md row:
  - First column: **Page-change ping**.
  - Endpoints: `GET /_memql/view.js`, `POST /_memql/view`, served by the edge on a deployable's own origin.
  - Reason: owner-approved 2026-11-08, from the website statistics design. A browser beacon is a plain POST and cannot open a stream.
  - Then the facts of §6 in two sentences, linking `docs/public/operate/deployables.md#statistics-and-health`.

- [ ] **Step 1: Write the failing tests.**
  - `TestViewScriptIsServedAsCacheableJavaScript`.
  - `TestAPingFromTheSiteRecordsAPageView`: Origin `https://shop.example.com` with body `/products/a?x=1` gives 204, a record with PageView true, Path `/products/a` and PathClass `view`.
  - `TestAPingWithoutOriginNeedsSameOriginFetchSite`.
  - `TestAPingFromAnotherOriginRecordsNothing`.
  - `TestAnOversizedOrNonPathBodyRecordsNothing`: 1025 bytes, `products`, and `""`.
  - `TestPingsAreRateLimitedPerAddress`: the 121st within a minute is not a page view.
  - `TestLiveDocumentsCarryTheViewScript`: the tag follows `<head>`, and the ETag differs from the same document with no tag.
  - `TestSystemOwnedAndDraftDocumentsCarryNoViewScript`.
  - The existing CSP tests stay green.
- [ ] **Step 2: Confirm they fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/edge/...
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass.** Then run `go test -count=1 github.com/znasllc-io/memql -run 'VendorApiKey|ClaudeMd'`; CLAUDE.md is scanned.
- [ ] **Step 5: Commit** as "feat(edge): count a single-page app's in-app pages through an approved page-change ping". Push the branch at the end of Phase 1.

---

## Phase 2: website rollups and the read

### Task 5: Rollup tables and the response-time histogram

**Files:**
- Create the migration: `component/database/memory-nodes/migrations/20261108090200_edge_stats_rollups.up.sql` and `.down.sql`.
  - `edge_stats_hourly (site_id TEXT NOT NULL, hour TIMESTAMPTZ NOT NULL, visitors BIGINT, visits BIGINT, bounces BIGINT, page_views BIGINT, requests BIGINT NOT NULL, errors BIGINT NOT NULL, not_found BIGINT NOT NULL, bytes BIGINT NOT NULL, bot_requests BIGINT NOT NULL, duration_hist BIGINT[] NOT NULL, PRIMARY KEY (site_id, hour))`, indexed on `(hour)`.
  - `edge_stats_daily`: the same columns with `day DATE`, PK `(site_id, day)`, indexed on `(day)`.
  - `edge_stats_daily_top (site_id TEXT, day DATE, dimension TEXT, value TEXT, visitors BIGINT NOT NULL, visits BIGINT NOT NULL, page_views BIGINT NOT NULL, PRIMARY KEY (site_id, day, dimension, value))`, indexed on `(day)`.
  - These are plain tables (§5).
- Create: `component/sitetraffic/histogram.go`, `histogram_test.go`, `histogram_db_test.go`

**Interfaces:**
- Produces:
  ```go
  const histogramBuckets = 64
  const histogramBaseNs = 500_000
  func histogramBucket(durationNs int64) int // clamp(floor(4*log2(max(ns,1)/500000)), 0, 63)
  func histogramBucketSQL(column string) string // LEAST(63, GREATEST(0, floor(4 * ln(GREATEST(<col>,1)::float8 / 500000) / ln(2))))::int
  func mergeHistograms(hs ...[]int64) []int64
  func percentileMs(h []int64, q float64) (float64, bool) // geometric interpolation inside the bucket; false when empty
  ```

- [ ] **Step 1: Write the failing tests.**
  - `TestBucketBoundaries`:

    | Duration | Bucket |
    | --- | --- |
    | 0 | 0 |
    | 0.5 ms | 0 |
    | 1 ms | 4 |
    | 2 ms | 8 |
    | 30 s | 63 |
  - `TestPercentileOfKnownDurations`: 900 at 10 ms and 100 at 500 ms give p50 within 10% of 10 ms and p95 within 10% of 500 ms.
  - `TestMergeIsElementwise`.
  - DB: `TestSQLBucketMatchesGo`, run over 20 durations.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/sitetraffic/ -run 'Bucket|Percentile|Merge|SQLBucket'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, with the migration applied by `TestMain`.
- [ ] **Step 5: Commit** as "feat(sitetraffic): rollup tables and a response-time histogram".

### Task 6: The rollup job

**Files:**
- Create: `component/sitetraffic/rollup.go`, `rollup_db_test.go`
- Modify `component/sitetraffic/integration.go`: capability `rollup`, handler `handleRollup`.
- Modify `dsl/platform/builtins.memql`: add, with an `@description`:
  ```memql
  @executor("integration.siteTraffic.rollup")
  builtin siteStatisticsRollup { }
  ```
- Modify `dsl/platform/automations.memql`: add, with a `///` doc on the cron leader:
  ```memql
  @trigger(schedule="0 */5 * * * *")
  automation siteStatisticsRollupSweep { roll := builtin siteStatisticsRollup() }
  ```
- Modify `component/auth/maintenance_actor.go`: add `"siteStatisticsRollupSweep"` with its reason.
- Modify `component/automations/maintenance_actor_gate_test.go`: add it to `want`.
- Modify `component/automations/strict_automation_boot_test.go`: `shippedAutomationCount` +1.

**Interfaces:**
- Consumes: Task 5 tables and histogram.
- Produces:
  ```go
  type RollupReport struct{ Hours, Days int; Backfilled bool; Deleted int64 }
  func Rollup(ctx context.Context, db *bun.DB, now time.Time) (RollupReport, error)
  ```
- What runs:
  - Recompute every hour of today, and today itself. Do the same for yesterday's hours and yesterday while `now < today 02:00 UTC`. Every hour, because a visit is segmented over the whole day's page views (spec D17).
  - When `edge_stats_daily` is empty, backfill every day from `edge_request`'s oldest day, and every hour of the last 30 days.
  - Each range is one transaction: `SET LOCAL statement_timeout = '60s'`, delete the range's rows (and that day's top rows), insert the aggregates.
  - Then delete hourly rows older than `now-30d`, and daily and top rows older than `today-400d`.
- Aggregates per `(site_id, bucket)`. Hour is `date_trunc('hour', served_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`; day is `(served_at AT TIME ZONE 'UTC')::date`.
  ```sql
  visitors   = CASE WHEN bool_or(measured) THEN count(DISTINCT visitor) FILTER (WHERE page_view AND visitor <> '') END
  page_views = CASE WHEN bool_or(measured) THEN count(*) FILTER (WHERE page_view) END
  requests = count(*); errors = count(*) FILTER (WHERE status >= 500)
  not_found = count(*) FILTER (WHERE status >= 400 AND status < 500)
  bytes = coalesce(sum(bytes),0); bot_requests = count(*) FILTER (WHERE bot)
  duration_hist = 64-element array of counts per histogramBucketSQL(duration_ns)
  visits, bounces = from the day's visits below, counted in the bucket of each visit's start; NULL when the bucket has no measured row
  ```
- Visits are built per site and UTC day from `page_view AND visitor <> ''` rows ordered by `(visitor, served_at)`:
  - A row starts a new visit when it has no previous row for that visitor that day, or the gap from the previous row is over 30 minutes. Use `lag(served_at) OVER (PARTITION BY site_id, visitor ORDER BY served_at)`, then a running `sum` of the start flags to number the visits.
  - A visit's start is its first row's `served_at`. Its entry page and entry source come from its first row (the first row whose `source <> ''` for the source dimension). Its country, device, browser and OS come from its first row.
  - A visit is a bounce when it has exactly one row.
- Top rows gain `visits`: a visit counts once under its entry page, its entry source, and its first row's country, device, browser and OS.
- Top rows come from `page_view` rows only:
  - Dimensions: `page` = path; `source` = source, with `''` excluded; `country`, `device`, `browser` and `os`, with `''` stored as `(unknown)`.
  - `visitors = count(DISTINCT visitor) FILTER (WHERE visitor <> '')`, `page_views = count(*)`.
  - Rank by `visitors DESC, page_views DESC, value ASC`. Keep 100; fold the rest into `(other)`, summing both.
- `handleRollup` refuses unless the actor is `auth.MaintenanceActor("siteStatisticsRollupSweep")`. Use `component/sitehealth/integration.go:141` as the pattern: match the `UserId` and require `IsClusterOwner()`. It answers one node with the report.

- [ ] **Step 1: Write the failing DB tests.**
  - `TestRollupCountsVisitorsPageViewsAndHealthPerHourAndDay`.
  - `TestRerunningTheRollupChangesNothing`.
  - `TestRequestsWithoutVisitFieldsRollUpToNullVisitors`.
  - `TestTopListsKeepAHundredAndFoldTheRest`: 150 paths give 100 rows plus `(other)`.
  - `TestTheFirstRunBackfillsEveryDayStillLogged`.
  - `TestYesterdayIsRecomputedUntilTwoAfterMidnight`: a late row for 23:59 yesterday counts at 00:30 and not at 02:30.
  - `TestRowsPastRetentionAreDeleted`.
  - `TestTheBuiltinRefusesAnyoneButItsMaintenancePrincipal`.
  - `TestAGapOverThirtyMinutesStartsANewVisit`: views at 10:00, 10:20, 10:55 and 11:30 give two visits, both started in hour 10.
  - `TestAOnePageVisitIsABounce`.
  - `TestAVisitCountsInTheHourItStarted`: views at 10:50 and 11:10 give a visit in hour 10 and none in hour 11.
  - `TestAVisitAcrossMidnightSplitsInTwo`.
  - `TestTopRowsCountAVisitUnderItsEntryPageAndSource`.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/sitetraffic/ -run 'Rollup|Visit|Bounce|TopRows'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass, and run the boot gates.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/automations/ -run 'TestMaintenanceAutomationsAreArgued|Strict'
  ```

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/memql/ -run 'BuiltinStepArgs|IntegrationExecutor'
  ```

  If the 4927 gate flags the empty call, add `siteStatisticsRollup` to its admit list beside `logsSweep`.
- [ ] **Step 5: Commit** as "feat(sitetraffic): fold requests into hourly and daily statistics every five minutes".

### Task 7: The monthly country refresh

**Files:**
- Create: `component/sitetraffic/countryrefresh.go`, `countryrefresh_db_test.go`
- Modify `component/sitetraffic/integration.go`:
  - Capability `countryRefresh`.
  - `Integration` gains `countryBaseURL` (default `https://download.db-ip.com/free/`) and `httpClient` (2-minute timeout), both settable by tests.
- Modify `dsl/platform/builtins.memql`: `builtin geoCountryRefresh { }`.
- Modify `dsl/platform/automations.memql`:
  ```memql
  @trigger(schedule="0 10 4 * * *")
  automation geoCountryRefreshSweep { refresh := builtin geoCountryRefresh() }
  ```
- Modify the maintenance list, `want` and `shippedAutomationCount` (+1), as in Task 6.

**Interfaces:**
- Produces:
  ```go
  type CountryRefreshReport struct{ Release string; Downloaded bool; Reason string }
  func refreshCountries(ctx context.Context, db *bun.DB, client *http.Client, baseURL string, now time.Time) (CountryRefreshReport, error)
  ```
- Steps the refresh takes:
  1. Skip when the stored `release` equals `now`'s `YYYY-MM`.
  2. Otherwise GET `baseURL + "dbip-country-lite-" + release + ".mmdb.gz"`, and on a 404 retry the previous month.
  3. Limit the compressed body to 32 MiB and the gunzipped one to 64 MiB.
  4. Require `geoip2.FromBytes` to succeed and `Metadata.DatabaseType` to contain `Country`.
  5. Upsert row `id = 1`.
  6. Any failure keeps the stored row, returns the error, and logs at Warn.
- The handler requires the maintenance principal `geoCountryRefreshSweep`.

- [ ] **Step 1: Write the failing tests** against an `httptest.Server` serving `writeCountryDB` gzipped.
  - `TestRefreshDownloadsTheCurrentMonth`.
  - `TestRefreshFallsBackToThePreviousMonthOn404`.
  - `TestRefreshRefusesANonCountryDatabase`: the database type is `GeoLite2-City`.
  - `TestRefreshRefusesAnOversizedBody`.
  - `TestAFailedDownloadKeepsTheStoredRelease`.
  - `TestRefreshIsANoOpWhenTheCurrentMonthIsStored`: the server is never called.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/sitetraffic/ -run Refresh
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, plus the Task 6 boot gates.
- [ ] **Step 5: Commit** as "feat(sitetraffic): download DB-IP's country database once a month".

### Task 8: Periods and the `siteStatistics` read

**Files:**
- Create: `core/statsperiod/statsperiod.go`, `statsperiod_test.go`
- Create: `component/sitetraffic/statistics.go`, `statistics_db_test.go`
- Modify `component/sitetraffic/integration.go`: capability `statistics`. Its payload is one node with ID `statistics` and Concept `integration:siteTraffic:statistics`.
- Modify `component/sitetraffic/reader.go`: add `readableSitesForAccounts`.
- Modify `dsl/platform/builtins.memql`:
  ```memql
  @sdk
  @executor("integration.siteTraffic.statistics")
  @args(profile="object")
  builtin siteStatistics { siteIds []string  accountIds []string  period string! }
  ```
  Give each field a `@description`.
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts` (`make sdk-gen`).

**Interfaces:**
- Produces:
  ```go
  // core/statsperiod
  type Period string // "24h" "7d" "30d" "90d" "12m"
  type Bounds struct{ Start, End, PrevStart, PrevEnd time.Time; Bucket string; Buckets int; HasPrevious bool } // Bucket: hour|day|month
  func Parse(s string, allowed ...Period) (Period, error)
  func For(p Period, now time.Time) Bounds
  ```
- `For` rules:
  - `24h`: `End` is the next whole hour after `now`. `Start = End - 24h` and `Prev* = [Start - 24h, Start)`.
  - `7d`/`30d`/`90d`: `End` is the next UTC midnight. `Start = End - N days` and `Prev* = [Start - N days, Start)`.
  - `12m`: `End` is the first of next month and `Start` the first of the month 12 months before the current one, giving 13 buckets. `HasPrevious` is false and the `Prev*` times are zero (spec D4).
  ```go
  // sitetraffic
  type StatisticsQuery struct{ SiteIds, AccountIds []string; Period statsperiod.Period; Now time.Time }
  type Totals struct{ Visitors, Visits, Bounces, PageViews, Requests, Errors, NotFound, Bytes, BotRequests *int64; ViewsPerVisit, BounceRate, MedianMs, P95Ms *float64 }
  type Point struct{ At time.Time; Visitors, Visits, Bounces, PageViews, Requests, Errors, NotFound *int64; P95Ms *float64 }
  type TopEntry struct{ Value string; Visitors, Visits, PageViews int64 }
  type SiteTotals struct{ SiteId string; Visitors, Visits, PageViews *int64 }
  type Statistics struct{ Period statsperiod.Period; Bucket string; Start, End, PrevStart, PrevEnd time.Time; MeasuredSince *time.Time; Totals, Previous Totals; Series, PreviousSeries []Point; Top map[string][]TopEntry; Sites []SiteTotals }
  func (r *Reader) Statistics(ctx context.Context, q StatisticsQuery) (Statistics, error)
  func (s Statistics) Payload() map[string]any // nil -> JSON null; times RFC3339; measuredSince "YYYY-MM-DD"; top keys page,source,country,device,browser,os; every other key is the lowerCamel field name (visits, bounces, pageViews, viewsPerVisit, bounceRate, notFound, botRequests, medianMs, p95Ms, previousSeries, siteId), which Task 15 parses; previous and previousSeries are null when !HasPrevious
  func (r *Reader) readableSitesForAccounts(ctx context.Context, accountIds []string) ([]string, error)
  ```
- `readableSitesForAccounts` runs `sitesAll` under the caller, drops system-owned rows, and keeps rows whose bare `accountId` is in the list when one is given.
- Narrowing: non-empty `SiteIds` go through `readableSites` (more than 200 is refused); otherwise `readableSitesForAccounts`.
- Visitor totals:
  - `24h`: `count(DISTINCT visitor)` over raw `edge_request` rows in range.
  - Other periods: the sum of daily visitors.
  - `null` when no measured row is in range.
- Visits and bounces: the sum of the buckets' visits and bounces. Each visit is counted once, in the bucket it started.
  - `ViewsPerVisit = PageViews / Visits` and `BounceRate = Bounces / Visits`, both `null` when there are no visits.
- Series:
  - One point per bucket.
  - When the range has any row, a bucket with none is 0 for requests. Its visitors and page views are 0 too, unless the bucket is before `MeasuredSince`, when they are `null`.
  - A range with no rows is all `null`.
- Top lists: `24h` computes them from raw rows; other periods sum `edge_stats_daily_top`. Keep 50 per dimension plus an `(other)` remainder.
  - The 24h visits per value come from the same visit segmentation as Task 6. Make Task 6's segmentation SQL one builder in `rollup.go` that both call, never a second copy.
- `MedianMs` and `P95Ms` come from the merged histograms.
- `MeasuredSince` is `min(day)` with non-null visitors among the narrowed sites.

- [ ] **Step 1: Write the failing tests.**
  - `TestBoundsForEveryPeriod` (pure). At `2026-11-08T10:17Z`:

    | Period | Start | End | Bucket |
    | --- | --- | --- | --- |
    | `24h` | `2026-11-07T11:00Z` | `2026-11-08T11:00Z` | hour |
    | `7d` | `2026-11-02` | `2026-11-09` | day |
    | `12m` | `2025-11-01` | `2026-12-01` | month (13 buckets, `HasPrevious` false) |

    Also check each previous range.
  - DB tests, with a real engine as in `reader_db_test.go`:
    - `TestStatisticsForOneSiteOverSevenDays`.
    - `TestStatisticsForTwentyFourHoursCountDistinctVisitorsFromRequests`.
    - `TestAPeriodBeforeVisitorCountingIsNullNotZero`.
    - `TestTwelveMonthsSumsDaysIntoMonths`: 13 points, with no `previous` and no `previousSeries`.
    - `TestVisitsBouncesAndTheirRates`: views per visit and bounce rate come from the summed visits, bounces and page views.
    - `TestHealthPercentilesComeFromMergedHistograms`.
    - `TestAClientMemberGetsNothingForAnotherClientsSiteEvenById`: a member of account A asks for B's site by id, then by `accountIds: [B]`. Both answer an empty `Sites` and nil totals.
    - `TestStaffGetEverySiteWhenNoneAreNamed`.
    - `TestAccountIdsKeepTheReadToThoseClients`.
    - `TestSystemOwnedSitesAreNeverInTheAnswer`.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/core/statsperiod/... github.com/znasllc-io/memql/component/sitetraffic/ -run 'Bounds|Statistics|ClientMember|Staff|AccountIds|SystemOwned'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, then `make sdk-gen && make sdk-gen-check` and `bash scripts/os/build.sh deps`.
- [ ] **Step 5: Commit** as "feat(sitetraffic): the siteStatistics read, narrowed to the sites a caller may see". Push the branch at the end of Phase 2.

---

## Phase 3: email facts and the read

### Task 9: The email facts rollup

**Files:**
- Create the migration: `component/database/memory-nodes/migrations/20261108090300_email_stat_facts.up.sql` and `.down.sql`.
  - `email_stat_fact (kind TEXT NOT NULL, source_id TEXT NOT NULL, account_id TEXT NOT NULL DEFAULT '', campaign_id TEXT NOT NULL DEFAULT '', rule_id TEXT NOT NULL DEFAULT '', journey_id TEXT NOT NULL DEFAULT '', audience_id TEXT NOT NULL DEFAULT '', day DATE NOT NULL, PRIMARY KEY (kind, source_id))`, indexed on `(account_id, day)` and on `(day)`.
  - `email_stat_watermark (source TEXT PRIMARY KEY, through TIMESTAMPTZ NOT NULL)`.
- Create the package `component/emailstats`: `doc.go`, `plugin.go` (`memql.RegisterPlugin("emailStats", ...)` with `pctx.Engine`, `pctx.BunDB`, `pctx.Logger`), `facts.go`, `rollup.go`, `main_dbschema_test.go`, `facts_test.go`, `rollup_db_test.go`.
- Modify `app/plugins_core.go`: add `_ "github.com/znasllc-io/memql/component/emailstats"`.
- Modify `dsl/campaigns/queries.memql`: three `@actor @serverOnly @cache(0)` queries, `deliveriesWrittenSince`, `engagementEventsWrittenSince` and `consentEventsWrittenSince`. Each:
  - takes `args { since datetime! }`
  - filters `row => actor.isClusterOwner == true && row.createdAt >= args.since`
  - uses `sort "row.createdAt", "asc"`, `paginate 500`
  - and shapes `deliveryFull`, `engagementEventFull` or `consentEventFull`
- Also two `@actor @serverOnly @cache(0)` lookups for attribution (spec D18):
  - `journeyEnrollmentsByIds { args { ids []string! } }` on `journeyEnrollment`, shaped by a new `enrollmentJourneyRef { row.id journeyId }`.
  - `recipientAudiencesByIds { args { ids []string! } }` on `recipient`, shaped by a new `recipientAudienceRef { row.id audienceId }`.
  - Each filters `row => actor.isClusterOwner == true && row.id in args.ids` and uses `sort "row.createdAt", "asc"`, `paginate 500`.
  - Compare `row.id` the way existing by-id queries in `dsl/campaigns/queries.memql` do: bare ids resolve inbound.
  - The two shapes go in `dsl/campaigns/shapes.memql`.
- Modify `dsl/campaigns/builtins.memql`:
  ```memql
  @executor("integration.emailStats.rollup")
  builtin emailStatisticsRollup { }
  ```
- Modify `dsl/campaigns/automations.memql`:
  ```memql
  @trigger(schedule="30 */5 * * * *")
  automation emailStatisticsRollupSweep { roll := builtin emailStatisticsRollup() }
  ```
- Modify the gates:
  - the maintenance list, `want`, and `shippedAutomationCount` (+1)
  - `test/dslconformance/server_only_parsed_test.go` `want` (the five queries)
  - the root `call_origin_conformance_test.go` allowlist (`component/emailstats`)
  - `module_taxonomy_test.go` (`"emailStats": kindComponent`, reason "reads the cluster's own rows and tables")
  - `scripts/ci/db-gated-packages.sh` `DB_GATED_TREES` (`"component/emailstats"`)
  - `scripts/ci/db-test-budgets.yaml`: one line in the file's format, 300s, serial and uncached

**Interfaces:**
- Produces:
  ```go
  type Fact struct{ Kind, SourceID, AccountID, CampaignID, RuleID, JourneyID, AudienceID string; Day time.Time }
  func journeyEnrollmentOf(requestID string) (enrollmentID string, ok bool) // "journey-" + 64 lowercase hex + "-" + step
  func attribute(ctx context.Context, engine Engine, facts []Fact, enrollmentOf, audienceOf map[string]string) error // fills JourneyID / AudienceID from two batched lookups per page
  func deliveryFacts(row map[string]any) []Fact
  func engagementFacts(row map[string]any) []Fact
  func consentFacts(row map[string]any) []Fact
  type Engine interface{ Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) }
  type RollupReport struct{ Rows map[string]int; Facts int; Deleted int64 }
  func runRollup(ctx context.Context, engine Engine, db *bun.DB, now time.Time) (RollupReport, error)
  ```
- Fact rules (§5):
  - **Deliveries:**
    - `status sent` with `sentAt` gives `sent` on the `sentAt` day.
    - `failed` and `skipped` give their kind on the version's `row.createdAt` day.
    - A skipped delivery whose `skipReason` is `hard_bounce`, `complaint`, `manual`, `bounced` or `complained` also gives `suppressed`. That is `campaignStats`' suppressed set (`component/campaigns/stats.go` `suppressedSkipReasons`); reference it, don't copy it.
    - Every delivery fact carries `rule_id` from `emailRuleId`. When `requestId` matches `journeyEnrollmentOf`, it carries `journey_id` from the enrollment's `journeyId`.
    - Provider status `delivered|expanded` gives `delivered`, and `bounced|suppressed|quarantined|filtered-spam|failed` gives `undelivered`, both dated by `providerAt`, else `row.createdAt`.
  - **Engagement:** `open|click` keyed by the event id, plus `open_unique|click_unique` keyed by `deliveryId`, both dated by `occurredAt`.
  - **Consent:** `withdraw` gives `unsubscribe`, `bounce` and `complaint` keep their names, `grant` gives `subscribe`, and `suppress` gives nothing.
    - `subscribe` and `unsubscribe` carry `audience_id` from the event's recipient's `audienceId`. An event with no `recipientId` keeps `audience_id` empty and counts in its organization's totals only.
    - The enrollment and recipient lookups run once per page, in batches of at most 500 ids, under the same internal-origin context as the walk.
  - Every id is `memql.BareShortId`.
- The walk, for each source:
  1. Set `since = watermark - 5m`, or `now - 400d` when there is no watermark.
  2. Use `ctx = auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx))`.
  3. Page with `memql.ContextWithCursor` / `memql.ResultPageCursor`, at most 40 pages per run, inserting each page's facts in one `INSERT ... ON CONFLICT (kind, source_id) DO NOTHING`.
  4. Advance the watermark to the page's newest `createdAt` only after its facts are written. A cursor that repeats is an error.
  5. Finally, delete facts with `day < today - 400d`.
- The handler requires the maintenance principal `emailStatisticsRollupSweep`.

- [ ] **Step 1: Write the failing tests.**
  - Pure:
    - `TestASentDeliveryIsASentFactOnItsSendDay`.
    - `TestProviderStatusesSplitIntoDeliveredAndUndelivered`.
    - `TestEngagementGivesATotalAndAUniqueFact`.
    - `TestConsentKindsMapToTheirFacts`.
    - `TestIdsAreBare`.
    - `TestASuppressedSkipIsASuppressedFact`.
    - `TestJourneyEnrollmentOfParsesOnlyAJourneyRequestId`: `journey-<64 hex>-welcome` yields the enrollment; `journey-short-x`, `rule-...` and `""` do not.
  - DB, seeding through the campaigns mutations (`recordCampaignDelivery`, `recordEngagementEvent`, `recordConsentWithdraw`, ...) under internal origin and an owner actor:
    - `TestRollupRecordsFactsFromTheGraph`.
    - `TestRerunsCountNothingTwice`.
    - `TestTheWatermarkOverlapCatchesALateWrite`.
    - `TestTheFirstRunWalksAtMostFortyPagesPerSource`.
    - `TestARuleSendIsAttributedToItsRule`.
    - `TestAJourneySendIsAttributedToItsJourney`, through `enrollJourneyRecipient` and a delivery with `requestId` `journey-<enrollment>-<step>`.
    - `TestASubscribeIsAttributedToItsRecipientsAudience`.
    - `TestAConsentEventWithNoRecipientCountsOnlyInTotals`.
    - `TestTheBuiltinRefusesAnyoneButItsMaintenancePrincipal`.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/emailstats/...
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass, then run the gates.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/test/dslconformance/...
  ```

  ```bash
  go test -count=1 github.com/znasllc-io/memql -run 'TestOnlyAllowlistedPackagesStampInternalOrigin|TestEveryRegisteredPluginIsClassified'
  ```

  Also run the Task 6 boot gates.
- [ ] **Step 5: Commit** as "feat(emailstats): fold campaign deliveries, engagement and consent into idempotent daily facts".

### Task 10: The `emailStatistics` read

**Files:**
- Create: `component/emailstats/statistics.go`, `statistics_db_test.go`
- Modify `component/emailstats/plugin.go`: capability `statistics`. Its payload is one node with ID `statistics` and Concept `integration:emailStats:statistics`.
- Modify `dsl/campaigns/builtins.memql`:
  ```memql
  @sdk
  @executor("integration.emailStats.statistics")
  @args(profile="object")
  builtin emailStatistics { accountIds []string  period string! }
  ```
- Regenerate the SDK.

**Interfaces:**
- Consumes: Task 8 `statsperiod`; Task 9 facts.
- Produces:
  ```go
  type GrantResolver interface{ ResolveAccessGrant(ctx context.Context) memql.AccessGrant } // asserted on pctx.Engine; absent -> every read answers nothing
  func allowedAccounts(grant memql.AccessGrant, requested []string) (ids []string, everything bool) // everything: staff with nothing requested (includes account_id '')
  type EmailTotals struct{ Sent, Delivered, Undelivered, Failed, Skipped, Suppressed, Opens, Clicks, UniqueOpens, UniqueClicks, Unsubscribes, Bounces, Complaints, Subscribes *int64; OpenRate, ClickRate *float64 }
  type EmailPoint struct{ At time.Time; Sent, UniqueOpens, UniqueClicks, Unsubscribes, Subscribes, Bounces, Complaints, Failed, Suppressed *int64; OpenRate, ClickRate *float64 }
  type CampaignFigures struct{ ID string; Sent, UniqueOpens, UniqueClicks int64; OpenRate, ClickRate *float64 } // also the Accounts rows (ID = account)
  type AutomationFigures struct{ ID, Kind string; Sent, Failed, Suppressed int64 } // Kind: "rule" | "journey"
  type AudienceFigures struct{ ID string; Subscribes, Unsubscribes, Net int64 }
  type EmailStatistics struct{ Period statsperiod.Period; Bucket string; Start, End, PrevStart, PrevEnd time.Time; HasPrevious bool; MeasuredSince *time.Time; Totals, Previous EmailTotals; Series, PreviousSeries []EmailPoint; Campaigns []CampaignFigures; Automations []AutomationFigures; Audiences []AudienceFigures; Accounts []CampaignFigures }
  func readStatistics(ctx context.Context, db *bun.DB, grant memql.AccessGrant, requested []string, p statsperiod.Period, now time.Time) (EmailStatistics, error)
  func (s EmailStatistics) Payload() map[string]any // nil -> JSON null; keys are the lowerCamel field names (uniqueOpens, openRate, suppressed, previousSeries, campaigns, automations, audiences, accounts, id, kind, net), which Task 17 parses; previous and previousSeries are null when !HasPrevious
  ```
- Read rules:
  - `allowedAccounts`: for `EveryAccount`, the requested ids, or everything when none are requested. Otherwise the requested ids that are in `AccountIDs`, or all of `AccountIDs` when none are requested.
  - An empty result answers with every figure `null`.
  - `MeasuredSince` is the global `min(day)` of `email_stat_fact`. Figures for buckets before it are `null`, and with no facts at all every figure is `null`.
  - A rate is the number of campaign `sent` facts in range (`campaign_id <> ''`) whose `source_id` has an `open_unique` (or `click_unique`) fact on any day, divided by the campaign `sent` facts in range. It is `null` when no campaign mail was sent (spec D18).
  - Series rates use send-day cohorts.
  - `Campaigns` groups by `campaign_id <> ''`, top 50 ordered by unique opens desc, then unique clicks desc.
  - `Automations` has one row per `rule_id <> ''` (kind `rule`) and per `journey_id <> ''` (kind `journey`), with sent, failed and suppressed.
  - `Audiences` has one row per `audience_id <> ''` with subscribes, unsubscribes and `Net = Subscribes - Unsubscribes`.
  - `Accounts` appears only when more than one organization is in scope.
  - Periods: `7d | 30d | 90d | 12m`. `12m` has no previous period (13 monthly buckets).

- [ ] **Step 1: Write the failing DB tests**, inserting facts directly.
  - `TestTotalsAndCohortRates`: 100 sent, 42 opened, 3 clicked give 0.42 and 0.03.
  - `TestRatesCountCampaignMailOnly`: 100 campaign sends with 42 opened, plus 50 rule sends, still give 0.42.
  - `TestSeriesByDayAndByMonth`: the series carries bounces, complaints, failed and suppressed; `12m` has 13 points and no previous.
  - `TestAutomationsListRulesAndJourneysWithFailedAndSuppressed`.
  - `TestAudiencesCarryTheirNetChange`.
  - `TestCampaignsRankByUniqueOpensThenUniqueClicks`.
  - `TestAMemberGetsOnlyTheirOrganization`, with a fake `GrantResolver` returning `[A]`: requesting `[B]` gives nil totals; requesting `[]` gives A only.
  - `TestStaffGetEveryOrganizationAndTheUnattributed`.
  - `TestAccountsListOnlyWhenMoreThanOneIsInScope`.
  - `TestNoFactsMeansNothingMeasured`.
  - `var _ GrantResolver = (*memql.MemQLEngine)(nil)`.
- [ ] **Step 2: Fail.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql/component/emailstats/ -run 'Totals|Series|Member|Staff|Accounts|NoFacts'
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, then `make sdk-gen && make sdk-gen-check` and `bash scripts/os/build.sh deps`.
- [ ] **Step 5: Commit** as "feat(emailstats): the emailStatistics read, narrowed to a caller's organizations". Push the branch at the end of Phase 3.

---

## Phase 4: shared statistics pieces (OS)

### Task 11: Periods, scales and change

**Files:**
- Create: `clients/os/src/kit/statistics/period.ts`, `scale.ts`
- Modify: `clients/os/src/kit/measure.ts`
- Test: `clients/os/test/kit/statistics/period.test.ts`, `scale.test.ts`, `change.test.ts`

**Interfaces:**
- Produces:
  ```ts
  export type Period = "24h" | "7d" | "30d" | "90d" | "12m";
  export type Bucket = "hour" | "day" | "month";
  export const SITE_PERIODS: readonly Period[];   // ["24h","7d","30d","90d","12m"]
  export const EMAIL_PERIODS: readonly Period[];  // ["7d","30d","90d","12m"]
  export function periodLabel(p: Period): string; // "Last 24 hours" | "Last 7 days" | "Last 30 days" | "Last 90 days" | "Last 12 months"
  export function isPeriod(v: unknown, allowed: readonly Period[]): v is Period;
  export function niceMax(max: number): number;          // smallest {1,2,2.5,5}x10^k >= max; max <= 0 -> 1
  export function valueTicks(max: number): number[];     // [0, niceMax(max)/2, niceMax(max)]
  export function timeTickIndexes(count: number, maxLabels: number): number[]; // evenly spaced, always first and last
  export function bucketLabel(at: string, bucket: Bucket): string; // UTC, en-US: "14:00" | "Nov 8" | "Nov 2026"
  // measure.ts
  export type Change = { kind: "none" } | { kind: "new" } | { kind: "ratio"; ratio: number };
  export function changeOf(current: Figure, previous: Figure | undefined): Change;
  export function changeLabel(change: Change): string; // "+12%", "−8%" (U+2212), "No change" (|ratio| < 0.005), "New", ""
  ```
- `changeOf`: either side absent gives `none`; previous 0 and current above 0 gives `new`; otherwise `(cur - prev) / prev`.

- [ ] **Step 1: Write the failing tests** with the values above, including:
  - `changeOf(figureOf(5), absent("unmeasured"))` is `{kind:"none"}`.
  - `niceMax(7)` is 10 and `niceMax(230)` is 250.
  - `bucketLabel("2026-11-08T14:00:00Z","hour")` is `"14:00"`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/kit/statistics
  ```
- [ ] **Step 3: Implement.** **Step 4: Pass.**
- [ ] **Step 5: Commit** as "feat(os): statistics periods, scales and change".

### Task 12: `TimeChart`

**Files:**
- Create: `clients/os/src/kit/statistics/TimeChart.tsx`, `statistics.css` (imported by the component, following the `kit/details.css` pattern)
- Test: `clients/os/test/kit/statistics/timeChart.test.tsx`

**Interfaces:**
- Consumes: Task 11.
- Produces:
  ```ts
  export interface ChartPoint { at: string; value: number | null }
  export function TimeChart(props: { label: string; bucket: Bucket; points: readonly ChartPoint[]; previous?: readonly ChartPoint[]; format: (v: number) => string; height?: number }): JSX.Element
  ```
- Drawing:
  - A `figure` holds an SVG with `viewBox="0 0 600 {height=160}"` and `preserveAspectRatio="none"`; the axis labels are HTML overlaid so the text stays crisp.
  - The current series is a `path[data-series="current"]` stroked in `--os-accent`, over an area filled with `--os-accent-soft`.
  - The previous series is a `path[data-series="previous"]`, dashed in `--os-muted`.
  - A `null` starts a new `M` segment.
  - `valueTicks` labels the value axis through `format`.
  - The time axis labels the `timeTickIndexes(points.length, floor(width / 72))` positions; the width comes from `useWide`/ResizeObserver.
- Interaction:
  - The plot is focusable (`tabIndex 0`, `aria-label={label}`).
  - Hover and ArrowLeft/ArrowRight move a readout (`role="status"`): `bucketLabel`, then the value or "Not measured", then the previous value.
- The `figcaption.os-sr-only` reads: `"{label}: {n} points from {first} to {last}; highest {max} on {date}; {k} not measured."`
- No transitions under `prefers-reduced-motion`.

- [ ] **Step 1: Write the failing tests.**
  - `TestNullPointsAreGapsNotZeroes`: the current path's `d` has two `M` commands.
  - `TestArrowKeysMoveTheReadout`.
  - `TestThePreviousPeriodIsDashed`.
  - `TestTheSummaryNamesTheHighestAndTheUnmeasured`.
  - `TestAnAllNullSeriesDrawsNoLine`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/kit/statistics/timeChart.test.tsx
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass.**
- [ ] **Step 5: Commit** as "feat(os): one time-series chart for every statistics page".

### Task 13: `StatFigures`, `RankedList`, `PeriodControl`, and `Overview.figures`

**Files:**
- Create: `clients/os/src/kit/statistics/StatFigures.tsx`, `RankedList.tsx`, `PeriodControl.tsx`, `index.ts`
- Modify `clients/os/src/kit/Overview.tsx`: an optional `figures?: ReactNode` is rendered in place of the metrics `dl` when given.
- Test: `clients/os/test/kit/statistics/statFigures.test.tsx`, `rankedList.test.tsx`, `periodControl.test.tsx`

**Interfaces:**
- Produces:
  ```ts
  export interface StatFigure { id: string; label: string; figure: Figure; previous?: Figure; format?: (v: number) => string; chartable?: boolean; loading?: boolean }
  export function StatFigures(props: { label: string; figures: readonly StatFigure[]; selected?: string; onSelect?: (id: string) => void }): JSX.Element
  export interface RankedRow { key: string; label: string; value: number; secondary?: string; onOpen?: () => void }
  export function RankedList(props: { title: string; rows: readonly RankedRow[]; limit?: number; format?: (v: number) => string; onShowAll?: () => void; showAllLabel?: string; footer?: ReactNode; loading?: boolean; empty?: string }): JSX.Element
  export function PeriodControl(props: { id: string; value: Period; periods: readonly Period[]; onChange: (p: Period) => void }): JSX.Element
  ```
- `StatFigures`:
  - A `role="radiogroup"` holds the chartable figures as `button role="radio"` with roving tabindex and arrow keys. Non-chartable figures are plain blocks.
  - Each shows the label, `Measure` and `changeLabel` in muted text; loading shows `InlineSkeleton`.
  - The grid follows DESIGN.md "App overviews": four in a row then two; five in a row then three then two.
- `RankedList`:
  - A `section aria-label={title}` with a `Subhead`, then an `ol` showing `limit ?? 8` rows.
  - Each row shows the label (truncated, with the full label as `title`), a right-aligned value and an `aria-hidden` bar sized as a percentage of the maximum.
  - A row with `onOpen` is a button.
  - `onShowAll` adds a quiet `Button` with `showAllLabel` when `rows.length > limit`.
  - Empty shows `Caption`; loading shows `RecordListSkeleton`.
- `PeriodControl` is kit `Select` with `tone="quiet"`, label "Period", and options from `periodLabel`.

- [ ] **Step 1: Write the failing tests.**
  - `TestOnlyChartableFiguresAreRadios`.
  - `TestArrowKeysChangeTheSelectedFigure`.
  - `TestChangeReadsAgainstThePreviousPeriod`: shows "+12%".
  - `TestAnAbsentPreviousShowsNoChange`.
  - `TestRankedListShowsEightAndOffersTheRest`.
  - `TestBarsAreProportionalToTheLargestValue`.
  - `TestPeriodControlOffersOnlyTheGivenPeriods`.
  - `TestOverviewDrawsFiguresInPlaceOfMetrics`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/kit/statistics test/kit/overview
  ```

  Put the Overview case in `test/kit/overview.test.tsx`, beside the existing kit tests.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass.**
- [ ] **Step 5: Commit** as "feat(os): figure row, ranked list and period control for statistics".

### Task 14: The client scope

**Files:**
- Create: `clients/os/src/apps/accounts/clientScope.ts`, `ScopeControl.tsx`
- Test: `clients/os/test/accounts/clientScope.test.tsx`

**Interfaces:**
- Produces:
  ```ts
  export interface ScopeOption { value: string; label: string }
  export interface ClientScope { showControl: boolean; value: string; options: readonly ScopeOption[]; accountIds: readonly string[]; set: (value: string) => void }
  export function clientScopeFor(access: ProfileAccess | null, accounts: readonly AccountRow[], stored: string): Omit<ClientScope, "set">
  export function useClientScope(stored: string, onStore: (value: string) => void): ClientScope // useSessionIfPresent()?.access + useAccountOptions()
  export function ScopeControl(props: { id: string; scope: ClientScope }): JSX.Element | null // kit Select tone="quiet", label "Client"; null when !showControl
  ```
- Rules (D13):
  - `everyAccount` gets `[{"", "All clients"}, ...selectableOrganizations(accounts)]`, sorted by name.
  - `accountIds.length > 1` gets `[{"", "All my clients"}, ...]` for exactly those ids. A name comes from `accounts`, else the group's `accountName`, else the id.
  - Anyone else gets `showControl` false, value `""` and `accountIds` `[]`.
  - A stored value not among the options becomes `""`.
  - `accountIds` is `[]` for `""`, else `[value]`.

- [ ] **Step 1: Write the failing tests.**
  - `TestStaffChooseAmongEveryClient`.
  - `TestAMemberOfTwoChoosesOnlyTheirs`.
  - `TestAMemberOfOneSeesNoControl`: renders nothing, and no other organization's name appears in `document.body`.
  - `TestAStoredChoiceOutsideTheOptionsFallsBackToAll`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/accounts/clientScope.test.tsx
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass.**
- [ ] **Step 5: Commit** as "feat(os): the client scope a statistics page reads under". Push the branch at the end of Phase 4.

---

## Phase 5: the pages

### Task 15: Deployable Statistics

**Files:**
- Create: `clients/os/src/apps/deployables/statistics/read.ts`, `useSiteStatistics.ts`, `SiteStatisticsView.tsx`, `StatisticsListPage.tsx`
- Modify `page/DeployablePage.tsx`:
  - The Head button becomes `IconButton label="Statistics"` with lucide `ChartLine`, still right after Logs.
  - `detail === "statistics"` renders `SiteStatisticsView`; `detail === "statisticsList"` with a held `Dimension` renders `StatisticsListPage`.
  - `detailTitle` gains `statistics: "Statistics"` and `statisticsList` (the dimension's title).
  - Drop the stale "Visit still asks in a dialog" comment.
- Modify `page/DeployableWorkspace.tsx`: `WorkspaceDetail` replaces `"traffic"` with `"statistics" | "statisticsList"`.
- Modify `settings.ts`:
  - `statisticsPeriod: Period` (default `"7d"`, validated with `isPeriod(SITE_PERIODS)`) and `statisticsScope: string` (default `""`) replace `trafficWindow`.
- Modify `traffic.ts`: keep only what the list row uses (`LIST_TRAFFIC_WINDOW`, `windowSpec`, `windowBounds`, `MAX_SITES_PER_READ`, `TrafficSummary`, `fetchTrafficSummaries`). Delete the rest.
- Delete `page/stops/Traffic.tsx`.
- Delete the `.os-traffic-*` rules in `clients/os/src/styles/index.css`, about lines 7692-7833. Keep the runtime-settings comment block inside that range.
- Modify the harness `test/deployables/harness.tsx`: `FakeSeed.siteStatistics?: Row` and `siteStatisticsError?: string`, plus a branch for `builtin siteStatistics(`.
- Modify `clients/os/qa/main.tsx` if a prop it passes changed.
- Tests:
  - Replace `test/deployables/traffic.test.tsx` with `statistics.test.tsx`. Keep its list-row cases (532-559) there.
  - Update `deployables.test.tsx:1019-1031` (button order; region `Statistics for <host>`), `accessCodes.test.tsx:183-213` (`statistics.nextElementSibling` is the lock), `reviews.test.tsx:104-107` and `settings.test.ts`.

**Interfaces:**
- Consumes: Tasks 8, 11, 12 and 13.
- Produces:
  ```ts
  export type Dimension = "page" | "source" | "country" | "device" | "browser" | "os";
  export interface SiteTotals { visitors: number | null; visits: number | null; bounces: number | null; pageViews: number | null; viewsPerVisit: number | null; bounceRate: number | null; requests: number | null; errors: number | null; notFound: number | null; bytes: number | null; botRequests: number | null; medianMs: number | null; p95Ms: number | null }
  export interface SitePoint { at: string; visitors: number | null; visits: number | null; bounces: number | null; pageViews: number | null; requests: number | null; errors: number | null; notFound: number | null; p95Ms: number | null }
  export interface TopEntry { value: string; visitors: number; visits: number; pageViews: number }
  export interface SiteStatistics { period: Period; bucket: Bucket; start: string; end: string; measuredSince: string | null; totals: SiteTotals; previous: SiteTotals | null; series: SitePoint[]; previousSeries: SitePoint[] | null; top: Record<Dimension, TopEntry[]>; sites: { siteId: string; visitors: number | null; visits: number | null; pageViews: number | null }[] } // previous* null for 12m
  export function parseSiteStatistics(row: Row): SiteStatistics // a JSON null stays null; never rowNumber() (it turns null into 0)
  export function fetchSiteStatistics(query: QueryClient, args: { siteIds?: readonly string[]; accountIds?: readonly string[]; period: Period }, signal?: AbortSignal): Promise<SiteStatistics | null>
  export interface StatisticsReading { value: SiteStatistics | null; state: "idle" | "loading" | "ready" | "error"; error: string; reload: () => Promise<void> }
  export function useSiteStatistics(args: { siteIds?: readonly string[]; accountIds?: readonly string[]; period: Period; enabled: boolean }): StatisticsReading // AutoRefresh 300_000 ms for 24h, else 900_000; aborts stale reads
  export function SiteStatisticsView(props: { site: SiteRow; onShowAll: (d: Dimension) => void }): JSX.Element
  export function StatisticsListPage(props: { site: SiteRow; dimension: Dimension }): JSX.Element
  ```
- Copy, exact:
  - Figures: "Visitors", "Visits", "Page views", "Views per visit", "Bounce rate". All five are chartable; a bucket's views per visit and bounce rate are worked out from its page views, visits and bounces. The rate uses `formatRate` from the kit (add it there if Email's `formatRate` is the only copy, and have Email import it).
  - Health figures: "Requests", "Errors", "Not found", "Response time, p95". Health facts: "Data served", "Median response", "Bot requests".
  - List titles: "Pages", "Sources", "Countries", "Devices". Devices has `LocalTabs` options "Device", "Browser", "OS".
  - Value words: `(other)` reads "Other", `(unknown)` "Unknown", `direct` "Direct".
  - Show-all buttons: "All pages", "All sources", "All countries", "All devices".
  - The Countries footer is a link to `https://db-ip.com` reading "IP Geolocation by DB-IP".
  - Before `measuredSince`, the reason is "Visitor counting started on {Nov 8, 2026}.". With no rows: "Nothing was recorded for {last 7 days}.".
  - A system-owned site keeps `SYSTEM_OWNED_TRAFFIC_NOTE`. A draft renders nothing.
  - Formats: ms reads "120 ms" or "1.4 s". Bytes use the kit formatter if `kit/format.ts` has one; if not, add `formatBytes` there with a test.
- Layout: the lists stand in a two-column grid when `useWide` reports at least 760px of pane (Pages beside Sources, then Countries beside Devices), and stack in one column below that. Health comes last.
- `PageHelp title="Statistics"` (the window's one information control, DESIGN.md) says, in five short paragraphs:
  - a visitor is counted once per UTC day, without cookies
  - a visit is a visitor's page views with no gap over 30 minutes, and a bounce is a one-page visit
  - days are UTC
  - a page view is a page load or an in-app page change
  - bots are left out and counted under Health
  - countries come from DB-IP's free database

- [ ] **Step 1: Write the failing tests** (`statistics.test.tsx`).
  - `TestStatisticsOpensAsAPageUnderTheDeployable`: no `dialog` role, and the trail ends in "Statistics".
  - `TestFiguresAndChartFromTheRead`: Visitors "1,204" and "+12%".
  - `TestPressingAFigureChartsIt`.
  - `TestThePeriodIsRemembered`: localStorage `memql-os-deployables-v1` gets `statisticsPeriod: "30d"`, and the next call contains `period: "30d"`.
  - `TestBeforeVisitorCountingTheFiguresSaySoInsteadOfZero`.
  - `TestAllPagesOpensTheFullListAsAPage`.
  - `TestCountriesCreditDbIp`.
  - `TestAReadRefusalShowsTheEnginesSentence`.
  - `TestASystemOwnedDeployableShowsTheNote`.
  - `TestTwelveMonthsShowsNoComparison`: no change text on any figure and no `data-series="previous"` path.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/deployables
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, plus `make os-typecheck`.
- [ ] **Step 5: Commit** as "feat(deployables): a deployable's Statistics page replaces Traffic".

### Task 16: Websites in the Deployables Overview

**Files:**
- Create: `clients/os/src/apps/deployables/statistics/WebsitesSection.tsx`
- Modify `map/MapSection.tsx`: render `WebsitesSection` after the status breakdown, before the map. Add a prop `onOpenStatistics(siteId)`.
- Modify `DeployablesApp.tsx`:
  - Wrap `mapContent` in the settings provider, which today wraps only the list and sources (comment at 420-425).
  - The openRequest `detail` type accepts `"statistics"`.
  - Wire `onOpenStatistics` to open the deployable with `detail: "statistics"`.
- Modify `DeployablesSection.tsx`: the `detail` type at lines 119 and 171 accepts `"statistics"`.
- Test: `clients/os/test/deployables/websites.test.tsx`

**Interfaces:**
- Consumes: Tasks 13, 14 and 15.
- Produces:
  ```ts
  export function WebsitesSection(props: { sites: readonly SiteRow[]; onOpenStatistics: (siteId: string) => void }): JSX.Element | null // null when no non-system site
  ```
- Layout:
  - `Subhead` "Websites" with `actions={<><ScopeControl/><PeriodControl/></>}`.
  - `StatFigures` (Visitors, Visits, Page views), then `TimeChart`.
  - `RankedList` "Websites": rows from `read.sites` named by `site.title || site.hostname`, valued by visitors (else page views). A row opens its Statistics.
  - Below it, `RankedList` "Sources" and "Countries" from `read.top.source` and `read.top.country`, already summed over the websites in scope, with the same words and the DB-IP footer as Task 15. Reuse Task 15's value-word and footer helpers rather than copying them.
- The read sends no `siteIds` and `accountIds: scope.accountIds`. The scope is stored in `DeployablesSettings.statisticsScope`.

- [ ] **Step 1: Write the failing tests.**
  - `TestWebsitesSumEverySiteInScope`.
  - `TestAWebsiteRowOpensItsStatistics`: the region "Statistics for <host>" appears.
  - `TestStaffSeeTheClientControlAndAMemberOfOneDoesNot`: uses `withSession({ role: "user", everyAccount: false, accountIds: ["acme"] })`.
  - `TestTheReadNamesTheChosenClient`: the call contains `accountIds: ["acme"]`.
  - `TestWebsitesShowSourcesAndCountriesForTheScope`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/deployables/websites.test.tsx test/deployables/map.test.tsx
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, keeping `map.test.tsx` green.
- [ ] **Step 5: Commit** as "feat(deployables): every website a person may see, together, in the Overview".

### Task 17: The Email Overview

**Files:**
- Create: `clients/os/src/apps/email/overview/statistics.ts`
- Modify `overview/OverviewSection.tsx`:
  - The props become `{ feeds, settings, update }`.
  - Use kit `Overview` with `figures={<StatFigures …/>}` and `actions={<><ScopeControl/><PeriodControl/></>}`.
- Modify `overview/figures.ts`: build figures from `EmailStatistics`. Delete `statsWanted`, `rateMetric` and anything else no longer called.
- Modify `EmailApp.tsx`: pass `settings` and `updateSettings` to `OverviewSection`.
- Modify `settings.ts`: `statisticsPeriod: Period` (default `"30d"`, `isPeriod(EMAIL_PERIODS)`) and `statisticsScope: string` (default `""`), each repaired independently.
- Modify `useEmail.ts`: delete `useCampaignStatsFor` if nothing else calls it (grep first).
- Modify the harness `test/email/harness.tsx`: `emailStatistics: reader(seed.emailStatistics)`.
- Tests: `test/email/overview/overview.test.tsx`, `test/email/overview/figures.test.ts`, `test/email/manifest.test.ts` (settings fields).

**Interfaces:**
- Consumes: Tasks 10, 11, 12, 13 and 14.
- Produces:
  ```ts
  export interface EmailTotals { sent: number | null; delivered: number | null; undelivered: number | null; failed: number | null; skipped: number | null; suppressed: number | null; opens: number | null; clicks: number | null; uniqueOpens: number | null; uniqueClicks: number | null; unsubscribes: number | null; bounces: number | null; complaints: number | null; subscribes: number | null; openRate: number | null; clickRate: number | null }
  export interface EmailPoint { at: string; sent: number | null; uniqueOpens: number | null; uniqueClicks: number | null; unsubscribes: number | null; subscribes: number | null; bounces: number | null; complaints: number | null; failed: number | null; suppressed: number | null; openRate: number | null; clickRate: number | null }
  export interface CampaignFigures { id: string; sent: number; uniqueOpens: number; uniqueClicks: number; openRate: number | null; clickRate: number | null } // also the accounts rows
  export interface AutomationFigures { id: string; kind: "rule" | "journey"; sent: number; failed: number; suppressed: number }
  export interface AudienceFigures { id: string; subscribes: number; unsubscribes: number; net: number }
  export interface EmailStatistics { period: Period; bucket: Bucket; start: string; end: string; measuredSince: string | null; totals: EmailTotals; previous: EmailTotals | null; series: EmailPoint[]; previousSeries: EmailPoint[] | null; campaigns: CampaignFigures[]; automations: AutomationFigures[]; audiences: AudienceFigures[]; accounts: CampaignFigures[] } // previous* null for 12m
  export function parseEmailStatistics(row: Row): EmailStatistics
  export function useEmailStatistics(args: { accountIds: readonly string[]; period: Period }): Reading<EmailStatistics | null> // the email Reading<T>; AutoRefresh 900_000 ms while visible
  export function OverviewSection(props: { feeds: EmailFeeds; settings: EmailSettings; update: (patch: Partial<EmailSettings>) => void }): JSX.Element
  ```
- Copy and layout:
  - Figures: "Sent", "Open rate", "Click rate", "Unsubscribes", "New subscribers"; the rates use `formatRate`.
  - Facts: "Subscriptions" (existing `useSubscriberCounts`, over the in-scope audiences) and "Active automations" (in-scope rules and journeys).
  - "Delivery problems" (spec 9):
    - its own `StatFigures` (Bounces, Complaints, Failed, Suppressed, all chartable), then a `TimeChart` of the selected one
    - a `Fact` "Delivered" only when the period has a delivered or undelivered fact (the provider reports outcomes); otherwise no Delivered at all
  - "Audience growth":
    - a `RecordList` of `RecordRow`s, one per `audiences` entry, named from the audiences feed
    - `secondary` reads "{12} new · {3} unsubscribed · {+9} net · {1,204} subscribed"; the last part comes from `useSubscriberCounts` and is left out while unread
    - a row calls `openRecord("audiences", { id })`
  - "Top campaigns":
    - a `RankedList` of `campaigns` in the read's order (unique opens, then unique clicks), valued by unique opens
    - `secondary` reads "{42%} opened · {3.1%} clicked", leaving out a `null` part
    - a row calls `openRecord("campaigns", { id })`
  - "Automations":
    - a `RecordList` of `RecordRow`s, one per `automations` entry, named from the rules or journeys feed
    - `secondary` reads "{120} sent · {2} failed · {1} suppressed"
    - a row calls `openRecord("automations", { id })`
  - "Campaigns": the existing status breakdown.
  - "Clients": a `RankedList` by sent, with the open rate as `secondary`. Shown only when `accounts.length > 1`.
  - The first-use empty state is unchanged.
  - `PageHelp title="Overview"` says:
    - an open or click rate is of the campaign messages sent in the period
    - automations show no opens or clicks, because automated mail is not tracked for them
    - a recipient counts once per campaign
    - mail privacy proxies make opens an upper bound
    - days are UTC

    The help does not repeat the figure labels.

- [ ] **Step 1: Write the failing tests.**
  - `TestOverviewShowsThePeriodsFiguresFromTheRead`.
  - `TestRatesComeFromTheCohort`: 0.42 shows "42%".
  - `TestPressingAFigureChartsIt`.
  - `TestDeliveryProblemsChartTheirOwnFigures`.
  - `TestDeliveredIsAFactOnlyWhenTheProviderReports`.
  - `TestAudienceGrowthRowsOpenTheAudience`.
  - `TestTopCampaignsRankByUniqueOpensAndOpenTheCampaign`.
  - `TestAutomationsShowSentFailedSuppressedAndOpenTheirPage`.
  - `TestTwelveMonthsShowsNoComparison`.
  - `TestStaffSeeAllClientsAndTheClientsList`.
  - `TestAMemberOfOneSeesNoClientControlAndNoOtherClient`.
  - `TestTheChosenClientIsSentToTheRead`.
  - `TestNothingMeasuredSaysSoNotZero`.
  - Keep the first-use test. `test/email/corrections.test.ts` stays green.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/email
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, plus `make os-typecheck`.
- [ ] **Step 5: Commit** as "feat(email): the Overview becomes per-client statistics". Push the branch at the end of Phase 5.

---

## Phase 6: no pop-ups

### Task 18: Visit page, Version page, Remove source in place

**Files:**
- Create: `clients/os/src/apps/deployables/page/VisitPage.tsx`, `VersionPage.tsx`
- Modify `page/VisitSiteButton.tsx`: for a storefront, `IconButton label="Visit {name}"` calls `onOpenVisit()`. Other kinds keep the direct link.
- Modify `page/Versions.tsx`: add `onOpenVersion(version, state, served)`, remove the dialog, and export `rollBackLegal`.
- Modify `page/DeployableWorkspace.tsx`: `WorkspaceDetail` adds `"visit" | "version"`, and `onOpenVersion` is passed through.
- Modify `page/DeployablePage.tsx`:
  - Hold the selected version.
  - Render `VisitPage` / `VersionPage` in the in-page detail frame with the trail `Deployables > {name} > Visit` and `... > Version {label}`.
  - On the version page the `ActionBar` acts are exactly `[Roll back to this version]` when legal and `[]` otherwise. The refusal stands in place as `ProblemNotice`.
- Modify `sources/RemoveSource.tsx`: confirm in place, following `apps/settings/PipelinesSection.tsx:186-219` (`GithubAppManagement`).
  - Show the consequence `Caption`, then `<InPlaceAsk className="os-confirm-row os-panel-actions" role="group" aria-label="Confirm source removal">` with Cancel and a danger "Remove source".
  - Move focus to Cancel on asking and back to the trash button on cancel.
- Delete `page/DetailDialog.tsx` and the `.deployable-dialog*` rules in `apps/deployables/composition.css` (lines 57-63, 74-75 and 209).
- Tests:
  - Update `test/deployables/visit.test.tsx`, `candidateDeploy.test.tsx:526-537`, `deployables.test.tsx` (the versions cases at 999-1042, 423 and 1682) and `sourceCatalog.test.tsx`.
  - Create `test/deployables/noDialogs.test.ts`.

**Interfaces:**
- Produces:
  ```ts
  export function VisitPage(props: { site: SiteRow; can: PartsHeld; readiness: PreviewReadiness | null; readinessError: string; onWritten: () => void; onOpenStore: () => void }): JSX.Element
  export function VersionPage(props: { site: SiteRow; version: SiteVersion; state: "Current" | "Candidate" | "Available"; served: boolean; bundled: boolean }): JSX.Element
  export function rollBackLegal(input: { site: SiteRow; version: SiteVersion; served: boolean; canPublish: boolean }): boolean // today's condition from Versions.tsx:99
  ```
- The roll-back act: `{ label: "Roll back to this version", tone: "primary", busy: lifecycle.busy, onAct: () => void lifecycle.rollTo(site.id, version.bundleRef), ariaLabel: \`Roll ${site.hostname} back to ${label}\` }`.

- [ ] **Step 1: Write the failing tests.**
  - `TestVisitOpensAPageWithTestingAndProduction`: no dialog.
  - `TestAVersionOpensAsAPageAndRollBackIsInTheBar`: the bar button calls `updateSiteBundle`.
  - `TestRollBackIsAbsentForTheCurrentVersion`.
  - `TestRemoveSourceAsksInPlace`:
    - the group is "Confirm source removal" and focus is on Cancel
    - a press before `CONFIRM_SETTLE_MS` is ignored
    - "Remove source" calls `setPackageSourceRemoved`
    - Cancel restores focus
  - `TestNothingInDeployablesOpensADialog`: scan `src/apps/deployables/**/*.tsx` for `/kit\/Dialog|DetailDialog|<dialog|role="dialog"/`.
- [ ] **Step 2: Fail.**

  ```bash
  cd clients/os && npm test -- test/deployables
  ```
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Pass**, plus `make os-typecheck`.
- [ ] **Step 5: Commit** as "fix(deployables): Visit, a version's details and Remove source open as pages, not pop-ups". Push the branch.

---

## Phase 7: documentation and delivery

### Task 19: Documentation changed with the code

**Files (all Modify):**
- `clients/os/DESIGN.md`:
  - A **Statistics** section after "App overviews": the kit statistics pieces are the only way to draw statistics; where `PeriodControl` and `ScopeControl` stand (Head or Subhead right, quiet); change and absence (§8); charts only from real series.
  - Remove the Deployables lines naming Traffic or a dialog.
- `docs/public/operate/deployables.md`:
  - Rewrite "## Traffic and health" (862-934) as "## Statistics and health": what is recorded and never stored, retention per tier, the ping and its insertion, `siteStatistics`, the monthly country download, and a privacy-policy paragraph an operator can adapt.
  - Fix the mentions at 366, 946 and 1006.
- `docs/public/operate/site-hosting.md`: lines 1154-1179.
- `docs/public/operate/campaign-sending.md`: a new "## Email statistics" after line 1886 (definitions, cohort rates, facts, `emailStatistics`, per-client scope), and an update to "## The Email app" (2364-2370).
- `docs/public/operate/site-access-codes.md:80`.
- `dsl/observability/concepts.memql:110-123`: the siteTraffic concept doc.
- `NOTICE`: "This product uses IP geolocation data by DB-IP (https://db-ip.com), licensed under CC BY 4.0."
- `clients/os/README.md:176, 998`, `clients/os/qa/loading-and-navigation.md:15` and `clients/os/src/apps/registry.tsx:234` (comment).

- [ ] **Step 1: Make the edits.**
- [ ] **Step 2: Run the documentation gates.**

  ```bash
  go test -count=1 github.com/znasllc-io/memql -run 'Docs|Database|Boundary|ClaudeMd|VendorApiKey'
  ```

  Expected: PASS.
- [ ] **Step 3: Commit** the listed files as "docs: website and email statistics, what is recorded and how to read it".

### Task 20: Verify, show and hand over

- [ ] **Step 1:** Run `make test`. Expected: PASS.
- [ ] **Step 2:** Run the database-gated trees with the Setup env.

  ```bash
  go test -count=1 -p=1 github.com/znasllc-io/memql/component/sitetraffic/... github.com/znasllc-io/memql/component/emailstats/... github.com/znasllc-io/memql/component/campaigns/... github.com/znasllc-io/memql/component/automations/... github.com/znasllc-io/memql/test/dslconformance/...
  ```

  Expected: PASS with no skips. `MEMQL_REQUIRE_DB=1` turns a skip into a failure.
- [ ] **Step 3:** Run these checks. Expected: all pass.

  ```bash
  make sdk-gen-check
  ```

  ```bash
  make frontdoor-paths-check
  ```

  ```bash
  make os-typecheck
  ```

  ```bash
  make os-test
  ```
- [ ] **Step 4:** Ask the owner before touching the local cluster, then run `make dev`. Check these in Chrome at `https://os.memql.localhost`, in light and dark, wide and narrow, empty and populated, with keyboard and reduced motion:
  - a deployable's Statistics page and its full lists
  - the Deployables Overview Websites section
  - the Email Overview as staff and as a single-client Member
  - the Visit and Version pages
  - Remove source

  Send the screenshots to the owner.
- [ ] **Step 5:** Delete this plan (`git rm docs/superpowers/plans/2026-11-08-site-and-email-statistics.md`); a plan is spent in the epic's own merge. Commit and push. Report that the work is on `feat/site-and-email-statistics`, not on main, and list anything unverified.

---
title: "Email, encrypted Inbox and derived deployment directories"
audience: internal
status: stable
area: build
sinceVersion: 0.31.23
owner: znas
---

# Email, encrypted Inbox and derived deployment directories

These checks use synthetic local fixtures in the same disposable PostgreSQL
instance as the engine paging measurements. They are not production timings.
No resource allocation, cluster setting, or database index was changed.

## Directory contracts

- Email campaigns, audiences, template summaries and each automation source
  search and apply facets in the named server query before a ten-row page.
  Original complete reads remain available for explicitly opened editors and
  pickers. Selected records are read independently from the loaded directory.
- `emailInboxPage(search?, accountId?, cursor?)` returns at most ten decrypted
  metadata summaries. Encrypted payloads remain encrypted at rest. The server
  examines at most 100 messages per request, including when a sparse search
  has not found ten matches; a continuation still appears in that case. The
  UI distinguishes that partial empty result from an exhausted search.
  `emailInboxMessage(messageId)` returns the body only for an opened message.
  Both preserve the signed-in operator requirement and rolling seven-day
  capture window. A cursor binds the search and account, retains its snapshot,
  and cannot extend that window by aging or supplying an older snapshot.
- `deployableDirectoryPage(resource, search?, kind?, status?, accountId?,
  source?, archived?, cursor?)` returns ten entries. The `resource` is
  `deployables` or `sources`. The server combines deployed sites, pending
  analyses and declared but undeployed apps before filtering and pagination.
  Repository summaries count their complete app set. Disabled declarations,
  scoped pending runs, exact archive counts and repository access provenance
  remain represented. Detail fields, build reports, settings and logs do not
  travel with list rows. Site hydration is one bounded batch, not one read per
  visible row. Unavailable provenance is explicit; the UI does not present an
  exhaustive zero-match answer when a provenance search could be incomplete.

The derived deployment directory still reads the complete authorized compact
indexes on the server in bounded source pages to compute exact joins, counts
and search results. This removes full catalogs from the browser and excludes
large detail payloads from its response; it is not a claim that the reducer
scans only ten underlying records. Complete maps and setup pickers request
full catalogs only when opened. Their retained models survive temporary
navigation away while network demand is released.

## Deterministic regression evidence

`component/memql/email_browse_db_test.go` executes five Email browse queries
against real organization memberships: 25 peer-authored Acme rows traverse
10/10/5; the twelve older search matches traverse 10/2. A row authored by the
caller in another organization stays excluded. Template bodies/documents are
absent and each page remains below 16 KiB. The same file executes the directory
source queries, batch site hydration and `packageNamesByIds` with twelve
peer-authored organization records plus a foreign record. This is the positive
proof supporting their existing mandatory-organization-boundary adjudications.

`component/packages/directory_db_test.go` verifies 25 repositories and fifty
derived apps, preserving two-app summaries, complete traversal and literal
case-insensitive search. Repository pages stay below 24 KiB. Adding a deployed
site and a pending run replaces the corresponding virtual declarations rather
than duplicating them. Large settings, build plans and logs are absent from the
response, which stays below 8 KiB for that joined result. A second engine reads
the same stored definitions. Unit tests additionally cover mixed canonical and
bare references, timestamp precision/offsets, a single site hydration batch,
old session cursors, and partial provenance failure.

`integrations/email/inbox_page_test.go` verifies ten metadata rows below 4 KiB,
bounded sparse search continuation, account filtering, changed-filter cursor
rejection before storage, operator permissions and selected-body lookup.
`capture_db_test.go` executes real captured-email mutations and reads the
result from another engine through 10/10/5 Inbox pages.

The full Email UI suite passed 715 tests. The Deployables suite plus shared
arrival tests passed 1,461 tests. These include ten-to-twenty-to-twenty-five
paging, older-record search, clearing search to the retained twenty rows,
closing/reopening without another initial read, no eager complete catalogs,
complete editor options, live provenance changes, failed read recovery,
retained setup drafts, observed site health and preserved update cues.

Run the native tests with `MEMQL_REQUIRE_DB=1` and `MEMQL_DATABASE_DSN` pointing
only to a disposable migrated test database:

```sh
go test ./component/packages ./integrations/email -run 'TestDirectory|TestInbox' -count=1
go test ./component/memql -run 'TestEmailBrowse|TestPlatformDirectory' -count=1
```

From `clients/os`, run:

```sh
npx vitest run test/email test/deployables test/live/live.test.tsx
```

## Secondary directories

Archive transfer browsing accepts optional `limit` and `search` arguments;
existing callers keep the default 100. The OS asks for ten. Search covers the
stored name, path, source kind, worker identity/name and failure message before
the owner's stable recent cursor and SQL limit. The actual backup journal
capability test traverses 25 rows as 10/10/5, finds twelve older literal `%_`
matches as 10/2, excludes another owner's rows/cursors, and bounds ten-row
responses below 12 KiB.

Reviews and the decision log now retain ten-row pages in the session cache,
with server search before paging. Waiting keeps its established oldest-first
moderation order; published reviews and decisions remain newest first. New
continuations are keyset cursors bound to the store/site/filter/search; old
numeric continuations remain accepted. A decision invalidates the relevant
cache scope, while inactive pages wait until reopened to refresh their retained
depth. Tests cover search, 10/20/25 growth, close/reopen, log tab retention and
mutation-driven refresh of a retained twenty-row list.

The actual Reviews database fixture contains 525 reviews and 525 decisions.
It verifies complete derived counts beyond the previous implicit 500-row
source ceiling, older search matches, ten-row output and a real
`appendReviewDecision` mutation becoming visible through a second engine.
Another caller cannot read the source site. Internal sources are now explicit
500-row pages walked to completion; moderation state/counts still require the
server to reduce the complete authorized source set. The browser receives only
the requested ten rows, not an expanding fifty-to-two-hundred-row prefix.

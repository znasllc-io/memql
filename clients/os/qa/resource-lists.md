# Resource lists and responsiveness verification

Source audit and local verification, November 7, 2026. This task has not deployed
these changes or changed instance resources. The measurements below are build
sizes and controlled fixture results, not production latency claims.

The [final verification audit](../../../docs/internal/qa/os-list-performance/verification.md)
consolidates the full 6,885-test OS run, 481-test SDK run, isolated database
classes, package repair history and all 48 mounted browser flows. Focused logs
below describe their own earlier scoped runs; they are not the final total.

## Shared implementation

`RecordList`, `RecordRow` and `LiveList` provide the shared resource-list
presentation: spacing, plain metadata, hover and keyboard focus; no horizontal
row dividers or boxed metadata tags. The shared `LoadMore` control preserves
loaded rows and offers a retry when continuation fails.

`usePagedCollection` uses the connection-scoped `sessionCollections` registry.
First reads request ten rows; only Load more requests another page. Search,
filters and order belong in both the server query and collection key. Closing
a window releases subscriptions after the navigation grace period, preserving
rows and cursors until sign-out or document reload. Hidden retained panes stop
retaining reads. Authentication changes dispose the previous connection cache.
Events update the visible collection; continuity recovery reseeds the loaded
depth without resetting it to ten. See the OS README for the authoritative
contracts and the SDK live-collection tests for races, continuation and bounds.

A summary page is not a complete membership roster, reverse-reference index,
archive traversal, picker catalog or deep-link lookup. Those reads remain
separate and authorized. Selected details keep full lifecycle models and
provenance checks; list summaries cannot authorize a mutation.

## Startup changes

- Packages, GitHub credentials, GitHub connections and external connections are
  demand-driven shell catalogs. Zero consumers means zero initial reads;
  consumers share one retained collection. Closing the final consumer releases
  work while the authenticated session retains the answer.
- The app registry loads navigation metadata immediately and app code on first
  open. Concurrent opens share the download; successful app code stays cached.
  Failed imports can retry. A title-bar Search clicked before its app loads
  waits for the app's search control to register.
- Readiness providers retain their context identity when heartbeat events leave
  the actual readiness verdict unchanged.
- Ask previously refreshed its entire conversation history on shell mount,
  readiness and resume; its transport drained every cursor. History now reads
  only while the Conversations panel is visible. Each search and loaded depth
  lives in the owning conversation session, independent of the transcript.
  Concurrent refreshes share one request, event bursts coalesce, and closing
  the final panel aborts its read. Active replies and selected-conversation
  recovery retain their original lifecycle behavior.

## Migrated primary directories

The table describes implemented source paths. Each row keeps the shared
presentation and authenticated session retention; exceptions are explicit.

| Surface | Browse path and preserved behavior |
| --- | --- |
| Deployables and Repositories | `apps/deployables/useDirectory.ts`: native derived directory returns ten entries plus compact totals; server search/filtering. Full source/deployment data is demanded by selected editors and lifecycle operations. |
| Pipelines and runs | `apps/deployables/pipelines/feeds.ts`, `RunsSection.tsx`: ten-row resource reads, server search/status, by-id details. List-open child/step fanout removed. |
| Fleet machines/workspaces | `apps/fleet/machines/MachinesSection.tsx`: ten-row searchable directory; detail/routing facts are separate. Desktop origin cards do not retain the whole inventory. Workspaces use the existing owner-scoped query with ten rows, server search/released filter; replica topology reads wait for the Replicas tab. |
| Accounts | Ten-row recent authorized client browse with server name/domain/contact search and archived facet. Account by-id detail and full picker/authority catalogs remain separate. Organization membership stays the row boundary. |
| Users people/groups/access requests | `apps/users/PeopleSection.tsx`, `useGroups.ts`: server-paged merged roster and group directory. Registered/invited semantics and person/group deep links preserved. Access requests now use existing admin-gated `pendingAccessRequests` with ten rows, server search and session continuation. |
| Files and backup transfers | `apps/files/useLibrary.ts`: ten artifacts per page, server facets/search/order; backing files for visible IDs, direct selected detail, compact folder structure. Exact aggregate facets walk all authorized source pages. Full subtree traversal occurs only for an explicit archive operation. Backup transfers request ten native journal records, search before paging, append/cache pages and pause status refresh while hidden. |
| Bin | Ten archived artifacts and ten archived folders merge into ten globally recent visible rows; server search precedes both cursors. Narrow folder metadata remains complete for accurate filed-in names. Full file detail is on demand. |
| Email | `apps/email/useEmail.ts` and section feeds: campaigns, audiences, templates, senders, automations, subscribers, deliveries and inbox use bounded pages and server search. Per-visible-page summaries and direct record reads replace root-wide related-record fanout. Inbox decrypts authorized pages incrementally; selected message bodies load separately. |
| Materializer | `apps/materializer/useCompose.ts`: compositions, templates and recipes have ten-row server-filtered summaries and by-id detail. Batch visible run-status reads replace per-row work-run fanout; wizard catalogs start on demand. |
| Fleet routing history | `apps/fleet/routing/HistoryTab.tsx`: ten-row server search/facets and session-retained continuation through the existing admin-gated decision read. |
| Selected-site Reviews and Decision log | `apps/deployables/reviews/useReviews.ts`: ten-row cursor pages and server search, retained for the selected store/site and filter; existing review decisions remain independently authorized. |
| Nexus | `apps/nexus/useNexusDirectory.ts`: goals, runs, approvals and automations use ten-row server search/filtering; visible-record status joins and selected record lookup are scoped. |
| Concepts | Registry remains a bounded schema catalog; generic rows load ten newest authorized records with server text search. Native search uses public declared string fields and ordinary row authorization; opaque cursors bind to concept/search. Retained pages survive reopen; explicit change band and raw record detail remain. |
| Training files/review/chunks | `useLibraryFiles.ts`, `useReviewQueue.ts`: ten-row server search, Review-only native pending reads, required-domain chunk inspector. Analysis runs are batched for visible file IDs; no blob content or unrelated full run histories at list open. |
| Training domains | `useDomains.ts`: native complete count reducer returns one record per domain, alongside the compact domain configuration index. Domain search remains local over this required configuration tree; chunk content is paged separately. |
| Training skills | `SkillsSection.tsx`: verified ten-row summaries, server name/description/category search; content and ten-row selection history load only after selection. Full server provenance verification remains. |
| Training lessons/evaluations/usage | Current lesson heads fold all authorized revisions before search/page boundaries; selecting a lesson loads ten revisions or ten usage selections plus their batched delivery receipts. Evaluations filter by scenario/search before ten-row pages. Counts explicitly describe loaded results. |
| Cluster Agents | `agents/AgentsSection.tsx`: ten-row name/description/role/slug search using existing `allAgents`; its full-model shape and prior shared-catalog authorization remain unchanged. Internal registry loading still uses its complete dedicated read. |
| Cluster Audit and Origins failures | `audit/AuditSection.tsx`, `origins/DeadLetters.tsx`: ten-row server searches and Load more. Dead letters read only for the opened target; Retry/Discard actions retain their original semantics. |
| Cluster Releases and Updates | `releases/ReleasesSection.tsx`, `updates/UpdatesSection.tsx`: candidates, build requests and published choices have ten-row search/continuation and session retention. Native cursors bind search and owner/source. Selected release authority is revalidated independently of cached summaries. |
| Ask saved conversations | `ask/conversationHistory.ts`, `AskConversations.tsx`: ten title summaries, server title search, session-retained pages/searches, single-flight/coalesced refresh, cancellation on final close. No transcript content enters this list response. |

## Secondary surfaces and deliberate exceptions

Finite configuration lists (roles, route chains/rules, module/schema registries,
provider catalogs, publisher trust settings and connection choices) retain full
answers where a form must offer every valid choice. Log tail/search keeps its
bounded streaming/search contract. A selected record's immutable facts and
workflow graph are not another resource directory.

The remaining exceptions below are outside resource-directory migrations:

| Surface | Remaining behavior |
| --- | --- |
| Identity token management | `identity/IdentityAccount.tsx` uses its existing identity-route Show more navigation, replacing the page. |
| Release wizard pickers | `StartRelease.tsx` and `PrepareRelease.tsx` keep explicit-flow 50-row Older/Newer pickers; these do not run when the directory opens. |

Training's native pending page uses a ten-row metadata keyset query, then reads
full text only for those ten IDs grouped by required domain (at most ten scoped
reads). Its opaque cursor binds the search, pending filter and fixed timestamp.
Approving an already displayed row cannot shift this frontier past an unread
row; the cross-replica regression proves a complete 10/10/5 traversal after a
concurrent approval. The direct full-text query still requires one domain;
neither the UI nor the wrapper adds an all-domains escape. Existing reader
admission to a named domain stays unchanged; the native Review surface requires
writer rank. It narrows the established metadata query with the existing
`paginate(...,10)` operator. Complete domain count reducers retain that query's
default 500-row metadata pages, with no new authorization surface.

The trainer keeps its intentional 40 accepted chunks, 600 characters each plus
truncation marker. It now follows ten-row source cursors until that budget is
filled or the domain is exhausted. The regression fixture puts 60 rejected
chunks before 40 eligible chunks and verifies all 40 are included; an error or
non-advancing cursor returns no partial corpus.

## Measured evidence

Raw logs and browser results live in `qa/evidence/`. The engine's matched
baseline/branch SQL benchmark and its limitations are in
[`docs/internal/qa/os-list-performance`](../../../docs/internal/qa/os-list-performance/README.md).

### Initial build download

Both checkouts were rebuilt with the same installed OS dependency tree and the
same current generated SDK. Baseline is commit
`8b7b3a4830650adbe7a0ed3547e727e861979fbf`. Both emitted HTML files load one
initial JavaScript entry and no additional static module-preload scripts.

| Vite build output | Baseline | Changed |
| --- | ---: | ---: |
| Initial JavaScript | 3,348.84 kB | 1,363.30 kB |
| Initial JavaScript gzip | 953.72 kB | 351.34 kB |
| Initial CSS | 344.81 kB | 279.43 kB |

Initial JavaScript is 59.29% smaller (about 63% smaller gzip). App chunks and
voice code still download when requested. This reduces initial download/parse
exposure; it does not measure the user's production response time. Earlier
exploratory baseline numbers used an older generated SDK and are superseded by
this matched comparison.

`startup-build-baseline.log` and `startup-build-after.log` contain raw build
output. `startup-build-comparison.json` records exact emitted byte sizes and a
separate reproducible Python gzip-level-6 calculation; gzip byte totals can
differ slightly from Vite's compressor.

### Data and request budgets

| Same-fixture measurement | Before | Changed | Evidence / boundary |
| --- | --- | --- | --- |
| Skills, 200 models with 16,800-byte bodies | 3,390,792-byte full catalog | 1,047-byte first ten summaries | `training-skill-payload.log`; handler fixture, not DB timing. Verification still reads full models server-side. |
| Domains, 600 chunks / 3 domains | 500-row / 44,001-byte capped answer | Complete 600-count / 367-byte rollup | `training-domain-rollup.log`; real PostgreSQL, complete source cursor walk. |
| Library facets, 601 owned + 1 foreign artifact | Raw artifact-derived facets | 152-byte aggregate, exact folder counts and oldest label | `library-facets.log`; real DB authorization and >500 correctness. |
| Lessons, 525 revisions / 25 current heads | 2 browser requests / 909,577 bytes | 1 browser request / 15,089 bytes for 10 heads | `training-lessons.log`; real DB; highest revision folds before search. Server still scans both source pages. |
| Ask, 25 conversations | 25 summaries / 3,234 bytes | 10 summaries / 1,269 bytes | `ask-browse.log`; real DB owner isolation, literal older-title search 10/2, page sizes 10/10/5. |
| Ask shell startup | Mount/readiness history scans | 0 history reads with closed panel | `test/ask/historyPaging.test.tsx`; frontend demand test. Opening history 10, Load more 20, reopen 20 without a read. |

Native domain, lesson and Library facet reducers walk authorized 500-row source
pages. They remove browser fanout and payloads, but still scan the underlying
rows on the server. Published release search may walk verified remote catalog
pages to find ten matches; its source-bound cursor, timeout and page budget
prevent an unbounded remote scan. No in-process cross-role result cache was
added to these reducers.

### Focused verification

- `ask-frontend.log`: 34 suites / 319 tests passed, covering all Ask behavior and
  release lifecycle regression tests. Full OS source/test typecheck passed.
- `secondary-frontend.log`: 33 suites / 404 tests passed (Training, Cluster, Workspaces, backup transfers and access requests), including 10→20, cached reopen and older-resource server search.
- `secondary-browse.log`: real DB 25-row Workspace/AccessRequest paging and search, released filter, owner boundary, admin admission and the undeclared-concept land gate all passed.
- `cluster-frontend.log`: 29 suites / 348 tests passed (Training, Cluster and live
  read recovery). Paging tests include close/reopen, search, late responses,
  failed continuation and retained lifecycle actions.
- `training-browse.log`, `cluster-browse.log`, `release-browse.log` contain real
  DB paging/search/authorization tests. Default 25-row fixtures exercise 10/10/5
  and 12 older matches exercise 10/2. Lesson aggregation uses 525 revisions;
  domains/facets exceed the default 500 ceiling.
- `resource-browser-checks.json` records Files and Materializer browser checks
  in desktop/phone, light/dark: 10→20, older-resource search, restored 20, no
  horizontal overflow, no row dividers or boxed tags. PNGs retain representative
  desktop/phone output. These are local fixture checks, not production traffic.

Reproduce from the repository root, with the disposable test DB configured as
`MEMQL_DATABASE_DSN` and `MEMQL_REQUIRE_DB=1` (the latter makes unavailable DB
infrastructure fail rather than silently skip):

```sh
npm --prefix sdk/ts run build
npm --prefix clients/os run typecheck
npm --prefix clients/os run build
npm --prefix clients/os test -- test/ask test/cluster test/training test/live/readRecovery.test.tsx
go test ./component/memql -run 'TestSecondaryBrowsePagesAndSearchDB|TestAskBrowsePagesSearchAndOwnerDB|TestClusterBrowsePagesSearchAndScopeDB|TestTrainingBrowsePagesSearchAndScopeDB|TestTrainingEvidenceBrowseScopesAndBatchesDB' -count=1 -v
go test ./integrations/knowledge ./integrations/library -run 'TestTrainingReviewNativePagesAndDomainBoundaryDB|TestTrainerCorpusWalkPreservesPromptBudgetAndRejectsPartialDB|TestTrainingDomainRollupsDB|TestLibraryBrowseFacetsWalksEveryAuthorizedPageDB|TestTrainingLessonBrowseFoldsAllRevisionsBeforeSearchAndPagingDB' -count=1 -v
go test ./integrations/work -run 'TestSkillCatalog(Page|Entry|Pages|Browse)' -count=1 -v
go test ./integrations/release ./component/releasecatalog -run 'TestCandidateBrowseSearchBeforePagingDB|TestReleaseBuildBrowseSearchBeforePagingDB|TestReleaseSearchPagesBeforeReturningTenAndBindsContinuation' -count=1 -v
```

For the build comparison, run the same OS build in the baseline checkout with
its `node_modules` linked to this checkout's installed dependency tree. Retain
both complete command outputs; compare only assets referenced by emitted
`dist/index.html`, not the sum of all deferred app chunks.

- Nexus/Concepts proof: `test/nexus` plus `test/concepts` passes 571 tests.
  `TestNexusBrowsePagesSearchScopeAndPayloadDB` exercises four real database
  directories at 10/10/5, filtered 10/2, caller scope and summary payload budgets.
  `TestConceptRowsSearchAuthorizedCursorDB` proves 10/10/5 across replicas,
  caller authorization and rejection of a cursor from a different search.
- Mounted browser checks: [`nexus-browser-checks.json`](evidence/nexus-browser-checks.json)
  and [`concepts-browser-checks.json`](evidence/concepts-browser-checks.json) record 20 flows across 1280/390 widths and both
  themes. Each verifies 10 → 20 → server-search 1 → restored 20, no horizontal
  overflow, no row dividers or boxed row metadata, and no browser errors.
- Generated request verification: the SDK generator uses the engine argument
  grammar; inline multi-field query/mutation/logic schemas reach both generated
  clients. SDK tests exercise actual generated search/facet/by-id request strings,
  including Nexus, Training and Cluster. A mocked named-method assertion alone
  cannot detect a builder which discards arguments.

- [`identity-fleet-browser-checks.json`](evidence/identity-fleet-browser-checks.json)
  records 12 equivalent mounted Accounts, Users and Fleet flows. All 32
  identity/Fleet/Nexus/Concepts screenshots are retained beside these matrices,
  named by app, theme and viewport width. These checks use local synthetic data.
- `identity-directory-scope.log` proves Accounts and Groups retain organization
  membership authorization while paging 10/10/5. Existing People and Invitation
  queries preserve their developer rank floor; their directory wrappers page
  10/10/5, filter before paging, and omit expired invitations only when the
  directory explicitly requests it. The legacy invitation call retains its
  existing result semantics.

- `window-search-regression-before.log` and `window-search-regression-after.log`
  retain a reproduced lazy-loading search defect and the passing fix. Five
  dedicated cases cover current-section search, delayed destination fallback,
  hide, other-section navigation and same-section reset; the full focused
  lifecycle/chrome run passes 45 tests. `sdk-directory-regressions.log` records
  all 480 SDK tests passing, including generated arguments, paging envelopes
  and offline subscription recovery.

- `conversation-discovery-before.log` reproduces a capability-discovery
  regression introduced by the new title browser: alphabetic truncation to
  three reads displaced the message-recall capability. The shortlist now
  prioritizes that already-authorized recall path without increasing its
  limit. `conversation-discovery-after.log` records a fresh isolated database
  pass for the unchanged conversation speaker/owner test and four discovery
  regressions, including bounded ordering and unavailable-capability exclusion.

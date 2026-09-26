# Loading and nested navigation coverage

Every registered app was reviewed for initial reads, refreshes, inline values,
and local view selectors. This inventory describes source coverage; rendered
verification is tracked separately from a passing typecheck.

| Surface | Fetch placeholders and navigation |
| --- | --- |
| Cluster | Mesh and Agents retain shared live-list skeletons; Audit Trail, Data Origins, dead letters, Modules, module details, readiness, automation map and loop stops use matching placeholders. |
| Fleet | Machine directory and overview, recent work, sharing picker, pull history, app sessions, delegation and machine routing, all three Model Library views, and call history. Machine views now use the same `LocalTabs` as Model Library, Activity, Workbenches and Task Routing. |
| Accounts | Account detail bands, credential collections and account live lists. |
| Users | Shared directory/membership live lists, with a detail skeleton while a deep-linked group is first read. |
| Bin | Shared `LiveList` handles initial reads and preserves archived rows during refresh. |
| Files | Shared live lists and explicit file-version skeletons; the folder hierarchy and version chronology retain their navigation model. |
| Deployables | Shared list reads, map, source metadata, repository/account choices, compose prerequisites, versions, domain guidance, traffic, inline workspace values and store connections. Ordered deployment/setup rails remain workflows. |
| Campaigns | Shared live collections; audience members, template sample, delivery ledger and breakdown reads. Recipient filters remain filters. |
| Concepts | Registry, concept details, initial rows and additional pages. Schema and row inspectors retain their split view. |
| Logs | Search/tail data and settings reads; loaded virtual rows remain usable on refresh. |
| Materializer | Live collections and composer prerequisite form. |
| Nexus | Live lists, maps, step histories, automation catalog and journal. Execution and approval status remains visible. |
| Training | Review queue, knowledge domains and shared live collections. Document ingestion is an operation, with its own progress. |
| Settings | Access roster, rules, decisions, language, cluster/mail facts, benchmark, tokens, keys, policies, integration reports and provider registry. Access's two sibling views use `LocalTabs`; saved preferences remain form choices. |
| Identity | Shared form skeleton while the native identity page is fetched; passkey/email interactions retain meaningful operation status. |
| Ask | Conversation-shaped history placeholder; active model calls retain the reverse response estimate. Sheet and widget share the same surface. |
| Shared surfaces | OS bootstrap, app setup facts, connection dialogs, `LiveList`, `RecordListSkeleton`, `ContentSkeleton`, `InlineSkeleton`, `LocalTabs`. |

The governing rules are in [DESIGN.md](../DESIGN.md#loading-is-the-shape-of-the-content-never-a-message).
A skeleton's label is screen-reader-only. Errors, disconnections and successful
empty reads remain distinct; queued work and a page fetch are not the same state.

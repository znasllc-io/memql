# Record-list consistency audit

The reference is Deployables: `RecordRow`, `RecordList`, and `Head.meta`.
All comparable app record collections use these components. `LiveList` supplies
the responsive list container while retaining its subscriptions, arrival cues,
error/empty states and semantic list items. Static reads use `RecordList` with
an existing list or `as="ul"`. Independent row actions are siblings of an
opening button; disclosure buttons retain `aria-expanded`.

`listCount` only returns a count for a live, error-free snapshot. Counts describe
the authorized filtered population. On-demand reads count only after successful
completion. A read with a cursor or cap labels its loaded/shown population and
keeps its existing pagination/total explanation. Dependent reads (for example a
People group filter) must settle before a count is presented. Disconnected,
loading, stale and refused reads do not invent a zero; an authoritative empty
answer does display zero.

## Coverage by app

| App | Comparable record surfaces |
| --- | --- |
| Accounts | Client registry; billing accounts; credentials; client group links and bounded ledger evidence |
| Bin | Archived files and folders, retaining selection and adjacent inspector |
| Files | Browsing/search results (including Desktop, Bin and materialized places); backups; version-history title count |
| Deployables | Deployables and Sources already canonical; source-produced apps, source credentials, Library zip choices, package report records, paired development stores; GitHub repository choices and group counts |
| Fleet | Machines; machine apps/models; recommended models; previous downloads; recent work; app sessions and produced artifacts; available models and host machines; catalog and uncatalogued models; workspaces/replicas; routing call history and policy references |
| Users | People, groups and roles; memberships, invitations, standing managers, role holders and sessions |
| Campaigns | Campaigns, audiences, senders, templates and rules; audience recipients and delivery ledger |
| Training | Files/teaching worklist, domains and domain chunks; existing domain disclosure preserved |
| Materializer | Compositions, templates, recipes and recorded composition sources |
| Cluster | Agents and standing grants; audit events; modules and per-node readiness; connector coverage, origin health and dead letters; standalone automations, relationships and stopped chains |
| Concepts | Grouped registry and browsed rows, preserving filters, paging and row inspection |
| Nexus | Goals, runs, approvals, automation catalog, goal run lists and journal records |
| Identity | Passkeys, sessions and tokens, including rename/revoke and token pagination |
| Settings | Apps, tokens, keys/keysets, rules, decisions, levels, doors/providers, integrations, deprecated language forms/uses, access grant holders, node versions and hidden permissions |
| Logs and app log sections | Archive record lists; truthful filtered counts on streaming/search windows |

## Inspected exceptions

These were inspected against the reference rather than omitted by their filename.
The shared row has a compact density and separate actions, which suffice for
ordinary directories and worklists. It does not replace specialized interaction
models below:

- **Logs' virtual grids:** fixed-height 22/30px rows, arrow-key selection,
  subject narrowing, following the stream and virtualization. Two-line record
  geometry would break their row offsets and keyboard model. Archive records
  use the shared row; grid headings still use honest filtered counts.
- **Trees, graphs and ordered histories:** Files' folder/place rail, schema and
  payload inspectors, Fleet/automation topology, causal traces, execution steps,
  file-version chronology and connection transitions retain their hierarchy or
  temporal position. Version history counts its server-reported total and keeps
  the existing shown/total explanation.
- **Permission and diagnostic comparisons:** permission matrices, rank diagrams,
  hardware/environment facts, consent checks, routing decision comparisons,
  benchmark plots and ordered decision evidence keep the axes that give values
  meaning. They are not independent records with an open action.
- **Pickers and editors:** Equipment banks, account/source selection,
  policy/task-rule composition, ordered source/configuration editors,
  preference controls and merge-tag controls remain form controls. Library zip
  record choices and GitHub repository choices use `RecordRow`. Repository
  choices preserve organization groups, branch/privacy/push facts, selected state,
  paging, and explicit refresh while following the canonical list anatomy.
- **Rich content/workflows:** conversations/transcripts, full-text chunk review,
  upload progress, import diagnostics, install/setup steps, preview checks and
  lifecycle/version rails retain content or ordered workflow semantics. Theme
  previews and launcher icons remain visual selection/navigation galleries.
- **Summaries:** overview charts, scalar definition lists, chart legends and
  bounded rollup statistics retain their summary layout. Account ledger record
  samples now use compact shared rows without inventing additional queries.

No navigation model, authorization query, enrollment, token, database schema or
attention revision is changed by this visual standardization.

## Validation

The component tests cover semantic list items, disclosure state, independent
row actions and absent counts for loading/degraded/disconnected/refused reads.
Accounts regressions cover filtered counts and live arrivals. Representative
app regressions cover dependent permission reads, filter-to-zero, pagination,
rename/revoke, and existing actions.

Browser QA uses real components over the existing test connection fixtures:
`?view=list`, `accounts`, `accounts-empty`, and `origins-mixed`, in light/dark
mode at 1280px and 560px widths. Accounts matches the reference's typography,
column alignment and flat row treatment. Origins verifies responsive metrics
and sibling actions; neither surface overflows horizontally. The owner's live
page is not navigated or used for fixture writes.

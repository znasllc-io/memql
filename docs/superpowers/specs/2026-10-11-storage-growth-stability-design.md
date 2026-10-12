# Storage growth stability release (design)

Status: approved by the owner, 2026-10-11. Release 1 of 2.

## Why

On 2026-10-11 a production instance with no user activity filled its 32Gi
database volume and was down for ten hours (instance record:
`znasllc-io/memql-znas` `docs/database-reliability.md`, October 11). Measured on
that database: about 145,000 new `"MemoryNodes"` versions a day, about
1.1 GB a day, almost all of it the engine writing about itself.

| Source | Share of rows written | Mechanism |
|---|---|---|
| `v1:worker:registration`, `v1:cluster:node`, `v1:platform:moduleReadiness` | about 40% | every heartbeat appends a full new version (15 s per worker, 60 s per node); nothing prunes superseded versions |
| `v1:work:run` + `v1:work:step` of scheduled automations | about 35% | every firing journals a durable run (3+ versions) and its steps, including firings that change nothing; 93% of all runs were such ticks |
| `log_line` | separate table, about 100 MB a day compressed | info-level lines per tick and per replica |

The retention that exists did not hold the line: `workJournalRetentionSweep` and
`logsRetentionSweep` both failed every night on the request pool's 8-second
statement timeout (and pgdriver's 10-second read deadline), visible only as log
lines. Deleting rows from a closed TimescaleDB chunk does not shrink the disk
in any case: new rows only go to the newest chunk, so space freed in an old
chunk is never reused.

## Decisions

1. **Prune superseded status history continuously.** For a closed list of
   status concepts (the three above), delete every version that has a newer
   version of the same id and is older than one hour, in bounded statements,
   from the topology reconciler's leader every 10 minutes. The latest version
   of every id is never touched. Running continuously keeps the deletions in
   the current chunk, where PostgreSQL reuses the space. No archive: a
   superseded liveness ping records nothing a reader needs (precedent:
   `component/node/readiness_row_purge.go`, the moduleReadiness collapse).
   The 15 s registration cadence stays (`online` is derived from it, memql#4350).
2. **A scheduled firing that writes nothing leaves no journal.** A
   `schedule`-triggered run holds its journal the way a directly called logic
   already does (`heldWrites`): the run row is written at the run's first
   observed write, or when the run fails. A firing that completes without a
   write writes nothing. `@journalRequired` automations, adopted runs and every
   non-schedule trigger are unchanged.
3. **Retention statements are bounded by construction.** No statement in a
   retention path may approach the 8 s serving timeout. The work retention
   pass selects candidates one createdAt day window at a time with a small
   LIMIT. The log sweep, after archiving a UTC day, drops that day's chunk
   (`drop_chunks` with `newer_than`/`older_than`: instant, and it frees disk)
   and falls back to batched deletes only for rows outside a whole chunk.
4. **Per-tick log lines move to Debug.** The cluster guard's
   duplicate-prevented line, `event trigger fired`, `starting automation
   execution`, a successful `automation execution completed`, and the seed
   materializer's `global row written`. Failures keep their levels.
5. **A maintenance page instead of ingress-nginx's 503.** A `memql
   error-pages` subcommand (no database, no mesh) serves, with status 503 and
   `Retry-After`: the MemQL-branded page (mark, wordmark, "MemQL is being
   updated") for the platform's own hosts (the apex, `api`, `identity`,
   `mcp`, `os`, `vscode`), a neutral page with no MemQL branding ("This site
   will be back soon") for every other host (hosted sites, custom domains),
   and JSON when the client asks for JSON. Every front-door Ingress and every
   Ingress the engine creates for a custom domain carries
   `nginx.ingress.kubernetes.io/default-backend: error-pages`, which ingress-nginx
   uses when the routed Service has no ready endpoints. Upstream 502/503/504
   responses are NOT intercepted: the API returns some of them deliberately
   and their bodies matter.

## Not in this release (release 2)

A declared retention attribute on concepts (time-to-live and keep-latest),
storing the concept schema once instead of in every row, liveness outside the
versioned store, write-on-change for the other steady writers
(`stageInboundRequest`, `recordReputationWindow`, `reconcileMirroredDomains`,
`reconcileCustomDomains`, `magicLinkExpirySweep`, `pollPackageUpstreams`, the
seed materializer), and an owner-facing storage warning in the OS.

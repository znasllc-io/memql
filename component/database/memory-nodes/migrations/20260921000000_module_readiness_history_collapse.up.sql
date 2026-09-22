-- Collapse v1:platform:moduleReadiness to one version per row
-- (epic memql#5316 ruling D4, issue memql#5325).
--
-- WHAT THIS REMOVES. Every version of every readiness row except the newest.
-- The newest is each node's current verdict on one module and is exactly what
-- the fold reads; the ones behind it are restatements of a verdict that had not
-- changed, appended by a writer that had no unchanged-skip.
--
-- HOW MANY. 772k versions for a few dozen live ids on a production instance
-- (2026-09-13), which is what motivated the write-on-change rule now in
-- component/memql/readiness_write.go (readinessRewriteFloor). Every registration
-- heartbeat flush -- one per connected machine every 15 s -- made every node that
-- heard it re-evaluate and append all seven of its rows, at roughly 3.7 versions
-- a second cluster-wide. Write-on-change stopped PRODUCING them in the release
-- that shipped it; nothing has ever removed the ones already written, because
-- v1:platform:moduleReadiness is append-only and had no retention of any kind.
--
-- WHY COLLAPSE AND NOT DELETE EVERYTHING. D4 said "delete every
-- v1:platform:moduleReadiness row once. The next boot writes the new shape",
-- and the reason was D3's plan to change that shape to one ai--cluster row. D3
-- was replaced by S1 (docs/superpowers/specs/2026-09-14-readiness-convergence-design.md)
-- and per-node rows stayed, so the shape does not change and there is nothing
-- for a rewrite to correct. Keeping the newest version therefore costs nothing
-- and buys the absence of a window: delete the current verdicts too and every
-- module reads `unreported` -- "Not reported" on the desk, an open core gate --
-- from the moment this migration commits until each node's next pass. Honest,
-- but a flicker on every installation for no reason that still holds.
--
-- WHY A MIGRATION AND NOT A SWEEP. The rows are indistinguishable from each
-- other: there is no field, no provenance and no age band that separates the
-- ones a heartbeat produced from the ones a change produced, so a recurring
-- sweep would have nothing to key on beyond "not the newest" -- which is this
-- statement, and it only ever needs to run once per installation. The ongoing
-- mechanism is the two things that now exist: write-on-change, which stops the
-- churn at the source, and component/node/readiness_row_purge.go, which removes
-- a stopped node's rows outright.
--
-- SCOPED BY CONCEPT, NEVER BY KEY NAME. Nothing here reads a payload key at
-- all, which is one better than the rule memql#5199 earned: `state`, `module`
-- and `nodeId` are all names a product bundle mounted at MEMQL_DSL_PATH may
-- declare on a concept this repository has never seen.
--
-- NOTHING READS READINESS HISTORY. The one query over the concept is
-- moduleReadinessAll (dsl/platform/queries.memql), `asOf latest`; the OS folds
-- the same latest rows. An audit walk over the concept CAN return older
-- versions, and after this it returns one per node per module -- which is the
-- whole of what the concept ever had to say, with the restatements removed.
--
-- Idempotent: a second run matches nothing, because after the first there is
-- exactly one version per id.

DELETE FROM "MemoryNodes" m
USING (
  SELECT id, MAX("createdAt") AS newest
    FROM "MemoryNodes"
   WHERE concept = 'v1:platform:moduleReadiness'
   GROUP BY id
) AS latest
WHERE m.concept = 'v1:platform:moduleReadiness'
  AND m.id = latest.id
  AND m."createdAt" < latest.newest;

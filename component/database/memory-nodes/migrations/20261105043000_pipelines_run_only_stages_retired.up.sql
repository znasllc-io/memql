-- Retired concept field: v1:pipelines:run.onlyStages (release trains removed,
-- owner decision 2026-11-05; design record
-- docs/superpowers/specs/2026-11-05-owner-pushes-main-design.md, D2).
--
-- WHY A DATA MIGRATION AND NOT JUST A CONCEPT EDIT. Every concept's JSON
-- schema is `additionalProperties: false`, and a mutation's read-merge
-- validates the MERGED payload, stored keys included. A run row still carrying
-- `onlyStages` would refuse its next write -- the driver's claim, heartbeat and
-- conclusion all go through updatePipelineRun -- with
-- `additionalProperties 'onlyStages' not allowed`. A run a deploy opened and an
-- older replica left unfinished would then be stranded on the upgrade.
--
-- WHAT WROTE IT. Only the automatic deploy's verification run, which limited
-- itself to the manifest's verification stages. It was never configured in
-- production, so this usually matches nothing; the predicate makes it
-- idempotent either way.
--
-- SCOPED BY CONCEPT, NEVER BY KEY NAME, and every version of every run, because
-- a row's history is its versions (20260908010000_cluster_status_retired.up.sql
-- is the worked example).
--
-- The three concepts trains wrote (v1:pipelines:train, v1:pipelines:standing,
-- v1:pipelines:pipelineDeployment) need nothing here: nothing registers or
-- writes them any more, so their rows are inert history, recorded as waivers in
-- component/conceptfields/concept-fields.snapshot.json.

UPDATE "MemoryNodes"
SET payload = payload - 'onlyStages'
WHERE concept = 'v1:pipelines:run'
  AND payload ? 'onlyStages';

# Nightly release, stages 2 and 3: publishing and deploying by themselves

> **For agentic workers:** executed natively in the session that wrote it. Steps use checkbox syntax for tracking.

**Goal:** a train from `main` that lands goes on, when the owner has allowed it, to a published release and a deployed, verified production, and a failed deploy restores what ran before.

**Architecture:** the train row carries the release (`releasing`, `published`) one step per tick, through a seam to `integrations/release`, under the owner's standing grant. A deploy is its own row, `v1:pipelines:deployment`, with its own step per tick: pin on a branch, sync each ArgoCD Application at that exact commit, verify from outside, then merge the pin. Failure syncs each Application back to the commit it ran before. Nothing here runs until the owner presses a switch.

**Spec:** `docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md`, sections 6 and 7, which this plan changes (below).

## Decisions that change the spec

1. **The switches are the owner's, not settings.** `release` and `deploy` move out of `MEMQL_PIPELINES_TRAINS` into one owner-only row, `v1:pipelines:standing`, set by a button that captures the owner's authority (`auth.CaptureExecutionAuthority`). Every tick rebuilds that authority with `auth.ContextWithPersistedOwner`, which re-reads the stored role, so a demoted owner's automation stops. Reason: an instance variable can be written by people who may not approve a release, and the engine says background work "must never acquire owner authority just because its owner holds that role" without a captured grant.
2. **A deploy is a row of its own**, so a deploy never fights a train for the newest-train slot, and a button can deploy a release cut by hand.
3. **Sync at the pin commit, merge after verification.** `operation.sync.revision` names the pin commit on its branch; `targetRevision` stays `main`. A failed deploy syncs back to each Application's last synced commit, closes the pull request, and `main` never carried the bad pin. Rolling back therefore needs no CI, which may be what is broken.
4. **No automatic deploy starts while a train is unfinished, and no train departs while a deploy is moving**, because syncing the mesh restarts `agent`, which kills running CI.
5. **A release that changed tables or data does not roll back by itself**: old code on a new schema is not proven safe. It stops and mentions the owner, who pressed the button and is there.

## Global constraints

- Nothing changes until the owner presses a switch; every new setting key is absent in production.
- Unknown settings keys turn trains off everywhere, so the engine deploys before any key is written.
- Every step starts from a fresh read and ends in a compare-and-swap under a gate; no GitHub or cluster call under a gate.
- `prune` is never on. `prune: false` is written explicitly, because ArgoCD merges an omitted field from the previous operation.
- Run with `MEMQL_REQUIRE_DB=1` against a real database for anything touching rows; a fake store cannot prove a write.

## Review Focus

- A replica dies during publishing, or the sync restarts the pod running the tick: the next tick resumes from the row and never repeats a finished effect.
- An owner who loses the owner role: the next tick refuses, and the train holds with a reason rather than publishing.
- The pin's pull request check fails or never starts: production is untouched and the owner is told.
- A sync hanging on a Deployment that never becomes healthy: its operation is terminated at the deadline and the rollback runs.
- A window across midnight, and a deploy that is due while a train is running.

## Tasks

- [x] **1. Settings and pure decisions** (`component/pipelines/train.go`, `train_deploy.go`): `releaseProfile`, `deployment{repository,applications,namespace,verifyStages,window}`; `InDeployWindow`; `ClassifyMigration` over real migration text; `RemovedPipelineSteps`; `WorkerContracts`.
- [x] **2. Rows** (`dsl/pipelines`, `store_dsl.go`, `types.go`): train states `releasing`, `published` and release fields; concepts `standing`, `deployment`; run `onlyStages`; SDK regenerated. The standing shape never carries `authority`.
- [x] **3. A run of named stages** (`open.go`, `tree.go`): `Opening.Stages`, persisted, applied to the plan on whichever replica drives it. Refuses a name the manifest does not have for that event.
- [x] **4. ArgoCD sync** (`integrations/argocd/sync.go`): `ReadApplication`, `StartSync`, `ObserveSync`, `TerminateSync`; idempotent on a request id, fenced on uid and resourceVersion.
- [x] **5. Release side** (`integrations/release/train_release.go`, `build_prepare.go`, `pipelinerun/release_build.go`): `prepareBuildAt` selects the train head; `AdvanceTrainRelease` takes one step of the journey; internal origin only; approval under the standing grant.
- [x] **6. Coordinator: release** (`pipelinerun/train_release.go`, `train.go`, `train_land.go`, `pipelinenotify`): `releasing`, one re-run of failed release steps, `published`; a held release resumes when the owner finishes it by hand; `Start release now` retries a failed release.
- [x] **7. Pin** (`integrations/release/train_deploy.go`, `installation_overlay.go`): the seven-file pin through the existing overlay recipe without a receiver; the publication read from the signed catalog.
- [x] **8. Coordinator: deploy** (`pipelinerun/deploy.go`, `deploy_guards.go`): waiting, pinning, syncing, verifying, landing, deployed; rolling_back, verifying_rollback, rolled_back; stopped. Standing switch, Deploy button, Resume.
- [x] **9. Wiring** (`app/`): release integration to pipelinerun, cluster API for the sync.
- [x] **10. OS** (`clients/os/src/apps/cluster/releases`): switches, Deploy, Resume, what is happening now.
- [x] **11. Product** (`memql-znas`): evidence reuse admits a full train run for a release; RBAC for the two Applications; runbook.
- [x] **12. Two replicas over real Postgres** (`test/pipelinehop`): a release and a deploy resumed across replicas, and a rollback. Then the record, the public docs, one fresh review.

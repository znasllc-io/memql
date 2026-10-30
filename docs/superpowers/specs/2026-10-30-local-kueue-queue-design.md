---
title: Should a local cluster run production's pipeline queue?
audience: internal
status: draft
area: operate
sinceVersion: 0.31.9
owner: znas
---

# Should a local cluster run production's pipeline queue?

The owner wants every installation to get the deadline promoter (memql#6382), the
local k3d cluster included. The promoter relabels waiting Jobs in a Kueue queue, and a
local cluster has no Kueue: its steps wait for a slot under `count/jobs.batch: 2` before
a Job exists. This record is for the owner to decide on. Nothing here has been built.

## Recommendation

**Yes, with local's own sizing, once step 1 below has landed.**

Every local cluster gets production's controller, queue, flavor, strict order and
promoter. It keeps today's step requests and its limit of two steps at once, so nothing
that runs today stops fitting. It does not copy production's full reservations, which
would stop the engine's largest steps from ever starting at Docker's minimum size.

The reason is parity, not the promoter. The promoter first moves a step after 240 of
local's 480 minutes, and this Mac's local cluster has no pipeline rows at all after four
days, so it will rarely act. What local gains is the admission path: today only
production runs it, and nothing tests it through a real installer. A few hours on a
throwaway cluster found two defects, one of which production has now.

1. **Make the queue's Job webhooks fail open, and stop pulling on every start**
   (`deploy/kueue/install`). Do this whatever is decided: it closes production's
   exposure too.
2. **Release #6382 and move `DEFAULT_STACK_TAG`** to that release.
3. **Register the operator and local's AppProject in `up.sh`**, ahead of step 4, so
   clusters can gain the operator before their overlay asks for it.
4. **Compose the component in the local overlay** with the values below.

Rejected: leaving local as it is, because the queue stays untested outside production;
an opt-in flag or second overlay, because the parity rule refuses a second way to deploy
and the weekly install leg would not run it; and production's full reservations or
per-machine budgets, for the reasons under "Quotas and fit".

## What it costs

Measured 2026-10-30 on throwaway k3d clusters at the pinned versions (k3s v1.32.13, Argo
CD v2.13.3, Kueue v0.20.2), through Argo CD as `make up` does, on Apple silicon Docker
Desktop (14 CPUs, 26.87 GiB), from #6382's head `c95ac6fc1`.

| Stage, cold | Time |
|---|---|
| Argo CD ready | 59 s |
| cert-manager | 649 s, of which 629 s is Argo's first fetch of the repository, paid today |
| CloudNativePG | 40 s |
| **Kueue, new** | **68 s**, of which 47 s pulls a 64 MB image |

The editor's installer runs the same script (`k3d.up`), so it pays the same, inside its
5400-second step ceiling and 2400-second operator budget. Nothing in `editors/vscode`
changes but the pin.

**Memory:** the server container grows from 600 to 871 MiB on an otherwise empty
cluster. About 210 MiB of that stays with the controller stopped: the API server holding
12 large CRDs. The controller used 55 MiB after install and 89 MiB with 128 Jobs queued,
under its 512 MiB limit, at 5m CPU when idle; production's uses 115 MiB. It requests 500m
and 512 MiB on the serving node, where this Mac's running cluster has 13% of CPU and 9%
of memory requested. The promoter adds one short-lived pod every five minutes.

## A controller outage

The webhooks use `failurePolicy: Fail` on every Job in `memql-pipelines`, labelled or
not, so with the controller down the API server answers HTTP 500 `failed calling
webhook`. With the real runner and the controller held down for 65 seconds:

| Step | What happened |
|---|---|
| Running | Finished and succeeded. Its heartbeats were refused and only logged |
| Waiting, suspended | Stayed suspended, then ran and succeeded 19 s after the controller returned |
| New, on a replica with a recent isolation proof | **Failed in 0.4 s, `pipeline_execution_uncertain`**: the runner reads any 5xx on a create as "may have been created". The same attempt stayed failed after the controller returned, so a person has to re-run it |
| New, on a replica with no proof yet | **Refused in 2.7 s, `pipeline_isolation_unenforced`**, because the probe Job is refused too |

From the code, a runner taking over a Job would fail as well, because its claim is an
annotation. That case was not run.

The window is 12.7 s after the controller pod is deleted, 14.8 s after a scale to zero
(Argo's self-heal undoes it) and 5.5 s after the API server is ready following a cluster
restart. Freezing both nodes for three minutes, as a stand-in for laptop sleep, caused no
restart and no refusal.

**Offline it does not end.** Upstream says `imagePullPolicy: Always` and a cluster
restart makes a new pod, so with the registry unreachable the controller sat in
`ImagePullBackOff` and every new step was refused. With `IfNotPresent` it answered in
12 s, still offline. The image is pinned by digest, so nothing is lost.

**Production has the same exposure.** It holds 13 stranded creation claims (a step Secret
at `creating` with no Job) since 17 October, up to three in one second. That is the shape
this failure leaves, but other failures can leave it too, and they have not been
attributed.

**The fix** is two values in `deploy/kueue/install`, with no Go change: `failurePolicy:
Ignore` on `mjob.kb.io` and `vjob.kb.io`, and `imagePullPolicy: IfNotPresent`. It is safe
because the runner already creates every queued Job with `suspend: true`, so a Job that
misses the webhook still waits, and only the workbench's Role can create Jobs there.

Measured with both hooks on `Ignore` and the controller down for 45 seconds: new steps on
both kinds of replica created their Jobs, which stayed suspended with claims and
heartbeats current, and both succeeded 19 seconds after the controller returned. Teaching
the runner to wait on this 500 also worked for new steps, in about ten lines, but leaves
takeover claims exposed. Production gets the fix when it moves its Kueue pin, after the
opt-in tests `deploy/kueue/README.md` names.

## Quotas and fit

The largest engine step (`proving`: cache-prep, clone, a postgres sidecar, artifact-prep,
the step and the collector) is accounted at 8256 MiB and 40976 MiB of disk under the
component's rule that every container reserves its limit. Two fit the 17 GiB build node
here, at 92% of its memory, and scheduled and ran; a third waited.
`TestPipelinesSubstrate` passed through this queue end to end: sidecar, shards, artifacts,
cache, isolation, cleanup and cancellation (with the one-method stub described at the end).

So the shape works on this Mac, but not on smaller ones. The build node is two thirds of
Docker's memory, rounded down, and 15 of the engine's 66 cluster steps reserve 8 GiB or
more: 11 state 8 GiB themselves, and 4 are a default step plus a service (computed, not
measured).

| Docker memory | Build node | Those 15, full reservations | Those 15, today's requests |
|---|---|---|---|
| 8 GB, the minimum | 5g | Never admitted | The 4 fit; the 11 cannot be placed, as today |
| 12 GB, recommended | 7g or 8g | Never, or one at a time | The 4 fit; the 11 only at 8g, as today |
| 26.9 GiB, this Mac | 17g | Two at once, 92% of memory | All fit |

Two measured behaviours make that worse than slow:

- **A step over the budget blocks the queue.** With a 5 GiB budget, an 8 GiB step sat at
  the head and a 4 GiB step behind it stayed suspended, for up to the run's ceiling of
  eight hours. `BestEffortFIFO` let the small one through, but production chose strict
  order because backfill starved large steps.
- **A budget over the node fails steps that never ran.** Kueue admitted a step the node
  could not hold, and its pod stayed `Pending` until the Job's deadline passed.

So `pods` should be the only bound, with the other budgets too large to bind:

| Object | Local value | Why |
|---|---|---|
| `ClusterQueue` `pods` | `2` | The real concurrency bound, today's number |
| `ClusterQueue` `cpu`, `memory`, `ephemeral-storage` | `64`, `64Gi`, `256Gi` | More than two pods can request |
| `BackgroundCapacityPolicy` | The same, with `jobs: 2` | Holds the Cluster app to two as well |
| `LimitRange` | Unchanged: requests 250m, 512 MiB, 1 GiB; limits 2 CPUs, 4 GiB, 20 GiB | Today's numbers, still pinned by `pipelinesSizing` |
| `count/jobs.batch` | `32` | See "The Job quota" |
| `stopPolicy` | `None` | See "What else must change" |

Measured with these: the same step reserves 1088 MiB and 2064 MiB with its limits
unchanged, two run, and the rest wait with `insufficient unused quota for pods`.

This breaks the component README's rule that a request equals its limit, on purpose. That
rule makes a budget mean something where memory and disk bind. Here they do not, and the
exposure is what local has today: two steps, each under its limits, inside a capped node.
Local will not exercise a refusal by memory or disk; the opt-in tests in
`deploy/kueue/install` still can.

## The Job quota

**It can rise to 32.** Once Jobs are born suspended the count stops meaning concurrency,
and `pods` holds the line: with 128 Jobs queued, two ran. #6018 was six cold builds
writing at once. Writers stay at two, so that warning's cold-cache evidence is needed to
raise `pods`, not this count. Thirty-two is two overlapping runs at the driver's
`parallel(16)`; past that the existing slot wait still works.

Waiting also writes less. With 16 real steps waiting, the slot wait made 3.0 persisted
writes per second plus 1.4 refused creates; the queue made 1.65, all heartbeats.

**The one trap is the upgrade.** A workbench that has not restarted still creates
unlabelled Jobs, Kueue ignores those, and with 32 allowed they all start, which is #6018
again. The overlay must roll the workbench in the same sync, with a pod-template
annotation carrying the queue name, as production does for its priority classes.

## What else must change

- **`stopPolicy`.** The component ships `Hold`: everything stayed suspended with
  `ClusterQueue memql-background is inactive`. Local sets `None`. `up.sh`'s inline
  Application also needs production's `ignoreDifferences` (`stopPolicy`, the four quotas,
  the change annotation), or Argo undoes the Cluster app's saves.
- **AppProject.** The shared project refuses the component's eight cluster-scoped kinds.
  Do not widen it: cloud installs render none of them, and it would allow
  `ClusterRoleBinding` everywhere. Local applies its own manifest through `up.sh`'s
  `PROJECT_MANIFEST` default, outside `deploy/argocd/apps/` because `root.yaml` renders
  that directory on the cloud, and a test holds it to the shared file plus those kinds.
- **`up.sh`.** A third operator in `install_operator_stack`, with `_register_operator_app`
  taking a path, since `deploy/kueue/application.yaml` must stay out of `apps/`. That
  Application stayed Synced and Healthy as it is, on k3s 1.32.
- **Release-image legs.** `repair` and `round-trip` run a commit's manifests on
  `DEFAULT_STACK_TAG`, now v0.30.0, which has no `pipeline-deadline-priority`. From its
  source (not run), it would treat the word as no subcommand and boot a node in a 128 MiB
  pod, to be killed at 120 s. The legs would stay green, because Argo 2.13.3 has no
  CronJob health check and `wait_for_workloads` reads Deployments, while the promoter
  never works. `stackPin.ts` already says the first release carrying what a manifest needs
  moves the pin. The `upgrade` leg then tests this exact upgrade, and a minute fits the
  95-minute budgets.
- **Existing clusters.** A cluster following a branch fails its sync on the unknown kinds
  until its next `make up`, and the mesh keeps running on what it had. Landing step 3
  first narrows this, and the build node set the precedent.
- **Gates and docs.** `pipelinesSizing`'s ceiling; a narrow exemption in
  `TestNoClusterRoleBindingReachesTheEngineIdentity` for `memql-background-capacity`,
  whose ClusterRole names its three objects; the component tests, which render it over
  local a second time; `up_operator_revision_test.go` and `up_source_recovery_test.go`;
  the comments in `pipelines-values.yaml`; the component and `deploy/kueue` READMEs,
  `pipelines-substrate.md`, `reproduce-the-cloud-locally.md`, the decision map's "no
  overlay in this repository" paragraph, and the memory figure in
  `minimum-requirements.md`.

## Not verified

- The composed overlay reconciled by Argo, and a real `make up` or installer run with it.
  The objects were applied directly; production reconciles the same kinds on the same Argo
  version.
- The upgrade of an existing cluster, the promoter on a local cluster (#6382 verified it on
  a throwaway cluster), and the Cluster app's capacity section locally.
- Linux or amd64, a real VM sleep, and cold builds under the queue.
- Today's requests end to end: steps were admitted and reached Running, but
  `TestPipelinesSubstrate` ran at full reservations.

## Separate from this decision

`TestPipelinesSubstrate/fork-refused` panics on `main`: `forkRows` has no
`RunsForPipelineSHA`, which `open.go` now calls. So the weekly `pipelines` leg fails
whatever is decided here. With a one-method stub the whole test passed.

On an Apple silicon Mac the engine's own package cannot run on the cluster: it declares
`linux/amd64` and the build node is arm64. Such a step holds a slot until its timeout,
today and under the queue alike.

## Decision needed

Yes or no, from the owner. On yes, step 1 goes first as its own change. On no, still do
step 1 and fix the stub.

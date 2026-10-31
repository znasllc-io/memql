# Bounded pipeline capacity

Optional resource admission for the existing dedicated build pool. The Cluster
app exposes reservations and lets a cluster owner change the active budget
within a separate operator-owned typed capacity policy. Neither control changes
machine size, node count or autoscaler limits.

Jobs enter suspended, with their complete memory and temporary-disk
reservations, including service and artifact containers. Execution timeout
starts at admission. The absolute run ceiling still bounds waiting, and the
runner retires expired attempts before removing their Jobs. CPU is burstable
within the dedicated pool; the CPU budget is reserved processing capacity,
not a cap on instantaneous utilization. This does not move ordinary Workbench
commands out of their service pods.

**No overlay in this repository composes this component.** An installation does,
above its instance overlay. That includes the local overlay: a k3d cluster has no
Kueue and no queue, and its steps wait for a slot under `count/jobs.batch` before a
Job exists. `component_test.go` renders the component over all three overlays
regardless, so what an installation composes is what the gates checked.

## Installation order

1. Install and qualify the pinned operator in `../../../kueue` through ArgoCD.
2. Release the matching agent and workbench binaries, with step protocol V10
   and status protocol V2. Keep the existing Job quota during the roll. The
   workbench image must also carry `memql pipeline-deadline-priority`, which this
   component runs: moving this component ahead of that image fails every pass.
3. Keep admission at the initial `Hold` setting while rolling all agents and
   workbenches. The existing Job-count quota stays in place; this component
   deliberately does not raise it while old runners might still create Jobs.
4. Compose this component after `pipelines`, in the same layer as the `images:`
   block that pins the workbench, set the four operator ceilings
   and initial quotas from allocatable resources after system reservations,
   and ensure every LimitRange memory/disk defaultRequest equals its limit.
   Overlay patches run after component patches and must preserve that equality.
5. Render and verify cluster-scoped queues/flavors/priority classes have no namespace. The
   included OpenAPI paths teach Kustomize their scope; the capacity policy also remains cluster-scoped.
6. The mesh AppProject must allow the queue/flavor, priority-class, capacity-policy, admission-policy and
   capacity RBAC kinds. An installation releasing through the native receiver also
   binds the promoter's image slot in its `installation.json` (`batch`, `CronJob`,
   `pipeline-deadline-priority`, field `containers`, container `promote`, the
   workbench's component and artifact), or that release is refused naming the slot.
   Delegate only this ClusterQueue's four `nominalQuota`
   fields, `/spec/stopPolicy` and the `memql.io/capacity-change` annotation via
   Argo `ignoreDifferences`, with `RespectIgnoreDifferences=true`. Keep pool
   selection, namespace selection, preemption and the ceiling reconciled.
7. Verify every agent and workbench is on the qualified queue-aware release,
   and that no unlabelled pipeline Job remains. Only then raise the retained
   Job-count quota (for example to 128, which counts suspended jobs as well),
   and let the owner switch admission on.
8. Verify admission, queue waiting past a step timeout, cancellation, expiry,
   replica recovery, over-ceiling rejection and queue-controller outages
   locally before enabling production. Check the rendered LimitRange and
   queue against the actual pool, including native sidecars and system Pods.

The capacity policy and ValidatingAdmissionPolicy are not writable by the
engine account. Kubernetes independently checks each budget change, including
a policy change racing the UI. Saves compare the queue UID and resource version
and are never automatically retried after an uncertain write.

Lowering a budget does not stop running work. New work waits until reservations
fit. `Hold` pauses admission without evicting admitted jobs. Unknown controller
measurements are shown as unavailable rather than zero. A smaller budget can
leave a large job waiting until the owner increases it or its run expires.

## Moving waiting steps forward

The queue admits by class first and age second, and a run's ceiling counts its
queue time. Left alone, a step of a low class, or one created late in an old run,
can wait out the whole ceiling and fail `pipeline_run_ceiling` without having
started. So every five minutes `CronJob/pipeline-deadline-priority` relabels each
suspended step Job by how close its run is to its deadline:

| When | Class | Value | So that |
|---|---|---|---|
| Past half the run's ceiling | `memql-pipeline-aged` | 350 | Old runs go before younger work of any event |
| The owner named the run | `memql-pipeline-boosted` | 375 | That run goes before old ones |
| In the last quarter of the ceiling | `memql-pipeline-expiring` | 400 | A run about to expire goes before everything |

A Job only moves up, and only while it is suspended. A pass never touches admitted
work, a Job past its deadline (the runner fails those), the ceiling, or any other
queue. Each promotion records `memql.io/priority-promoted-from`, `-at` and `-reason`
on the Job. The queue is `StrictFIFO` for this reason and the older one in
`queue.yaml`.

The classes a step starts at stay the installation's own
(`MEMQL_PIPELINES_QUEUE_PRIORITY_CLASSES`), and **every one must rank below 350**: a
pass cannot read a class's value, so a Job started above these would be relabelled
down to them.

### Turning it off, setting its windows, naming a run

All three are patches in the layer that composes this component.

```yaml
patches:
  - target: { kind: CronJob, name: pipeline-deadline-priority }
    patch: |-
      # Off. Every Job already promoted keeps its class.
      - { op: add, path: /spec/suspend, value: true }
      # Fixed windows, instead of a quarter and a half of each run's own ceiling.
      - { op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --window-minutes=180 }
      - { op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --aged-window-minutes=360 }
      # A run to go first, by its pipeline run id. Repeat for more.
      - { op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --boost-run=79bb3043f59bf98ed9c0898cb74d823f }
```

`--aged-window-minutes=0` turns the aged tier off, keeping event classes in force
until a run nears expiry. The windows need no stating as a rule: they follow the
ceiling each Job's run was started under, which the runner writes on the Job
(`memql.io/run-ceiling-minutes`). A boost names a run, not a pull request, so a re-run
needs its own entry. Remove an id once its run finishes; a stale one costs nothing,
and a Job it moved keeps its class.

To pin the CronJob to a pool, add a `nodeSelector` the same way. It needs none to stay
off the build pool, whose taint it does not tolerate.

### Operating it

`kubectl -n memql logs job/<latest pipeline-deadline-priority-*>` prints the pass: what
moved, what did not and why (`not-due`, `not-waiting`, `already-promoted`, `expired`,
`other-queue`, `no-deadline`, `no-ceiling`, `changed-since-list`), and any patch refused.
A refused patch fails the Job and leaves the others promoted. To run a pass now:

```bash
kubectl -n memql create job --from=cronjob/pipeline-deadline-priority pipeline-deadline-priority-now
```

From a checkout, without writing anything, through your own credentials:

```bash
kubectl proxy --port=8001
```

```bash
go run . pipeline-deadline-priority --api=http://127.0.0.1:8001 --dry-run
```

Known limits: a step created late in an old run starts at its event's class and waits
up to five minutes for its first promotion. Promotion reorders one queue and cannot
make room it does not have: when more urgent work waits than the pool can run in time,
the oldest goes first and the rest can still expire. A step larger than the queue's
budget blocks a strict queue from its head until it expires, and promotion can carry
one there. Why it is built this way, and what has been verified:
[the decision map](../../../../docs/internal/ops/harness-decisions.md#waiting-pipeline-steps-move-forward-as-their-run-ages-2026-10-29).

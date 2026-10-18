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

## Installation order

1. Install and qualify the pinned operator in `../../../kueue` through ArgoCD.
2. Release the matching agent and workbench binaries, with step protocol V9
   and status protocol V2. Keep the existing Job quota during the roll.
3. Keep admission at the initial `Hold` setting while rolling all agents and
   workbenches. The existing Job-count quota stays in place; this component
   deliberately does not raise it while old runners might still create Jobs.
4. Compose this component after `pipelines`, set the four operator ceilings
   and initial quotas from allocatable resources after system reservations,
   and ensure every LimitRange memory/disk defaultRequest equals its limit.
   Overlay patches run after component patches and must preserve that equality.
5. Render and verify cluster-scoped queues/flavors have no namespace. The
   included OpenAPI paths teach Kustomize their scope; the capacity policy also remains cluster-scoped.
6. The mesh AppProject must allow the queue/flavor, capacity-policy, admission-policy and
   capacity RBAC kinds. Delegate only this ClusterQueue's four `nominalQuota`
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

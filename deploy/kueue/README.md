# Background admission controller

This optional operator is reconciled through `application.yaml`, using the
same `install/` manifests on every target. It does not provision machines.
The pinned upstream Kueue v0.20.2 manifest is distributed under
`install/LICENSE.kueue`; its source and checksum are in the kustomization.

Only labelled batch Jobs are managed. Job and Workload webhooks are restricted
to `memql-pipelines`; a controller outage must not block customer Deployments,
Pods or Jobs in other namespaces. There is no eviction/requeue timeout and no
preemption. An admitted command must not be replayed to improve utilization.
Server-side apply is required for the large CRDs.

A controller outage costs a step time and nothing else. The two hooks on Jobs fail
open, so a new step still creates its Job, which stays suspended until the controller
returns, and a running step finishes. The image is pinned by digest and started
`IfNotPresent`, so the controller restarts with no registry. To prove it against the
real runner on a disposable cluster, with no Argo CD self-heal on this operator,
because the test scales the controller to zero:

```bash
MEMQL_CAPACITY_TEST_CONTEXT=k3d-<cluster> MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT=k3d-<cluster> go test -count=1 -run TestNewStepsWaitThroughAControllerOutage ./integrations/pipelinesteps/
```

Install and verify this operator before enabling
`../k8s/components/pipelines-capacity`. Removing it while that component is
active leaves new work suspended. Its Application deliberately does not prune
CRDs: deleting a CRD deletes its queue and workload records.

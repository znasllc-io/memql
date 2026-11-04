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

**The local cluster runs it** (memql#6387). `make up` registers `application.yaml`
after cert-manager and CloudNativePG and waits for the controller
(`scripts/k3d/up.sh`, `install_operator_stack`), because the local overlay composes
`pipelines-capacity`. The file stays here rather than in `../argocd/apps/`, which the
cloud's app-of-apps renders: a cloud installation registers it from its own
repository. A cluster brought up before this gets the operator at its next `make up`.
Measured on k3d (2026-10-30): 68 s on a cold bring-up, 47 s of it the image pull, and
about 270 MiB, most of it the API server holding the large CRDs.

That component's promoter (`memql pipeline-deadline-priority`) stands on a behaviour
of this controller: a workload that has not reserved quota is re-prioritized when its
Job's priority-class label changes, a Job that started with no class included. Before
moving the pin, run the opt-in proofs against a disposable k3d cluster with this
operator installed, a node labelled and tainted `memql.io/pipeline-pool=builds`, and
`memql-workbench:local` imported:

```bash
MEMQL_CAPACITY_TEST_CONTEXT=k3d-<cluster> go test -count=1 ./deploy/kueue/install/
```

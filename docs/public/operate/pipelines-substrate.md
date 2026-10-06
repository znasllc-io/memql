---
title: Pipelines substrate -- where a pipeline's steps run, and what an operator sets up
audience: public
status: stable
area: operate
sinceVersion: 0.25.0
owner: znas
---

# Pipelines substrate

**Audience:** operators running the cluster a pipeline's steps run on, and owners whose steps run on their own machines.
**Design:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md`, decisions D10 and D11 and section 4's epic 3 (epic memql#5478).

[Pipelines](pipelines.md) is the seam: the manifest's `pipeline:` block, how a
run is chosen and planned, its secrets, and the check run it reports on GitHub.
This page starts where the seam stops, at the moment a compiled step is handed
over to be run: where it runs, what it may reach, what becomes of its output and
its files, how it ends, how long its records stay, and what an operator sets up
before the first one.

> **On AKS as `scripts/deploy/azure-provision.sh` creates it, no step runs.**
> The script enables no network policy engine, so the runner cannot prove the
> steps' namespace isolated, and it refuses every step
> `pipeline_isolation_unenforced` until an operator enables one
> ([The isolation proof](#the-isolation-proof)). A local cluster (k3s) enforces
> network policy as installed. Read [Owner actions](#owner-actions) before the
> first run.

---

## Where a step runs

The agent node driving a run hands each command step to the substrate's
executor (`integrations/pipelinesteps`), which decides where it runs:

- **On the cluster**, for a container step placed on the cluster: a Kubernetes Job in the
  `memql-pipelines` namespace. The agent forwards the step over the node mesh
  to a workbench replica, whose runner creates the Job, watches it, captures its
  output, files its log and artifacts and records its outcome. Every name the
  runner uses is derived from the run, the step and the attempt, so whichever
  replica is asked next finds the same Job.
- **On one of the owner's own machines**, for `placement: fleet`, native execution, or a step that names a host need --
  `display`, `docker`, `gpu`, `macos_tooling` or `user_files`, which no step pod
  offers -- and only on a pipeline connected with `compute: cluster_and_fleet`
  ([Compute](pipelines.md#compute)). The agent dispatches the step to a machine
  through its worker dispatcher ([The fleet](#the-fleet)).

Both paths give a step the same contract environment, the same secrets and the
same effective timeout, capture its output the same way, and file its log and
artifacts in the owner's Library the same way. A notify stage's step is the driver's own and never
reaches the executor.

| Node | What it does for a step |
|---|---|
| agent | Drives the run (the seam's driver) and holds the executor: refuses what it must not route, computes the step's effective timeout, forwards a cluster step and watches for the answer, dispatches a fleet step and files that step's log and artifacts |
| workbench | Holds the runner, the only MemQL process that creates, watches and deletes a step's Job and Secret -- through the API server, as the dedicated identity `memql-engine-workbench`, whose Role reaches nothing outside `memql-pipelines` |
| the step's pod | Not a MemQL node. It holds no cluster credential |

**What each node needs.** An agent node needs a route to a workbench replica
(`MEMQL_WORKBENCH_REMOTE`, which the base sets) for cluster steps, and its worker
dispatcher for fleet steps; without one, the steps that need it fail
`pipeline_runner_unavailable`, naming the missing half. A workbench node runs
steps when it has both a clone image (`MEMQL_PIPELINES_CLONE_IMAGE`, which the
pipelines component's ConfigMap sets) and an in-cluster API server. Without
either it says once, at Info, what is missing, and answers every step it is sent
`pipelines_not_configured`, which the agent reports as
`pipeline_runner_unavailable`.

The Kubernetes objects are the `deploy/k8s/components/pipelines` component --
the namespace, the runner's Role, the step's ServiceAccount, the cache claim,
the ceiling and limits, the network policy and the workbench's ConfigMap --
composed by the `local`, `cloud` and `cloud-entry` overlays. Tenant overlays do
not compose it: `memql-pipelines` is one namespace per cluster. The component's
[README](https://github.com/znasllc-io/memql/blob/main/deploy/k8s/components/pipelines/README.md)
says why each object is shaped as it is.

---

## The step's Job

### The pod

A step's pod runs up to four kinds of container, in this order:

| Container | Kind | Image, user | What it sees |
|---|---|---|---|
| `cache-prep` | init, only when the step declares `caches` | the clone image, root | the cache claim's root, to open the step's directory under it. A fixed script: no git, no token, no checkout |
| `clone` | init | the clone image, root | the checkout volume and the clone token; never the cache |
| `svc-<name>` | native sidecar, one per service the step names, in name order | the service's `image`, its own user | its declared `env` only: no checkout, no cache, no secret |
| `step` | main | the pipeline's `image`, its own user | the checkout at `/workspace`, its repository's cache at `/cache`, the step's environment and its secrets |

Every container runs with `allowPrivilegeEscalation: false`, and the pod with
the `RuntimeDefault` seccomp profile, `restartPolicy: Never` and a 10-second
termination grace (the step's wrapper is PID 1 and does not forward a SIGTERM).
No container keeps the container runtime's `NET_RAW`, with which a root process
on a bridge network can forge ARP replies and read the traffic of the pods
beside it -- something no network policy governs. `cache-prep`, `clone` and the
isolation probe run fixed scripts that need no capability at all, and drop every
one; the step and each service drop `NET_RAW` alone, because their images'
entrypoints may use the rest of the runtime's set (postgres's changes its data
directory's permissions and switches to its own user, and fails without them).
The pod runs as the ServiceAccount `memql-pipelines-step`, which nothing is bound
to and whose token is never mounted, with Service links off: no cluster
credential is in it, and the only Secret it can name is its own. The namespace
enforces Pod Security `baseline` -- a privileged, host-path, host-network or
host-PID pod is refused at admission -- and warns at `restricted`, which a
typical CI image running as root would fail.

The Job is created with `backoffLimit: 0` -- Kubernetes never retries a step --
with what is left of the step's time as its `activeDeadlineSeconds`
([Time](#time)), and with a `ttlSecondsAfterFinished` of 30 minutes as a
backstop: the agent requests deletion after durably journaling the step's
outcome, then confirms its Job, Secret and pods are absent.

### The checkout

`clone` makes `/workspace` a repository and fetches exactly the commit under
test -- `git fetch --depth=1 <clone URL> <SHA>`, then `git checkout FETCH_HEAD`
-- so the step sees one commit, a detached head, no branch and no remote. A tool
that wants history or a base commit finds neither.

- **The token** is minted for the step on the workbench: a short-lived token of
  the GitHub App installation the pipeline runs under, narrowed to the one
  repository and to contents read. It reaches the clone container alone,
  through the step's Secret, and git receives it as an `http.extraheader` for
  the one fetch -- never in the URL, the repository's config or the log, and the
  step's output is masked for it. A token older than 30 minutes by the time the
  Job is created (after a long wait for a slot) is minted again. With no
  installation to mint under (installation `0`), the clone is anonymous, which
  only a public repository allows.
- **The clone URL** is `https://github.com/<owner>/<name>.git`. A URL that is
  not plain `https`, or that carries a user, a query or a fragment, refuses the
  step `pipeline_job_rejected` before anything is created.
- **The checkout is world-writable.** The clone runs as root and clears its
  umask, so the step, running as its image's own user, can write the files it
  checked out. A tool that refuses a configuration file others can write needs
  `chmod go-w` on that file in the step ([Known limitations](#known-limitations)).
- The clone's last 200 lines are kept at the end of the step's archived log.

### Services

A service the step names runs as a native sidecar (an init container with
`restartPolicy: Always`), and the step reaches it on `localhost`: the pod has one
network. Sidecars start in name order, and the kubelet starts the next container
only once a sidecar's startup probe -- the service's `ready` command, run every 2
seconds -- has passed. A service whose check has not passed after 90 tries is
restarted, and a service that stopped even once before the step started -- it
exited, or its check kept failing -- fails the step `pipeline_service_failed`.
Once the step ends the kubelet stops every service, which is not a failure. For
a failed step, each service's last 50 lines are kept at the end of the step's
archived log.

The step's command, its environment values, a service's `env` values and its
`ready` check reach their container as written: every `$` is escaped, so the
kubelet expands no `$(NAME)` and turns no `$$` into `$`.

### Caches

`caches:` names `go`, `npm` or both, the two the runner knows; any other name
fails the step `pipeline_job_rejected`. A step that declares a cache mounts one
directory of the shared claim `memql-pipelines-cache` at `/cache`, and only
that one: its owner's, its repository's, and its run's trust's
(`owners/<owner>/repos/<repository>/<trust>`: 24 hex derived from the owner's
id, 24 hex derived from the repository's owner and name, and the trust). A step
can rewrite any entry of the cache it is given, and Go trusts its build cache by
its own key, so whoever writes a cache decides what a later build that reads it
gets. Hence three walls:

- **Two owners never share a cache.**
- **Two repositories of one owner never share one.**
- **A pull request never writes what the default branch reads.** A run opened
  for a push to the default branch, the merge queue or a published release is
  `trusted`; a pull request's run is `untrusted`; each mounts its own half, so a
  collaborator's unmerged change cannot plant a build the merge queue then
  uses. A re-run keeps its original's trust: it is opened with the original's
  event.

Every step of one repository at one trust shares its cache, so the second run
starts warm; the price is one cold cache per repository and trust.
`cache-prep` creates the step's directory, world-writable and sticky (`1777`),
before the clone runs, and the step's wrapper points each declared cache at a
tree of the step's own uid:

| Cache | Variables the wrapper sets |
|---|---|
| `go` | `GOMODCACHE=/cache/go-u<uid>/mod`, `GOCACHE=/cache/go-u<uid>/build` |
| `npm` | `npm_config_cache=/cache/npm-u<uid>` |

A step whose image has no `id` command runs uncached rather than guess a uid. A
claim whose storage class does not exist -- on AKS, a cluster without the Blob
CSI driver -- stays Pending, and a step that declares a cache cannot be
scheduled: `pipeline_job_unschedulable` after 10 minutes. A claim that is bound
but cannot be mounted shows as `cache-prep` never starting (the pod's events
name the mount error), and fails the step `pipeline_job_rejected` after 10
minutes. Caches are the cluster's alone: a fleet step uses its machine's own.

### The environment

The step's command sees what every runner exports -- the `MEMQL_*` contract in
[What a step's command sees](pipelines.md#what-a-steps-command-sees) -- and the
step's allowed secrets, each under its own name. That is exactly the map the
seam renders for the step, split in two: the contract as plain values, and each
secret by reference to the step's own Secret, which the step container reads one
key at a time.

The substrate sets a few variables of its own, which are not part of the
contract and which a step must not rely on: the wrapper's
(`MEMQL_STEP_COMMAND`, `MEMQL_STEP_ARTIFACTS`, `MEMQL_ARTIFACT_MARKER`,
`MEMQL_CACHES`), git's `safe.directory` for `/workspace` (`GIT_CONFIG_COUNT`,
`GIT_CONFIG_KEY_0`, `GIT_CONFIG_VALUE_0`, because the checkout belongs to
another uid), and the cache paths above. A secret named like one of them, or
`GIT_TOKEN`, fails the step `pipeline_job_rejected`.

No step pod has a Docker daemon. A step that builds or runs containers names the
`docker` need and runs on a machine of the owner's.

### Time

A step runs under its effective timeout: the lesser of its own (`timeout:`, 20
minutes when it names none, at most 2 hours) and what is left of its run's
wall-clock ceiling (`MEMQL_PIPELINES_RUN_MAX_MINUTES`, 120 minutes by default,
counted from when the run started) when its Job is created. Whichever bound it
is names the failure,
`pipeline_step_timeout` or `pipeline_run_ceiling`, and a step whose run has less
than a second of its ceiling left is not started (`pipeline_run_ceiling`).

**Time queued is the run's, not the step's.** A step's own timeout counts from
its Job's creation. Before that -- waiting for a free slot under the ceiling,
for a forward to reach a runner, or for the isolation proof -- only the run's
ceiling bounds the wait, so a stage wider than the ceiling runs every step, a
slot at a time, for as long as the run's ceiling allows. The Job is created
with the lesser of the step's timeout and what is left of the run's ceiling as
its `activeDeadlineSeconds`. A step still waiting when the run reaches its
ceiling fails `pipeline_run_ceiling` without ever starting, and its message says
what it was waiting for.

A step stopped at its deadline reports exit code -1, whatever its command did as
it was stopped; a step whose command ended before the deadline keeps its own
answer.

### Steps at once, and their size

The namespace's ResourceQuota counts Jobs (`count/jobs.batch`): one Job is one
running step, and the API server counts it atomically across every workbench
replica. A step that finds the quota full waits, says so once in its log
(`memql: waiting for a free slot under the pipelines ceiling`), and tries again
every 10 seconds until a slot frees or its run reaches its ceiling. A finished
Job counts until the agent has durably committed its work-step receipt and
acknowledges it. Returning an execution result does not delete the evidence.
The agent sends that acknowledgement up to 5 times within a 30-second budget,
waiting twice as long before each send. The runner confirms the Job, Secret and
pods are absent; an accepted delete with a terminating pod is not enough.
Jobs are deleted with foreground propagation, retaining their quota slots
until dependent pods are gone. Isolation probes confirm their own cleanup
before returning a successful verdict or reusing the same probe name.
Unconfirmed cleanup leaves the run unfinished for recovery. A replacement
driver retries cleanup from the committed receipt without executing the command
again. The Job's 30-minute TTL remains a fallback, not evidence of deletion.

The LimitRange is the one place a step's size is decided: the runner sets no
resources, and every container of the pod -- `cache-prep`, `clone` and each
service as well as the step -- gets the namespace's default, so a step's request
multiplies with its services.

| Overlay value | `local` | `cloud` | `cloud-entry` |
|---|---|---|---|
| Steps at once | 1 | 2 | 1 |
| Each container's request | 250m CPU, 512Mi, 1Gi of disk | 250m CPU, 512Mi, 1Gi of disk | 250m CPU, 512Mi, 1Gi of disk |
| Each container's limit | 2 CPU, 4Gi, 20Gi of disk | 2 CPU, 4Gi, 8Gi of disk | 2 CPU, 4Gi, 8Gi of disk |
| The workspace's size limit | 20Gi | 8Gi | 8Gi |
| Cache claim | 20Gi, `ReadWriteOnce`, the cluster's default class | 100Gi, `ReadWriteMany`, `azureblob-nfs-premium` | 50Gi, `ReadWriteMany`, `azureblob-nfs-premium` |

Set `MEMQL_PIPELINES_NODE_POOL` on every workbench replica to reserve a build
pool. For a value of `builds`, label the chosen nodes
`memql.io/pipeline-pool=builds` and taint them with
`memql.io/pipeline-pool=builds:NoSchedule`. Step Jobs and isolation probes both
select that pool and tolerate exactly that taint. The step's Linux architecture
constraint still applies. A missing matching node leaves work waiting; it does
not send the build to serving nodes. A malformed pool value refuses execution.
The unset setting preserves Linux placement without a pool constraint.

Provision the nodes before enabling this setting. An existing local-path cache
remains bound to its original node; changing placement does not migrate it.
All local k3d nodes share one Docker VM's CPU, memory and disk. The local
one-Job ceiling applies across Workbench replicas; additional virtual nodes
do not increase physical capacity. An isolation probe on a selected pool does
not establish enforcement on every node in that pool.

**Disk** is ephemeral storage. A container's own files outside any volume, and
its logs, count against its limit; every emptyDir counts against the sum of the
pod's containers' limits, the workspace among them; and the workspace has a
size limit of its own, the same number, which the Job shows
(`MEMQL_PIPELINES_WORKSPACE_LIMIT`, set by the pipelines ConfigMap beside the
LimitRange). The kubelet evicts a pod past any of them, and the step fails
`pipeline_step_disk_exceeded`, quoting the kubelet. A declared cache is the
claim's storage and counts against none of them; on a local cluster the
`local-path` class keeps that claim on a node's disk and does not enforce its
size.

**The cloud disk values are sized for the 32 GiB OS disk** `azure-provision.sh`
gives each node (`--node-osdisk-size 32`). That disk holds the OS, every image
and the mesh as well as the steps' ephemeral storage, and at the ceiling both of
`cloud`'s steps can land on one node, so 8Gi a container is sized for two steps
beside the rest. A step's pod is bounded by the sum of its containers' limits:
a step with no services is held to 8Gi in all, and each service it names adds
another 8Gi to that sum, while its workspace stays within its own 8Gi. The
kubelet enforces these by looking, not by refusing a write: it measures usage
periodically (a volume's about once a minute), so a step can write past its
bound for up to about a minute before it is evicted. An
operator who provisions nodes with larger disks raises both values -- the
LimitRange's default ephemeral-storage limit and
`MEMQL_PIPELINES_WORKSPACE_LIMIT` -- in their own overlay; the render gate
(`render_pipelines_test.go`) keeps the two equal. A local cluster, on a
workstation's disk, keeps 20Gi.

The cloud values are sized for the default node pool `azure-provision.sh`
creates (2 x Standard_D2as_v4) with the mesh already on it. An instance that runs
a real CI load raises the ceiling and the requests in its own overlay, together
with a dedicated node pool for the steps: raising them alone leaves steps
Pending until their scheduling timeout. A step's pod that cannot be scheduled
within 10 minutes fails `pipeline_job_unschedulable`.

### What a step can reach

The network policy `memql-pipelines-isolate` selects every pod in the namespace:

- **In: nothing.** No rule admits a connection to a step or its services. A step
  reaches its own services on `localhost`, which no network policy governs.
- **Out: cluster DNS and the internet.** UDP and TCP port 53 into `kube-system`,
  and `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `169.254.0.0/16` and `168.63.129.16/32`. A step can fetch from GitHub, a
  module proxy or a package registry. It cannot reach the mesh, the database,
  another pod, a node, the cloud's instance-metadata endpoint
  (`169.254.169.254`) or Azure's WireServer (`168.63.129.16`, a public address
  that on AKS serves a node's goal state and provisioning material; a step
  resolves names through cluster DNS and needs nothing from it). An API server
  with a public endpoint is on the internet like any other host; the step holds
  no credential for it.

A network policy is enforced by the cluster's network policy engine, not by the
object, which is why [the isolation proof](#the-isolation-proof) runs before any
step does.

### Workbench identity cutover

Only `memql-engine-workbench` holds the pipeline runner Role. The other engine
accounts cannot create pipeline Jobs or read step Secrets. Before switching a
cloud workbench to this account, prepare the exact new subject in its external
trusts: [OpenAI mapping](auth/openai-federation.md#the-cutover),
[Anthropic prefix](auth/anthropic-federation.md#separate-kubernetes-identities),
and any instance-specific Azure federation. Keep the existing engine subject
for the other nodes. Verify both subjects, then roll out and verify the new
workbench before allowing builds. Local RBAC verification does not establish
that a cloud provider has accepted its token.

### Reading a step on the cluster

Each Job and its Secret carry:

| On | Key | Value |
|---|---|---|
| label | `app.kubernetes.io/managed-by` | `memql-workbench` |
| label | `memql.io/pipelines-run` | the `v1:pipelines:run` id (one that is not a legal label value becomes `r-` and 24 hex) |
| label | `memql.io/attempt` | the step's attempt |
| annotation | `memql.io/step-key` | the step's key, `<stage>.<step>`, with `#<i>` for a shard |
| annotation | `memql.io/work-run` | the `v1:work:run` id |
| annotation | `memql.io/owner-user` | the pipeline's owner |
| annotation | `memql.io/run-deadline` | the agent's absolute run deadline in UTC; authoritative for orphan-Secret retention |

The Job's name is `mp-` and 24 hex, derived from the run, the step and the
attempt; its Secret is the same name with `-env`. The runner's own annotations
on the Job say which replica holds it and when it last said so
(`memql.io/runner`, stamped every 10 seconds), how far its log was captured
(`memql.io/log-cursor`), and, once the step has ended, how it ended
(`memql.io/observation`) and its outcome (`memql.io/outcome`). The runner logs
under `component=pipelines.runner`, with the run, the step and the Job's name.

```bash
kubectl get jobs -n memql-pipelines -l memql.io/pipelines-run=<run id>
kubectl logs -n memql-pipelines job/<job name> -c step --follow
```

A Job lasts only until the agent holds its outcome. A Secret no Job came to own
-- its runner went before the Job it made owned it -- is deleted by any
replica's orphan sweep (at startup and every 10 minutes, even with no builds)
once its Job is gone and the stamped run deadline plus the Job TTL has passed.
A different workbench ceiling cannot shorten that retention. Legacy Secrets
without a valid deadline use their creation time plus the sweeping workbench's
run ceiling and Job TTL as a bounded fallback.

Each cleanup batch has time, page and deletion limits. It retains an unfinished
page and pagination position, continuing promptly until the scan is complete;
an expired API snapshot starts a fresh scan. Shutdown cancels in-flight API
calls. Deletion includes the observed Secret's UID and resource version, so a
replacement or newly assigned owner invalidates a stale cleanup decision.
Uncertain API responses retain resources for a later pass. This maintenance
only runs where the Kubernetes pipeline runner is configured and creates no
build capacity.

---

## The isolation proof

Before the first step it creates, each workbench replica proves that
`memql-pipelines` is isolated -- that a pod there cannot open a connection to
another pod -- and it creates no step until the proof passes.

- **The probe** is one Indexed Job in the namespace: index 0 listens, index 1
  tests the egress restriction, and index 2 is a positive control. The listener
  admits both connectors through the same ingress rule; only index 2 has a
  narrow egress exception to that listener on TCP 8080. Ordinary steps carry
  no probe label and gain no exception. It takes one slot of the ceiling, so it waits for room as a step
  does, bounded by the creating step's run's ceiling.
- **After a 5-second settle** -- a new pod's egress can be open for its first
  second or two while the policy engine programs it -- the connector makes three
  attempts one second apart, each at the cluster's DNS, which the policy allows,
  and at the listener.
- **The verdict.** Connected on any attempt: not isolated. Isolated requires all of these:
  - every listener attempt was refused or timed out;
  - DNS answered every attempt;
  - the positive control reached the same listener on every attempt;
  - the listener held throughout.

  The listener has held when, read again once the connector has ended, all of
  this is true:
  - it is the same pod, matched by its UID, so a replacement with the same name
    is caught;
  - it is not being deleted, not marked for disruption, and not ended;
  - its container is running and Ready, with no restart;
  - its pod's Ready condition is True;
  - its node's kubelet still answers through the API server (one bounded log
    read, which a lost node cannot answer).

  A policy engine may reject a denied connection rather than drop it, which the
  connector sees as refused; a listener that held would have accepted any
  connection that reached it. Anything else is inconclusive, and inconclusive
  is never a pass.

A pass is trusted for an hour on that replica; its next create after the hour
proves again. A proof that did not pass is not kept: the step that asked for it
is refused `pipeline_isolation_unenforced` -- no Secret and no Job are created,
so nothing ran -- and the replica's next create proves again. The message says
what the proof saw. For a namespace found open, it names the fix. For a proof
that could not decide, it says the proof is tried again before the next step,
and that if this persists the operator should check that the cluster's network
policy engine is running and that cluster DNS (kube-system) answers. Steps
created at the same moment on one
replica share one proof. Adopting a step another replica started creates
nothing new, and never waits on the proof.

It refuses rather than warns because a step is a repository's code: on a cluster
that does not enforce the policy, it could reach the cloud's instance-metadata
endpoint (on AKS, a source of tokens for the node's identity), the mesh's
in-cluster `/metrics` and the database's Service. The positive control proves that listener ingress is open to the restricted
connector as well. Missing egress protection or a missing IP exception list
therefore fails the probe instead of being hidden by ingress denial. A missing
control path is inconclusive and refuses execution.

This is a representative egress check, not a scan of every cluster address or
cloud endpoint. Operators must keep all pod, service, node and metadata ranges
inside the denied ranges when configuring a different cluster network. The
probe uses Indexed Job pod labels (Kubernetes 1.28 or later); see the
[Kubernetes Job contract](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
and [additive network policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

The opt-in local regression creates and removes its own namespace, without
replacing the installed engine. It checks the shipped rules, missing egress,
a missing `except` list and a missing listener ingress exception:

```bash
MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT=k3d-memql \
  go test ./integrations/pipelinesteps -run '^TestIsolationProofAgainstLocalNetworkPolicy$' -count=1 -v
```

**On AKS as `azure-provision.sh` creates it, every step is refused.** The script
passes no `--network-policy`, so the cluster runs no network policy engine: the
policy is accepted and changes nothing, the connector connects, and every step
is refused `pipeline_isolation_unenforced` until an operator enables an engine:

```bash
az aks update --resource-group rg-<install> --name aks-<install> --network-policy <engine>
```

Which engines a cluster can take depends on its network plugin, and the update
is an operation on the whole cluster: Microsoft's
[network policies on AKS](https://learn.microsoft.com/en-us/azure/aks/use-network-policies)
says which engine fits and what the change does to the nodes. Provisioning does
not choose one for you; no engine has been measured on AKS for this release. A
local cluster (k3s) enforces network policy as installed, so its proof passes.

---

## The fleet

A fleet step runs on one of the pipeline owner's opted-in machines. The
agent dispatches it through its worker dispatcher as `workerHost.pipeline_step`
under the pipeline purpose: no agent is named, and the gates that ask about an
agent -- per-task approval, standing scope, the classifier -- are not asked,
because there is none. Three other things must hold instead.

1. **The pipeline's owner consented.** The pipeline was connected
   `compute: cluster_and_fleet`. A need on a `cluster` pipeline refuses the run
   when it compiles (`pipeline_fleet_not_consented`), and the executor refuses it
   again.
2. **The machine's owner consented.** The machine reports `pipelines=allowed`,
   which its Cockpit advertises exactly when the `pipelines` block of its
   `policy.yaml` allows pipeline steps:

   ```yaml
   pipelines:
     allow: true
     repos:
       - acme/widgets
     workspace_root: ~/ci
     max_timeout_sec: 3600
   ```

   | Key | Meaning |
   |---|---|
   | `allow` | Whether the machine runs pipeline steps at all. Off unless set |
   | `repos` | When it lists any, only these repositories' steps run there. An empty list accepts any |
   | `workspace_root` | Where each step's checkout is made, and removed again |
   | `max_timeout_sec` | The longest a step may run there, whatever its own timeout |

   Cockpit also reports an action-specific `repositoryScopes` entry in its
   capability descriptor. The router requires an explicit scope accepting the
   requested repository before dispatch. Missing scope metadata is unknown
   consent: upgrade and reconnect older workers before using them for builds.
   It must also report `workerHost.pipeline_step` action contract **3**. An older,
   missing or unknown contract refuses before dispatch; operator labels cannot
   override it. Native OS/architecture comes from the binary descriptor and is
   rechecked on the receiving replica.
   An empty advertised list accepts every repository; a nonempty list matches
   exact names, ignoring case, surrounding whitespace and a `.git` suffix.
   A repository-only policy change triggers re-registration. The worker still
   checks its current policy on arrival, so stale advertisements cannot grant
   execution after a withdrawal.

   **An operator label cannot allow a machine.** The router matches a machine's
   reported and operator labels together, but the replica about to dispatch
   re-reads the machine's live registration and refuses before start, sending
   nothing, a machine whose own report lacks `pipelines=allowed`. Fleet's
   machine page shows the machine's own answer as **Pipeline steps**.
3. **The owner's computer use is on.** The owner's computer-use switch is the
   one control that means "nothing runs on my machines": switched off, every
   fleet step is refused `pipeline_fleet_disabled`, with nothing sent. A switch
   that cannot be read refuses too, as `pipeline_executor_error`: an unread
   switch is never taken as on.

The machine must also offer the need, as a routing label it carries. Labels
match exactly:

| Need | Label required of the machine |
|---|---|
| `display` | `display=true` |
| `docker` | `docker=true`, which a machine carries when its Cockpit reports a Docker runtime |
| `gpu` | `gpu=true` |
| `macos_tooling` | `os=darwin` |
| `user_files` | none: every machine of the owner's holds the owner's files |

**Which machine.** The owner's routing policy orders the candidates, as it does
for an agent's call ([The routing policy](workers-runbook.md#52-the-routing-policy)),
with one difference: a pipeline step always moves on to the next matching
machine when one refuses before starting, whatever the policy's `fallback` says.
A refusal before start ran nothing, and a machine whose stream another agent
replica holds is reached over a forward under the owner's authority -- skipped,
never failed, when it cannot be. With no machine that offers the need and allows pipelines, the step is
refused `pipeline_no_machine_for_need`, and nothing ran. An otherwise eligible
worker that is offline can keep admission waiting for its connection; revoked
workers, withdrawn repository consent and incompatible contracts cannot. Under contract 3, policy rejection, busy build capacity and an unavailable
runtime are confirmed pre-execution refusals and permit another candidate.
When the dispatcher confirms that shared build capacity was busy, an eligible
worker is offline, or its connection disappeared before dispatch, the fleet
runner waits and routes again under the run's wall-clock ceiling. Every attempt rechecks consent and routing. Waiting does not consume
the command's own timeout, but the run ceiling still limits its remaining
execution time. A long wait refreshes the repository clone token and keeps
both the old and new token masked. Cancellation ends the wait promptly.
Timeouts, disconnects after dispatch, uncertain cleanup and interrupted prior
attempts do not permit another dispatch: a command might already have performed
an external effect. Each dispatch is recorded like an agent's call, as a
`v1:worker:invocation` of `workerHost.pipeline_step` naming no agent, filed under
the work run and the step.

**Execution is explicit.** `execution: container` uses the exact digest-pinned
Linux image with the declared architecture, even on a Mac. It cannot inherit
host Docker, GPU, display or file access through a `needs` label. Use native
execution for host tools. A container on the fleet uses `placement: fleet`,
without host needs. Docker must answer a live daemon identity/platform probe;
architecture emulation is refused. Native execution also probes Docker when
`needs: { docker: true }` is declared. Labels alone do not prove daemon health.

The current worker admits one pipeline command per OS user across all its
cluster connections. A durable attempt record outlives process death. A new
worker reconciles the recorded containers and network only against the same Docker daemon;
unknown daemon identity, uncertain cleanup and interrupted native execution
remain blocked for reconciliation. This reservation does not reserve agent
work or another OS user's capacity.

Container steps support up to four digest-pinned services on the declared
platform. Services share `localhost` with the command on a per-attempt bridge,
publish no host ports and receive neither the checkout nor the step's secrets.
Declared readiness probes must succeed before the command starts. Every
container and network name is recorded before creation so cancellation or a
replacement worker can remove the entire attempt without sweeping other work.

Declared Go/npm caches persist on the worker. The engine supplies an opaque
owner/repository/event-trust scope; Cockpit also separates local cluster
enrollments, repository hosts, architectures, images and runtime users. Pull
request writes never enter the trusted push/release cache. Cockpit retains up
to 10 GiB between builds, evicting oldest cache directories; this is not a hard
disk quota while a command runs. Native steps cannot declare services or these
managed caches. Upgrade the engine and Cockpit together for contract 3.

**What the machine does with the step** -- the clone (the cluster Job's own
clone script, over `https` only), the command in the machine's own environment,
its output streamed back masked, the artifacts packed, the checkout removed --
is the Cockpit's, and
[Pipeline steps on this machine](https://github.com/znasllc-io/memql-cockpit/blob/main/docs/pipelines.md)
(`docs/pipelines.md` in the memql-cockpit repository) says it. On the engine's
side:

- The agent mints the clone token (one repository, contents read). The dispatch
  carries it with the clone URL, the commit, the command, the contract
  environment, the secrets, the artifact paths and the effective timeout.
- The pipeline's `image`, its services and its caches are the cluster's. A fleet
  step runs its command in the machine's own environment, with no service and
  no cluster cache.
- The dispatch's own deadline is the step's effective timeout plus 5 minutes,
  for the clone before the command and the packing after it.
- The output becomes the step's log through the same capture a cluster step's
  goes through ([Logs](#logs)), and its artifacts are read within the same cap
  ([Artifacts](#artifacts)).

---

## Logs

A step's output goes three places through one capture, the same for a cluster
step and a fleet step. Every line is cleaned once, before it goes anywhere: NUL
bytes dropped, invalid UTF-8 replaced, and every value of four bytes or more
among the step's secrets and clone tokens replaced with `***`.

- **The log store** gets one line per output line, bound to the
  `v1:pipelines:run` as its subject, from component `pipelines.step`, with the
  attributes `stepKey` and `workRunId`: a run's lines read together, and its
  steps' apart. It is a live view, and bounded as one:
  - at most `MEMQL_PIPELINES_LOG_STORE_MAX_LINES` lines a step (2000 by
    default), then one warning line, coded `pipeline_log_capped`, saying the
    live log stops there; the step's result carries the same code as a note;
  - at most 200 lines a second on a node, shared by every step that node is
    running; a line over that pace is missing from the store, and only from the
    store;
  - a line longer than 4096 bytes is stored in pieces.

  Reading the store is an admin-and-above read ([Logs](logs.md)): the Logs app,
  and a run's lines in Deployables' Logs section.
- **The archive** is the record: every line, whole, up to 64 MiB, then one line
  saying how many bytes were dropped. For a cluster step it also holds what the
  store does not show: the clone's last 200 lines, each service's last 50 for a
  failed step, and the runner's own notes. It is filed in the pipeline owner's
  Library as `<step key>.log`, every character outside `A-Z`, `a-z`, `0-9`, `.`,
  `_` and `-` made a dash (`tests.go-tests#2` is `tests.go-tests-2.log`), with
  source `pipeline`, bound to the work run and the step: the full log a run's
  page opens.
- **The tail** is the last 40 lines within 16 KiB: what the check run quotes for
  a failed step.

Two of the runner's notices reach both the store and the archive: the wait for
a free slot under the ceiling, and a re-attach after a replica was lost.

### When a workbench replica is lost

The replica holding a step's Job stamps its claim on the Job every 10 seconds,
with how far it has captured the step's log. A claim 45 seconds without a stamp
is abandoned, and the next replica asked for the step adopts the still-running
Job: it takes the claim, follows the step's log from the start of what the node
still serves, writes the lines up to the previous holder's cursor into the
archive only -- the store has them -- and the rest into both, behind one notice
saying it re-attached and what the archive holds of the step's start. The
kubelet serves only the container's current log file, so once the node has
rotated it, the archive begins where the node's log does, and the notice says so.
A step that ended before the adopter arrived is settled by the end the previous
holder recorded on the Job, when it recorded one. What can be lost or doubled
across a cut is in [Known limitations](#known-limitations).

---

## Artifacts

A step's `artifacts:` are paths or globs relative to the working copy.

- **The paths** must be relative, one word each, with no `.` or `..` segment, no
  glob segment that begins with a dot or a bracket, and no whitespace or shell
  metacharacter (`;`, `&`, `|`, `$`, a backslash, a backtick, a quote of either
  kind, `<`, `>`). Anything else refuses the step `pipeline_job_rejected`
  before it runs. A plain dotfile (`.coverage`) and an ordinary glob
  (`dist/*.js`) are fine.
- **How they leave the pod.** After the command exits, the step's wrapper tars
  the declared paths, gzips and base64-encodes the archive and prints it between
  two marker lines; the capture lifts it out of the output, so it reaches
  neither the log nor the tail. The step's image needs `tar`, `gzip` and
  `base64` for that: without them the artifacts are lost -- a
  `pipeline_artifact_missing` note -- and the step keeps its own outcome. A
  fleet machine packs them itself, under its own caps (the Cockpit's page).
- **What is kept**: regular files under a declared path -- never a link, a
  device or a FIFO, an absolute name or a `..` -- at most 1024 files, and the
  whole decoded archive at most `MEMQL_PIPELINES_ARTIFACT_MAX_BYTES` (64 MiB by
  default). Past that nothing is stored and the step fails
  `pipeline_artifact_too_large`: a step whose command passed fails, its exit
  code 0 kept, and a command that failed keeps its own failure. A refused entry
  is skipped and named in a note, and a declared path nothing matched is a
  `pipeline_artifact_missing` note.
- **Where they go**: one Library file each, owned by the pipeline's owner, with
  source `pipeline`, bound to the work run and the step (`producedByRunId`,
  `producedByStepKey`), and ready at once. A file is named by its path with every
  `/` made `__` (`out/report.xml` is `out__report.xml`) and carries the content
  type its extension names, else `application/octet-stream`. The Library serves
  every file as an attachment, so a step's `.html` or `.svg` is downloaded, never
  rendered in the OS's origin.
- **What the Library will not take is a note, never a failure.** A file over the
  Library's per-file limit (`MEMQL_LIBRARY_MAX_UPLOAD_BYTES`), an owner over
  their quota (`MEMQL_LIBRARY_USER_QUOTA_BYTES`), a node with no object storage,
  or a storage error leaves that file out -- the log as much as an artifact --
  with a `pipeline_artifact_missing` note saying which.

---

## Lifecycle

### Recovering an interrupted driver

A replacement driver preserves completed journal receipts and acknowledges
retained resources idempotently. An unfinished intent is sent as `recoverOnly`:
a cluster runner may adopt an existing Job or its stored outcome, but a missing
Job is `pipeline_execution_uncertain`, without creating a replacement or minting
a clone token. Fleet execution currently has no durable remote receipt lookup;
an interrupted fleet intent is also uncertain and is not dispatched again.
Inspect the original attempt before authorizing new work.

The workbench action is `pipelineStepV2` and its receipt acknowledgement is
`pipelineReceiptAckV2`. Older replicas reject these actions instead of ignoring
new execution, recovery or cleanup guarantees. Upgrade the coordinated engine
set and drain old drivers before enabling the new protocol.

Retention is bounded. Durable external-attempt identity, late-delivery
reconciliation, and deduplicating artifacts if interruption happens before the
runner records its final outcome remain required for complete recovery. A
missing receipt is never proof that the external work did not run.

### Cancel

`pipelinesCancel`, or **Cancel** on the run's page, flags the run
([Pipelines](pipelines.md#connecting-a-pipeline)), and the agent driving it stops
the run's steps:

- Every step it has in flight ends. A cluster step's forward is cancelled, and
  the runner holding it deletes its Job and Secret; a fleet step's dispatch is
  cancelled, and the machine stops the command.
- A workbench replica is asked to delete every Job and Secret carrying the run's
  label, which reaches the Jobs other replicas hold. The request is tried up to
  five times; if no replica confirms it, the Jobs end at their deadline and go
  at their TTL. The driver keeps the run unfinished on a cancellation error,
  retaining its durable cancel request for another agent to retry. It does not
  report a completed cancellation to GitHub while that error remains.

A cancelled step reports `pipeline_step_cancelled`, and what it had printed is
still archived to the Library (a cluster step's archive ends with a line saying
it was cancelled). A cancel that arrives after a step's outcome is recorded on
its Job changes nothing: the outcome stands.

### Cancel-on-push

A new head of a pull request -- delivered by the webhook or seen by the poll --
cancels the pull request's earlier unfinished runs of the same pipeline in
affected mode, each concluded with
`cancelledBy: superseded by <the new run's id>`: a queued run at once, a driven
one through its agent as above. It does so
only when GitHub, asked at that moment, names the new commit as the pull
request's current head. Deliveries are not processed in push order -- two
replicas take them at once, a redelivery arrives late, a poll reads the heads
just before a newer push -- so a late delivery for an older head cancels
nothing, and neither does a head GitHub could not be asked about (no
installation token, a refused read, no head named): the earlier runs run on, and
the log says why. GitHub is asked only when there is something to cancel.

A push never cancels a full-mode run -- the merge queue's, a push to the
default branch's, a release's -- or another pull request's run. A re-run cancels
nothing: it is not a push, and re-running an earlier head must not cancel the
run of the head the pull request shows. Nor does a second sighting of a head (a
redelivered webhook, or the webhook and the poll seeing one head), which opens
nothing. Two pushes to one pull request opened at the same moment may both run,
which costs a runner and nothing else.

**A force-push back to a superseded commit runs it again.** Push X, push Y
(which cancels X's run), force-push back to X: X's next attempt opens and
cancels Y's run -- again only when GitHub names X as the head -- whether X's run
had concluded cancelled or was still stopping. A late redelivery of a
superseded head is answered by the superseded run and changes nothing; so is a
fork's pull request at a superseded commit. A run a person cancelled still
answers its head: a later delivery for that commit does not run it again; re-run
it on purpose.

### Losing a replica, a node or a machine

Nothing about a running step lives in one process only:

- **The workbench replica running a step goes away.** The agent's forward to it
  is watched: once the replica stops being one the agent would send to, the
  agent forwards the step again, to any replica, and that replica finds the Job
  by its name and adopts it once the old claim is 45 seconds unstamped
  ([When a workbench replica is lost](#when-a-workbench-replica-is-lost)).
- **Beside the watch, the agent reads the step's status every 30 seconds**,
  preferring the replica selected for the active forward. That replica can
  report a queued step before its Job exists. An unavailable replica falls
  back to another, and every replica reads the same Job once it exists. A finished step's outcome is taken
  from the Job, even when the reply to the forward was lost. A holder gone quiet
  while the mesh still counts it healthy is forwarded away from once the live
  forward is 3 minutes old -- an adopting replica may be proving isolation
  first. A Job still absent long after the forward is forwarded again, each time
  after twice the wait before it, because a Job is most often absent while its
  runner waits for a slot.
- **No workbench replica is reachable** (a rolling deploy of the workbench). The
  agent tries again every 2 seconds. A step no replica has taken fails
  `pipeline_runner_unavailable` after 2 minutes of that; a step a replica took
  waits on, because its Job may still be running.
- **The node under a step's pod is drained, or the pod preempted or evicted**:
  `pipeline_node_lost`, unless the step's command had already ended.
- **The agent replica driving the run goes away.** Another agent replica takes
  the run over once its lease is 120 seconds stale
  ([Who drives a run](pipelines.md#who-drives-a-run)) and hands the step over
  again as the same attempt, so the runner finds the same Job, running or
  finished.
- **The connection to a fleet machine ends before the machine reports**:
  `pipeline_node_lost`. A fleet step that may have started is never run again
  on another machine.

### When each side gives up

Nothing waits forever, and each party waits longer than the one inside it:

| Who | Gives up at | Then |
|---|---|---|
| The step's Job | its `activeDeadlineSeconds`: the lesser of the step's timeout and what was left of the run's ceiling when it was created | the step fails with its deadline code |
| The runner, settling a step that ended | 3 minutes 30 seconds in all: recording how it ended (10 s), reading the log to its end (30 s), the clone's and services' tails (20 s), the Library writes (2 min) and recording the outcome on the Job (30 s), each phase in a window of its own | a Library that does not answer costs the step its files, never the outcome recorded on the Job |
| The agent | while the step's Job does not exist, the run's ceiling plus 4 minutes 30 seconds; once it does, the Job's creation plus the step's timeout plus 4 minutes 30 seconds, never past the run's ceiling plus 4 minutes 30 seconds. The 4 minutes 30 seconds are the runner's settling and one more minute | one last status read decides. A recorded outcome is taken; a Job still held reads as its deadline code; a Job never created (no slot came before the run's ceiling, or the request never reached a runner) reads `pipeline_run_ceiling`; a holder gone quiet, or no answer, reads `pipeline_node_lost`. Whatever it reads, the Job is deleted |
| The seam's driver | the run's ceiling plus 10 minutes | `pipeline_run_ceiling` |
| A fleet dispatch | the effective timeout plus 5 minutes | by then the machine has stopped the command at the timeout, and the step failed with its deadline code |

---

## Retention

`MEMQL_PIPELINES_RUN_RETENTION_DAYS` (30 by default) is how many days after a
finished run's latest version its records are kept. The nightly operational
retention sweep -- `workJournalRetentionSweep`, 03:40 UTC on the cron leader --
archives them first and deletes them second
([Operational record retention](env-vars.md#operational-record-retention)), in
this order:

1. the pipeline's `v1:work:run` (`triggeredBy: pipeline:<mode>`, at
   `succeeded`, `failed` or `cancelled`), with the steps and closed approvals
   its owner wrote;
2. the `v1:pipelines:run` row, once it is `completed` -- a refused run is
   completed too;
3. the pipeline's `v1:work:goal` (`requestedVia: pipeline`), once no
   `v1:work:run` of it remains. A re-run is a second run of the same goal, so the
   goal goes with its last run.

A work run with model-call or observation detail, a pending approval, a recent
child, or a child with no owner or someone else's, is kept until that clears,
and its goal with it. Nothing is deleted without an archive the sweep has read
back: a cluster with no archive container (`MEMQL_WORK_ARCHIVE_CONTAINER`, else
`MEMQL_AZURE_BLOB_CONTAINER`) deletes nothing.

Not retired by it:

- **The Library files** a run's steps archived. A step's log and its artifacts
  are the owner's, and stay until the owner deletes them; once the run is
  retired, a file's `producedByRunId` names a run that is gone.
- **The log store's lines**, which follow the log store's own window,
  `MEMQL_LOGS_RETENTION_DAYS` ([Logs](logs.md)).

---

## Environment variables

Registered in `scripts/secrets/manifest.yaml` under the `pipelines` component.

| Variable | Default | Read by | What it does |
|---|---|---|---|
| `MEMQL_PIPELINES_NAMESPACE` | `memql-pipelines` | workbench | The namespace the runner creates step Jobs and their Secrets in. It must be the one the pipelines component grants the engine's identity Jobs in, `memql-pipelines`: any other value makes every create a 403, and every step fails `pipeline_runner_unavailable`. The component's `memql-pipelines` ConfigMap sets it |
| `MEMQL_PIPELINES_CLONE_IMAGE` | none | workbench | The image `clone` and `cache-prep` run, which needs `git`, `base64` and `tr`. Pin it by digest: it is handed the repository token. Unset, the node cannot run steps, and every step sent to it fails `pipeline_runner_unavailable`. The ConfigMap pins `docker.io/library/buildpack-deps:bookworm-scm` by its multi-arch index digest |
| `MEMQL_PIPELINES_NODE_POOL` | none | workbench | Select nodes labeled `memql.io/pipeline-pool=<value>` and tolerate only the matching `NoSchedule` taint, for both steps and isolation probes. A lowercase DNS label of at most 63 characters; malformed values refuse execution. Unset means Linux placement without a pool constraint. |
| `MEMQL_PIPELINES_RUN_MAX_MINUTES` | `120` | agent, workbench | A run's wall-clock ceiling, clamped to 5..1440. It bounds how long a step may wait for a slot, and a step's Job is given no more than what is left of it; past it the step fails `pipeline_run_ceiling`. The agent's value sets each run's deadline, stamped on its Job and Secret. Orphan cleanup waits until that deadline plus the Job TTL, regardless of the sweeping workbench's ceiling. The workbench value is only a retention fallback for legacy or malformed Secret metadata |
| `MEMQL_PIPELINES_LOG_STORE_MAX_LINES` | `2000` | workbench (a cluster step), agent (a fleet step) | How many lines of one step reach the log store, clamped to 100..100000, before one `pipeline_log_capped` line. The Library's log keeps every line |
| `MEMQL_PIPELINES_ARTIFACT_MAX_BYTES` | `67108864` (64 MiB) | workbench (a cluster step), agent (a fleet step) | The cap on one step's decoded artifact archive, clamped to 1 MiB..256 MiB. Past it nothing is stored, and the step fails `pipeline_artifact_too_large` |
| `MEMQL_PIPELINES_WORKSPACE_LIMIT` | `20Gi` | workbench | The size limit of every step's `/workspace`, a whole number of `Ki`, `Mi`, `Gi` or `Ti`; anything else is the default. It must equal the LimitRange's default ephemeral-storage limit ([Steps at once, and their size](#steps-at-once-and-their-size)), and the component's ConfigMap sets it so in every overlay |
| `MEMQL_PIPELINES_RUN_RETENTION_DAYS` | `30` | the nightly retention sweep | Days after a finished run's latest version before its records are archived and deleted ([Retention](#retention)) |

The execution settings are read when a node starts. A numeric cap that is not a positive whole
number falls back to its default -- never to a bound, and never to no limit --
and a value past a bound is clamped to it; a workspace limit that is not a
whole number of `Ki`, `Mi`, `Gi` or `Ti` is its default. The agent and the
workbench both read the ceiling and the two caps, so set them where both read
them: `memql-secrets`, which every node reads, rather than the
`memql-pipelines` ConfigMap, which only the workbench does. The retention
window reads a stored global variable of the same name when the environment
does not set it, and a value that is not a positive whole number is 30.

---

## Codes

The substrate's codes, in the seam's catalogue beside the ones
[Pipelines](pipelines.md#refusal-codes) lists. A **failure** fails its step and
so its stage; a **note** changes no outcome. Each reaches the step's row, its
run's page and, for a failed step, the check run, with its sentence.

| Code | Kind | Meaning | What to do |
|---|---|---|---|
| `pipeline_step_timeout` | failure | The step ran past its own timeout, counted from its Job's creation, and was stopped; or nothing reported it by that deadline and the grace past it; or, on a fleet machine, the machine's own `max_timeout_sec` stopped it first, which the message names | Raise the step's `timeout:` (at most `2h`) or split the work. For a fleet step that its machine's cap stopped, raise that machine's `max_timeout_sec` |
| `pipeline_run_ceiling` | failure | The run's wall-clock ceiling bounded the step. It was stopped when the ceiling ran out; or it was not started because the run had used the ceiling; or it was still waiting for a free slot when the run reached it; or nothing reported it by the ceiling and the 10 minutes past it | Shorten the run, run fewer steps at once (a stage wider than the overlay's ceiling waits its turn), or raise `MEMQL_PIPELINES_RUN_MAX_MINUTES` |
| `pipeline_isolation_unenforced` | failure | The workbench replica could not prove `memql-pipelines` isolated, so it created nothing: the step is refused, with what the proof saw | Enable a network policy engine on the cluster (on AKS, `az aks update --network-policy`), then re-run |
| `pipeline_image_pull_failed` | failure | An image of the step's pod -- the step's, a service's, the clone image -- could not be pulled. The pod pulls with no credential, and the first failed pull decides, so a registry's passing error fails the step too | Check the reference, and that the registry allows anonymous pulls (a GHCR package must be public); re-run after a passing error |
| `pipeline_clone_failed` | failure | The commit could not be fetched: the clone exited non-zero, the clone token could not be minted, or a fleet machine's fetch failed. The clone's last lines are in the step's log | Check that the GitHub App's installation still reaches the repository, and that the commit exists |
| `pipeline_service_failed` | failure | A service stopped before the step started: it exited, or its `ready` check kept failing until the kubelet restarted it | Open the full log, which ends with each service's last 50 lines. Fix the service's image, `env` or `ready` |
| `pipeline_job_rejected` | failure | The step could not be made into a safe Job (an artifact path that could leave the working copy, a secret named like a variable the platform sets, an unknown cache, a clone URL that is not plain `https`, no image or command); the cluster refused its Job or Secret; a container could not be created; or the step's cache directory could not be prepared, a cache volume that would not mount within 10 minutes included | The message says which. Fix the manifest when it names the step; for the cache, the pod's events name the mount error |
| `pipeline_job_unschedulable` | failure | The step's pod could not be scheduled within 10 minutes: no node had room, or the cache claim it mounts is Pending (a storage class that does not exist; on AKS, no Blob CSI driver) | Give the steps room -- a dedicated node pool, or lower requests or ceiling in the overlay -- or turn the Blob CSI driver on (`azure-provision.sh` does) |
| `pipeline_step_disk_exceeded` | failure | The kubelet stopped the step for the disk its pod wrote: its working copy past the workspace's size limit, or the pod's or one container's files past the namespace's ephemeral-storage limit. The working copy, what the containers write anywhere in their own filesystems and their logs all count; a declared cache does not. The message quotes the kubelet's sentence | Write less, or declare a cache for what a tool keeps between runs (Go's build cache otherwise lands in the image's home directory); an operator raises the overlay's limit |
| `pipeline_execution_uncertain` | failure | A prior intent has no recoverable Job or fleet receipt; no replacement effect is started | Reconcile the original attempt before authorizing new work |
| `pipeline_node_lost` | failure | The cluster stopped the step's pod (a drain, a preemption, an eviction) before its command ended; the pod or Job failed with nothing saying why; the Job was deleted while another replica held it; no workbench replica reported the step within its deadline and the 4 minutes 30 seconds past it; or a fleet machine's connection ended before it reported | Re-run the step |
| `pipeline_step_cancelled` | failure | The step was cancelled: its run was cancelled or superseded, or the drive running it ended | Re-run when ready |
| `pipeline_no_machine_for_need` | failure | No machine of the owner's that offers the need, allows pipeline steps and is online could take the step, and nothing ran; or the machine's own pipelines policy refused it | Turn on a machine that has the need and allows pipelines in its `policy.yaml` (listing the repository, when `repos` lists any), or drop the need |
| `pipeline_fleet_disabled` | failure | The owner's computer use is switched off, so no step runs on their machines; nothing was sent | Switch computer use back on in Fleet |
| `pipeline_artifact_too_large` | failure | The step's artifacts were over `MEMQL_PIPELINES_ARTIFACT_MAX_BYTES` or the archive's limits (on a fleet machine, the machine's own caps), and none was stored. A command that passed has its step fail; its exit code stays | Narrow the artifact paths, or raise the cap |
| `pipeline_artifact_missing` | note | A declared path matched no file; the step printed no readable artifact frame (an image without `tar`, `gzip` or `base64`); an entry was refused; or a file, the log included, was not stored: over the Library's per-file limit, over the owner's quota, no object storage on the node, a storage error | Check the path, or the setting the note names |
| `pipeline_log_capped` | note | The live log stopped at `MEMQL_PIPELINES_LOG_STORE_MAX_LINES` lines | Open the full log, which is in the Library |
| `pipeline_timings_unreadable` | note | The step's Go test timings could not be read from its log; its packages keep their earlier weights in the timing table | Nothing |
| `pipeline_outcome_trimmed` | note | Some artifact file ids or Go timings were left out of the step's recorded outcome, which must fit in a Job annotation | Nothing: the files are in the Library, filed under the run and the step |

Two of the seam's codes come from the substrate too:

| Code | Kind | When the substrate reports it | What to do |
|---|---|---|---|
| `pipeline_runner_unavailable` | failure | No runner could take the step: the agent has no route to a workbench replica or no dispatcher for a need; no workbench replica was reachable for 2 minutes; the replica cannot run steps (no clone image, no in-cluster API server, no node id); or the API server refused the engine's identity or did not answer | Set the cluster up as [Where a step runs](#where-a-step-runs) says, then re-run. Nothing in the repository is wrong |
| `pipeline_executor_error` | failure | Something answered that is not a step's outcome: a workbench replica's refusal, an outcome recorded on a Job that cannot be read, the engine's own dispatcher refusing a fleet step before routing it (an unreadable computer-use switch, owner's machines that could not be read), or a machine answering what the engine does not expect | Re-run; if it repeats, the message names the part to look at |

The executor also checks again what the compiler refused first, a need outside
the closed set (`pipeline_need_unknown`) and a need on a `cluster` pipeline
(`pipeline_fleet_not_consented`): it routes neither.

---

## The engine's own pipeline

This repository is a source like any other: its `memql-package.yaml` declares a
pipeline and no apps, and an owner connects it the way any source's is connected
([Connecting a pipeline](pipelines.md#connecting-a-pipeline)); nothing connects
it by itself. Its steps are lanes of `.github/workflows/ci.yml` copied with
their commands, and the file's header names the lanes it leaves to `ci.yml`: the
ones that need Docker or a display, the node-tag, cluster-e2e and per-module
passes, and a few not carried here yet.

| Stage | Steps | Runs on |
|---|---|---|
| `checks` | `go-checks` (30 min: build, vet, the env registry, the DSL lint and engine load, and the SDK, concept-snapshot, authoring-surface and proto drift gates) and `path-routing` | every run |
| `tests` | `go-tests` (affected packages outside the db-gated trees, 4 shards, 30 min), `db-tests` (the affected db-gated packages, 5 shards, 40 min, each beside its own Postgres sidecar), `fuzz`, and `os-checks` when the change touches the OS bucket | every run |
| `gates` | `gate-inputs`, when the change is not all Go source | pull requests |

No step names a need, so the pipeline runs on the cluster alone and connects
with `compute: cluster`. `memql_package_manifest_test.go` holds three of its
copies to their originals -- `select.dbGated` to
`scripts/ci/db-gated-packages.sh`, the gate packages to `ci.yml`'s
`GATE_PACKAGES`, the Postgres service to `ci.yml`'s db-tests service -- and
nothing holds the rest: the step commands, the bucket globs, the shard counts
and the fuzz targets are hand copies of `ci.yml`, and a change there is made
here too.

What a run of it costs, and where it falls short of `ci.yml`: every step clones
the commit on its own (10 to 76 seconds a step for this repository, measured on
a local cluster); every Go step fetches the standalone Tailwind CLI into the
checkout to build the identity stylesheet, which no declared cache holds; and
`go-checks`' authoring-surface gate (`scripts/ci/memqlbreaking-base.sh`) has no
base commit in a run -- a step's checkout is one commit with no remote -- so it
compares against the committed baseline, which the change under test can edit
too, and says so: `ci.yml`'s lane stays the gate for it.

### The toolchain image

`ghcr.io/znasllc-io/memql-toolchain` is what the engine's pipeline runs in,
built from `deploy/toolchain-image/Dockerfile` by `build-toolchain-image.yml`:
dispatched on main only, smoke-tested before it is pushed, a published tag
never replaced unless the dispatch says so, `linux/amd64` only. It carries:

- Go 1.27.1 (`go.work`'s toolchain line), Node 22, protoc 33.4 and kubectl
  1.32.13 (the minor of the local cluster's k3s); a test holds Go, protoc and
  kubectl's minor to the repository's own pins;
- the Docker client 29.8.2, and no daemon -- here or anywhere in a step's pod --
  so `docker build` and `docker run` cannot work;
- git, make, bash, curl, tar, gzip, unzip, jq, python3 with PyYAML, coreutils,
  ca-certificates and the PostgreSQL client (`psql`, `pg_isready`);
- uid 1000 (`memql`), never root, with git trusting `/workspace`.

`memql-package.yaml` names the image by the digest its run's summary printed
(version `1.0.0`). A rebuild takes a new version and a new pin: the manifest's
test fails on any reference that is not a digest of this repository.

---

## Owner actions

- [ ] **Make the `ci-timescaledb` GHCR package public**: the engine
  pipeline's Postgres sidecar (`memql-toolchain` is public already). A step's
  pod pulls every image with no
  registry credential -- its ServiceAccount carries no pull secret -- so a
  private package fails every step that uses it `pipeline_image_pull_failed`.
  On GitHub: the organization's packages, the package, Package settings, Change
  visibility.
- [ ] **On AKS, enable a network policy engine**
  ([The isolation proof](#the-isolation-proof)). Until then every step is
  refused `pipeline_isolation_unenforced`.
- [ ] **Give the nodes the Library's blob storage**: `MEMQL_AZURE_BLOB_CONTAINER`
  and `MEMQL_AZURE_STORAGE_CONNECTION_STRING`. A step's log and artifacts are
  filed by the workbench (a cluster step) and the agent (a fleet step) with the
  storage the bff's Library uses. No cloud overlay declares either key: put both
  in `memql-secrets`, which every node reads. Without them every step still
  runs, and each log and artifact is a `pipeline_artifact_missing` note ("this
  node has no object storage configured") instead of a Library file. A local
  cluster has both, for its in-cluster Azurite.
- [ ] **Bring a cluster that predates the substrate onto it**: an install whose
  Argo CD runs its own AppProject adds `memql-pipelines` as a destination before
  it syncs a revision carrying the pipelines component (the repository's `memql`
  AppProject permits it); on AKS, re-run `azure-provision.sh`, which turns on
  the Blob CSI driver the cloud cache's storage class needs.
- [ ] **For a step that names a need**: a machine of the owner's whose Cockpit
  carries the `pipelines` policy, allowed in its `policy.yaml`
  ([The fleet](#the-fleet)), and the pipeline connected
  `compute: cluster_and_fleet`.

---

## Known limitations

Each of these is understood, and accepted for this release.

- **The log follower's one-second window.** When a stream of a step's log
  drops, the runner opens it again from the earliest line it saw cut, or one
  second before the latest line it fed whole, whichever is earlier, and keeps
  every line exactly once within that window. A line it has no record of --
  written after a stream ended, or cut inside its timestamp -- that is stamped
  more than a second before the latest line fed is never served again, and is
  lost: the window covers the milliseconds by which stdout's and stderr's stamps
  disagree, not a clock that jumps. Further back than the window, a line can be
  captured twice. After an adoption, the store can hold up to one stamp's worth
  (10 seconds) of lines twice: those the old holder captured after its last
  stamp, which the adopter captures again from the cursor. And a line the old
  holder never captured, written after its cursor's line but stamped before it,
  reaches the archive only.
- **Two pull requests that share a head commit.** A run is keyed by repository,
  commit, mode and event, not by pull request, so that commit's run carries the
  first pull request's number, and a push to the first pull request can cancel
  it while it is still the other pull request's head. The other pull request then
  shows a cancelled check until someone re-runs it (`pipelinesRerun`, or
  GitHub's re-run).
- **A force-push back in a narrow window.** A force-push back to a superseded
  commit that is delivered in the milliseconds between the newer head's read of
  GitHub and its cancelling of the older run is answered by the superseded run,
  which is then cancelled. Re-run it.
- **A superseded run's check run reads only "Cancelled".** The reason -- the
  newer run that superseded it -- is on the run row (`cancelledBy`) and its page
  in MemQL OS, not on GitHub (memql#5814).
- **GitHub may briefly show the superseded attempt.** After a force-push back,
  the newer attempt's check run is created before the superseded attempt's is
  concluded cancelled, so a pull request showing the latest-completed check run
  can read cancelled until the newer attempt completes.
- **The clone's umask.** The checkout is world-writable inside the pod
  ([The checkout](#the-checkout)): a tool that refuses a configuration file
  others can write -- `ssh -F`, `mysql --defaults-file` -- needs `chmod go-w` on
  that file in the step.
- **The cluster-e2e leg runs the substrate in its test process.**
  `install-cluster-e2e.yml`'s `pipelines` leg installs the cluster from source
  and runs `TestPipelinesSubstrate` (`test/clustere2e`): the real executor, its
  fleet half and two workbench runners in the test process, talking to the
  cluster's real API server as `memql-engine-workbench`, under the deployed Role, quota,
  limits, cache claim, network policy and ConfigMap, cloning a public repository
  anonymously at a pinned commit. GitHub there is a test server whose check runs
  are held to recorded fixtures, and the fleet's dispatcher is a stand-in with no
  machine. It does not cover the deployed agent and workbench wiring -- the
  app's adapter, the node mesh's streams, the dispatcher -- which unit and
  in-process hop tests cover. It runs on that workflow's own triggers --
  nightly, on demand, and on pull requests that change the installer, the local
  cluster scripts, `deploy/k8s` or the workflow -- so a change to the
  substrate's code alone does not run it. A live lane against GitHub itself,
  `pipelines-live.yml`, ships disarmed.
- **Readiness cannot see the workbench.** Settings -> Pipelines' runner fact
  says whether the agent node reporting it registered an executor, which every
  agent node now does. It cannot see whether a workbench replica can run steps,
  or whether the isolation proof passes: a cluster whose workbench has no clone
  image, or whose isolation cannot be proved, reads configured and fails its
  steps (memql#5803).
- **A step can run twice after an agent replica loses a run's lease mid-step.**
  The old replica may take the step's outcome and have its Job deleted before the
  new driver hands the same attempt over again, which then finds no Job and
  starts a fresh one. The answer is the second run's; the cost is a runner.
- **The cloud cache on Azure Blob NFS is unmeasured.** Two things are untested
  (memql#5813):
  - cache-prep, which runs as root with every capability dropped, creates the
    cache's directory and sets mode 1777 on it, and nobody has checked that
    Blob NFS allows either. If it does not, every cloud step that declares a
    cache fails at cache-prep. (On any class, cache-prep cannot open a claim
    root that another user owns and keeps closed to others.)
  - Blob NFS has no NLM locking, while concurrent steps of one repository and
    trust on two nodes share one Go build and module cache.
- **A multi-line secret's short lines are not masked one by one.** Masking
  covers each secret whole, trimmed, and each of its lines, trimmed. Like the
  check run's masking, it drops any form shorter than 4 bytes. So a secret
  printed one short line at a time is masked only where its longer lines appear.
- **A partition can leave an old queued request alive.** Status reads prefer
  the active forward's replica, avoiding duplicate waits during ordinary quota
  pressure. A replacement after a lost route can still overlap a surviving old
  request; deterministic Job naming makes them converge on one Job (#5821).
- **A cancel can miss a step still waiting for a slot.** A cancel deletes every
  Job the run has and stops every step its agent has in flight. A step still
  waiting for a slot on a workbench replica, forwarded by an agent replica that
  has since gone away, can create its Job after the cancel and run until its
  deadline; its outcome reaches nobody, and the Job's TTL collects it.

---

## Related

- [Pipelines](pipelines.md) -- the manifest's `pipeline:` block, runs and their
  plan, secrets, and the check run
- [Workers](workers-runbook.md) -- the fleet a step that names a need runs on:
  machines, labels and the routing policy
- [Logs](logs.md) -- the log store a step's lines stream into, and its retention
- [Library](library.md) -- where a step's log and artifacts are filed
- [Environment variables](env-vars.md) -- the registry, and operational record
  retention
- [Workbench](workbench-runbook.md) -- the workbench node, which also holds the
  pipelines runner

### Recovery keeps the original execution definition

A pipeline records a digest of each complete compiled step and its immutable
run inputs before execution. Recovery restores the recorded package selection,
then verifies the command, image, services, placement, dependencies, step order,
source commit, driver engine revision and cluster domain. A changed definition,
missing proof or unknown/dirty engine revision stops recovery. Finished receipts
are preserved; an explicit new attempt can run a newly reviewed definition.
A failed-only rerun reuses earlier success only when that definition matches.
This does not yet pin the transitive definitions of arbitrary DSL automations.

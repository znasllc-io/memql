# Pipelines, the substrate -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every compiled pipeline step runs somewhere safe: as a Kubernetes Job in the
`memql-pipelines` namespace created and watched by the workbench node, or on a fleet
machine when it names a need the cluster cannot meet; its output lands in the log store
and the Library, its artifacts in the Library, and its lifecycle (cancel, ceilings,
retention) is typed.

**Architecture:** The seam (epic memql#5477, session memql-46, branch
`epic/pipelines-seam`) owns `component/pipelines` (the leaf: compiled `Step`,
`StepRequest`, `StepResult`, the `Executor` interface and `RegisterExecutor`) and
`component/pipelinerun` (the driver, which calls `Execute` ALWAYS on an agent node). This
epic registers one `Executor` on the agent node (`integrations/pipelinesteps`): a step
with no needs is forwarded to a workbench replica over the existing
`WorkbenchForwardRequest`, where a runner creates, watches and captures a Job through the
repository's own HTTPS Kubernetes client (`component/deploycontrol.ClusterAPI` -- client-go
is banned by `no_client_go_test.go`); a step naming a need is dispatched through the
agent's existing fleet dispatcher as a new `workerHost.pipeline_step` action that the
cockpit executes with the same clone-and-command contract.

**Tech Stack:** Go 1.26 (multi-module workspace), kustomize 5 (via `kubectl kustomize`),
Kubernetes batch/v1 Jobs with native sidecars (k3s v1.32, AKS >= 1.30), MemQL DSL,
TypeScript (MemQL OS, minimal), GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md` (D7-D11 and
section 4, epic 3) plus the documentation program amendment D15
(`docs/superpowers/specs/2026-09-27-documentation-program-design.md`). Issues: epic
memql#5478; tasks #5492-#5497. One PR for all of them (owner instruction).

## Global Constraints

- client-go, k8s.io/apimachinery and controller-runtime are banned (`TestNoKubernetesClientInTheModuleGraph`); talk to the API server through `deploycontrol.ClusterAPI`.
- No `if env == ...` branching in engine code; every difference between local and cloud is an overlay VALUE.
- Namespace `memql-pipelines`; the step pod carries `automountServiceAccountToken: false`, `enableServiceLinks: false`, and no Secret of the platform's.
- Env contract inside a step: exactly `StepRequest.Environment()` (MEMQL_PACKAGES, MEMQL_RUN_ID, MEMQL_VERSION, MEMQL_SHA, MEMQL_MODE, MEMQL_EVENT, MEMQL_STEP plus resolved secrets) -- the same on the cluster and on a fleet machine.
- Per-step timeout default 20 min (1200 s); a run wall-clock ceiling (`MEMQL_PIPELINES_RUN_MAX_MINUTES`, default 120); both are typed failures, never a skipped step.
- Retention: `MEMQL_PIPELINES_RUN_RETENTION_DAYS` (default 30), archive first, delete second, no archive means no delete.
- Typed codes use the `pipeline_` family and live in the seam's catalogue (`component/pipelines/refusal.go` + the packages literal catalogue + OS copy); this epic's codes: `pipeline_step_timeout`, `pipeline_run_ceiling`, `pipeline_no_machine_for_need`, `pipeline_fleet_disabled`, `pipeline_job_rejected`, `pipeline_job_unschedulable`, `pipeline_image_pull_failed`, `pipeline_clone_failed`, `pipeline_service_failed`, `pipeline_step_cancelled`, `pipeline_node_lost`, `pipeline_artifact_too_large`, notes `pipeline_artifact_missing`, `pipeline_log_capped`.
- A forwarded handler never blocks the NodeService receive loop for long work: spawn a goroutine (sends are serialized by `serializeStream`).
- Multi-node is the default: every piece of state names the node that holds it; the cross-node hop is tested in-process.
- New env vars go in `scripts/secrets/manifest.yaml`, then `make env-registry-sync`, then `docs/public/operate/env-vars.md`.
- Stage files by explicit path; never `git add -A`. Never run prettier. No emojis.
- The plan file is deleted in the epic's merge.

## Review Focus

1. **A secret the step echoes.** A step that prints a resolved secret must not put it in the log store, the Library archive, the log tail or the result: every captured line is masked (`***`) for every secret value of length >= 4 before it goes anywhere. Test: Task 5 `TestCaptureMasksSecretValues`.
2. **A workbench replica restarting mid-step (a deploy).** The step must neither hang until its deadline nor run twice: the agent sees the runner's heartbeat go stale, re-forwards, and another replica adopts the still-running Job by its deterministic name. Test: Task 6 `TestRunnerAdoptsAJobWhoseRunnerWentStale` and Task 12 `TestExecuteReattachesWhenTheWorkbenchIsLost`.
3. **A broken image reference or an image without the tools.** An image that cannot be pulled fails in seconds as `pipeline_image_pull_failed`, not after 20 minutes; a step declaring artifacts in an image with no `tar`/`base64` keeps its own exit status and reports `pipeline_artifact_missing`. Tests: Task 4 `TestClassifyImagePullBackOffIsTerminal`, Task 5 `TestArtifactFrameMissingIsANoteNotAFailure`.
4. **The concurrency ceiling is full.** A step that meets an exhausted `count/jobs.batch` quota waits (the run shows it queued) rather than failing, bounded by the run ceiling. Test: Task 6 `TestRunnerWaitsOutAnExceededQuota`.
5. **Hostile or huge output.** Lines longer than 4096 bytes, invalid UTF-8, a 1 GB log and an artifact path such as `../../etc/passwd` must neither crash the workbench nor escape the step: long lines are split, invalid bytes replaced, the store copy capped (`pipeline_log_capped`), the archive capped with a marker, traversal refused. Tests: Task 5 `TestCaptureSplitsLongLinesAndRepairsUTF8`, `TestCaptureCapsTheStoreCopy`, `TestExtractArtifactsRefusesTraversal`.

---

## Part 1 -- foundations (no dependency on the seam's code)

Part 1 runs on `epic/pipelines-substrate`, stacked on the seam's contract commit `2d6336ba1`
(`component/pipelines`: `Step`, `StepRequest`, `StepResult`, `Service`, `Repository`, `Failure`,
`Where`, the `Code*` catalogue, `Needs()`). The runner uses those types directly (imported as
`pl`); only `StepRun`, the runner's own forward payload, is defined here. When the seam merges:
`git rebase --onto origin/main 2d6336ba1`. `integrations/go.mod` gains
`github.com/znasllc-io/memql/component/pipelines v0.0.0` in its require block (the replace is
already there).

### Task 1: deploy/k8s -- the memql-pipelines namespace, RBAC, cache volume, ceiling and limits (#5492)

**Files:**
- Create: `deploy/k8s/components/pipelines/kustomization.yaml`
- Create: `deploy/k8s/components/pipelines/namespace.yaml`
- Create: `deploy/k8s/components/pipelines/rbac.yaml`
- Create: `deploy/k8s/components/pipelines/step-serviceaccount.yaml`
- Create: `deploy/k8s/components/pipelines/cache.yaml`
- Create: `deploy/k8s/components/pipelines/ceiling.yaml` (ResourceQuota + LimitRange)
- Create: `deploy/k8s/components/pipelines/networkpolicy.yaml`
- Create: `deploy/k8s/components/pipelines/config.yaml` (ConfigMap `memql-pipelines` in the mesh namespace)
- Create: `deploy/k8s/components/pipelines/README.md`
- Create: `deploy/k8s/components/pipelines/component_test.go`
- Create: `deploy/k8s/overlays/{local,cloud,cloud-entry}/namespace-transformer.yaml`
- Create: `deploy/k8s/overlays/{local,cloud,cloud-entry}/pipelines-values.yaml` (overlay values as patches)
- Create: `deploy/k8s/overlays/render_pipelines_test.go`
- Modify: `deploy/k8s/overlays/{local,cloud,cloud-entry}/kustomization.yaml` (`namespace: memql` -> `transformers:`; add the component and the values patch)
- Modify: `deploy/k8s/overlays/render_cloud_test.go` (`TestTheCloudOverlayLandsWhollyInOneNamespace`), `deploy/k8s/overlays/render_cloud_entry_test.go` (`TestCloudEntryLandsWhollyInOneNamespace`), `TestTheAppOfAppsRendersTheApplication` (also require `namespace: memql-pipelines` in the AppProject)
- Modify: `deploy/argocd/apps/project.yaml` (destination `memql-pipelines`)
- Modify: `scripts/deploy/azure-provision.sh` (`--enable-blob-driver` on create; idempotent `az aks update --enable-blob-driver` on an existing cluster) and `deploy/k8s/overlays/substrate_overlay_coupling_test.go` (cloud cache class `azureblob-*` <=> the flag)

**Decisions (measured, keep them):**
- The objects are a COMPONENT, not base: tenants render `base` under `namespace: __TENANT__`, so a fixed `memql-pipelines` in base would collide across tenants. Tenants do not compose the component (stated in its README).
- The three overlays swap the `namespace: memql` field for a `NamespaceTransformer` with `unsetOnly: true`. Measured on 2026-10-03: with the field, a second Namespace object fails the render with "namespace transformation produces ID conflict"; with the transformer, all three overlays render BYTE-IDENTICAL to today (0 diff lines) and explicit namespaces survive. `setRoleBindingSubjects` is left at its default.
- The Role binds the shared `memql-engine` ServiceAccount (the workbench's identity). A dedicated workbench SA would break `render_anthropic_federation_test.go` and the federation trust that names `memql-engine` (the workbench can be the cron leader and make model calls). Precedent: `base/custom-domain-rbac.yaml`, a feature Role in its own file. The Role is confined to `memql-pipelines`; only the workbench binary contains the runner.
- The concurrency ceiling is a ResourceQuota `count/jobs.batch` -- atomic and cluster-wide, so two workbench replicas cannot both exceed it; the runner waits on `exceeded quota`. Container limits are a LimitRange default, so the runner sets no resources itself (one source of truth).

- [ ] **Step 1: Write the failing render gate** `deploy/k8s/overlays/render_pipelines_test.go` (package `overlays`, reuse `render(t, dir)` and `parse(t, ...)` from `render_cloud_test.go`; extend the parsed struct locally if it lacks fields). It asserts, for `cloud` and `cloud-entry` (the local overlay gets the same assertions through `overlays/local/render_domain_test.go`'s `render(t)` in a sibling test `overlays/local/render_pipelines_test.go`):

```go
const pipelinesNamespace = "memql-pipelines"

// The objects the component must render, by kind and name, in memql-pipelines.
var wantPipelinesObjects = map[string]string{
	"ServiceAccount/memql-pipelines-step":   pipelinesNamespace,
	"Role/memql-pipelines-runner":           pipelinesNamespace,
	"RoleBinding/memql-pipelines-runner":    pipelinesNamespace,
	"PersistentVolumeClaim/memql-pipelines-cache": pipelinesNamespace,
	"ResourceQuota/memql-pipelines-ceiling": pipelinesNamespace,
	"LimitRange/memql-pipelines-limits":     pipelinesNamespace,
	"NetworkPolicy/memql-pipelines-isolate": pipelinesNamespace,
	"ConfigMap/memql-pipelines":             cloudNamespace, // the workbench's env
}

func TestPipelinesObjectsRenderInTheirNamespace(t *testing.T)        // every key above present, in its namespace; a Namespace object named memql-pipelines exists
func TestPipelinesRoleGrantsJobsThereAndNothingElse(t *testing.T)   // rules == exactly the table below; no ClusterRole/ClusterRoleBinding in the render names pipelines
func TestPipelinesRoleBindsTheEngineIdentityOnly(t *testing.T)      // one subject: ServiceAccount memql-engine in namespace memql
func TestPipelinesStepIdentityHoldsNothing(t *testing.T)            // SA automountServiceAccountToken=false; no RoleBinding subject names memql-pipelines-step
func TestPipelinesNetworkPolicyIsolatesTheNamespace(t *testing.T)   // podSelector {}, policyTypes [Ingress Egress], no ingress rules, egress = DNS to kube-system + 0.0.0.0/0 except 10/8, 172.16/12, 192.168/16, 169.254/16
func TestPipelinesCeilingAndLimitsAreValues(t *testing.T)           // ResourceQuota hard count/jobs.batch present and >0; LimitRange type Container with default + defaultRequest cpu and memory
func TestWorkbenchReadsThePipelinesConfig(t *testing.T)             // Deployment workbench envFrom configMapRef memql-pipelines; no other Deployment does
```

Role rules, exactly (order-insensitive compare):

| apiGroups | resources | verbs |
|---|---|---|
| `batch` | `jobs` | create, get, list, watch, patch, delete, deletecollection |
| `""` | `pods` | get, list, watch |
| `""` | `pods/log` | get |
| `""` | `secrets` | create, get, patch, delete, deletecollection |

Per-overlay values the gate pins: `local` cache PVC has no `storageClassName` and `ReadWriteOnce`; `cloud` and `cloud-entry` use `storageClassName: azureblob-nfs-premium` and `ReadWriteMany`.

- [ ] **Step 2: Run it to see it fail** -- `go test ./deploy/k8s/overlays/... -run 'Pipelines|WorkbenchReadsThePipelinesConfig' -count=1` from the repo root. Expected: FAIL (objects absent).

- [ ] **Step 3: Write the component.** `kustomization.yaml`:

```yaml
# The pipelines substrate (epic memql#5478, task #5492). Composed by the local,
# cloud and cloud-entry overlays; NOT by tenants (a tenant renders base under its
# own namespace, and memql-pipelines is one name per cluster).
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
resources:
  - namespace.yaml
  - step-serviceaccount.yaml
  - rbac.yaml
  - cache.yaml
  - ceiling.yaml
  - networkpolicy.yaml
  - config.yaml
patches:
  - target: { kind: Deployment, name: workbench }
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/envFrom/-
        value:
          configMapRef:
            name: memql-pipelines
```

`namespace.yaml` (explicit name, survives the unsetOnly transformer; PSA baseline enforce, restricted warn):

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: memql-pipelines
  labels:
    app.kubernetes.io/part-of: memql
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/warn: restricted
```

`rbac.yaml`: Role `memql-pipelines-runner` (rules from the table above) and RoleBinding `memql-pipelines-runner`, both `namespace: memql-pipelines`, subject `{kind: ServiceAccount, name: memql-engine, namespace: memql}`. Header comment: why `memql-engine` (federation; custom-domain precedent), why confined.

`step-serviceaccount.yaml`: ServiceAccount `memql-pipelines-step`, `namespace: memql-pipelines`, `automountServiceAccountToken: false`.

`cache.yaml`: PVC `memql-pipelines-cache`, `namespace: memql-pipelines`, `accessModes: [ReadWriteOnce]`, `resources.requests.storage: 20Gi`, no storageClassName (overlay value).

`ceiling.yaml`: ResourceQuota `memql-pipelines-ceiling` (`hard: {count/jobs.batch: "4"}`), LimitRange `memql-pipelines-limits` (`type: Container`, `default: {cpu: "2", memory: 4Gi}`, `defaultRequest: {cpu: 250m, memory: 512Mi}`), both `namespace: memql-pipelines`.

`networkpolicy.yaml`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: memql-pipelines-isolate
  namespace: memql-pipelines
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  ingress: []
  egress:
    - to:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: kube-system }
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16]
```

`config.yaml` (no namespace: lands in the mesh namespace through the transformer):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: memql-pipelines
data:
  MEMQL_PIPELINES_NAMESPACE: memql-pipelines
  MEMQL_PIPELINES_CLONE_IMAGE: "docker.io/library/buildpack-deps:bookworm-scm@sha256:<resolve with `docker buildx imagetools inspect docker.io/library/buildpack-deps:bookworm-scm` and paste the index digest>"
```

(The digest is measured at implementation time, never invented; record the command in the commit body.)

- [ ] **Step 4: Swap the overlays to the transformer and compose the component.** In each of `local`, `cloud`, `cloud-entry`: replace `namespace: memql` with `transformers: [namespace-transformer.yaml]` (keep the comment block, rewritten to say the transformer is the namespace VALUE and why `unsetOnly`), add `../../components/pipelines` to `components:`, add `pipelines-values.yaml` to `patches:`. `namespace-transformer.yaml`:

```yaml
# The mesh namespace. unsetOnly is load-bearing: the pipelines component renders
# objects into a SECOND namespace (memql-pipelines), which the plain `namespace:`
# field would rename into this one ("namespace transformation produces ID conflict").
apiVersion: builtin
kind: NamespaceTransformer
metadata:
  name: mesh-namespace
  namespace: memql
unsetOnly: true
```

`pipelines-values.yaml` per overlay -- local: none needed beyond defaults (create the file with the ceiling `"4"` and limits as explicit values so the overlay owns them); cloud: PVC `storageClassName: azureblob-nfs-premium`, `accessModes: [ReadWriteMany]`, `storage: 100Gi`, ceiling `"2"`, limits `cpu: "2"`, `memory: 4Gi`, requests `cpu: 250m`, `memory: 512Mi`; cloud-entry: class and RWX as cloud, `storage: 50Gi`, ceiling `"1"`, same limits and requests (ledger Ruling R11: sized for the default 2 x D2as_v4 pool). Before editing, render each overlay to a temp file; after, `diff` the old render against the new one with the component's objects filtered out -- it must be empty (the 2026-10-03 measurement says it is).

- [ ] **Step 5: Amend the one-namespace gates.** `TestTheCloudOverlayLandsWhollyInOneNamespace` and `TestCloudEntryLandsWhollyInOneNamespace` now accept exactly two Namespace objects (`memql`, `memql-pipelines`) and accept `metadata.namespace == "memql-pipelines"` only for the kinds in `wantPipelinesObjects`; anything else outside `memql` still fails. Update both doc comments: the second namespace is the pipelines substrate's, rendered on purpose. `project.yaml`: add `- namespace: memql-pipelines` with the same server as the others; `TestTheAppOfAppsRendersTheApplication` additionally requires it.

- [ ] **Step 6: Azure blob CSI.** In `scripts/deploy/azure-provision.sh`, add `--enable-blob-driver` to the `az aks create` args; for an existing cluster, an idempotent `az aks update --enable-blob-driver` behind the script's existing "cluster exists" branch (follow its pattern; capability-script contract: no prompts, JSON envelope). Extend `substrate_overlay_coupling_test.go` with `TestPipelinesCacheClassNeedsTheBlobDriver`: if the cloud overlay's rendered cache PVC class starts with `azureblob-`, the provision script must contain `--enable-blob-driver`.

- [ ] **Step 7: Run every render gate.** `go test ./deploy/... -count=1` (needs `kubectl` on PATH; `~/.local/bin/kubectl` locally). Expected: PASS. Also `go test ./components/... -run Tenant -count=1` from `deploy/k8s` is covered by the same command.

- [ ] **Step 8: Commit.** Explicit paths. Message: `Issue #5492: the memql-pipelines namespace, RBAC, cache volume, ceiling and limits as a component the overlays compose`.

### Task 2: ClusterAPI -- a test seam and a streaming GET (#5493)

**Files:**
- Modify: `component/deploycontrol/k8sapi.go`
- Test: `component/deploycontrol/k8sapi_test.go`

**Interfaces (produces):**

```go
// NewClusterAPIWith builds a ClusterAPI against an explicit API server. Tests pass an
// httptest server; production code keeps NewClusterAPI.
func NewClusterAPIWith(base, token string, client *http.Client) *ClusterAPI

// Stream issues a GET whose body the caller reads incrementally (pod logs with
// follow=true). It is NOT bounded by the 30 s per-call timeout; the context bounds it.
// A non-2xx answer is a *StatusError, the same as Do.
func (e *ClusterAPI) Stream(ctx context.Context, path string) (io.ReadCloser, error)

// IsConflict reports a 409 (AlreadyExists on create, a resourceVersion mismatch on update).
func IsConflict(err error) bool

// IsForbiddenQuota reports a 403 whose body names an exceeded ResourceQuota.
func IsForbiddenQuota(err error) bool
```

- [ ] **Step 1: Failing tests** in `k8sapi_test.go`: `TestStreamReadsPastTheCallTimeout` (an httptest handler that writes a line, flushes, sleeps longer than a client `Timeout` set on the per-call client, writes a second line; `Stream` returns both); `TestStreamReturnsStatusErrorOnNon2xx`; `TestIsConflictAndIsForbiddenQuota` (409 -> IsConflict; 403 with body `exceeded quota: memql-pipelines-ceiling` -> IsForbiddenQuota; 403 without it -> false); `TestNewClusterAPIWithSendsTheBearer`.
- [ ] **Step 2: Run** `go test ./component/deploycontrol/ -run 'Stream|IsConflict|NewClusterAPIWith' -count=1` -- FAIL (undefined).
- [ ] **Step 3: Implement.** `ClusterAPI` gains a `stream *http.Client` (same Transport, `Timeout: 0`), set in `NewClusterAPI` and `NewClusterAPIWith`. `tokenNow()` keeps re-reading the projected file; for `NewClusterAPIWith` the fixed token is returned when the file is absent (already the fallback). `Stream` builds the request like `do`, sends it on `e.stream`, and on non-2xx reads at most 64 KiB of the body into a `*StatusError` and closes it.
- [ ] **Step 4: Run** the same command -- PASS; then `go test ./component/deploycontrol/ -count=1`.
- [ ] **Step 5: Commit** `Issue #5493: ClusterAPI gains a test seam and a streaming GET for followed logs`.

### Task 3: integrations/pipelinesteps -- config, names, the Job and Secret specs, the step wrapper (#5493)

**Files:**
- Create: `integrations/pipelinesteps/doc.go` (package overview: the three halves and which node runs each)
- Create: `integrations/pipelinesteps/config.go`, `names.go`, `protocol.go`, `kubetypes.go`, `jobspec.go`, `wrapper.go`
- Test: `integrations/pipelinesteps/jobspec_test.go`, `names_test.go`, `config_test.go`
- Modify: `scripts/secrets/manifest.yaml` + `make env-registry-sync` (`component/envregistry/manifest.yaml`)

**Interfaces (produces) -- `protocol.go`, the runner's own wire, JSON-tagged plain data:**

```go
// The four forward action NAMES are integrations/workbench's (Task 7:
// workbench.PipelineStepAction "pipelineStep", PipelineStatusAction "pipelineStatus",
// PipelineAckAction "pipelineAck", PipelineCancelAction "pipelineCancel"); this
// package never re-declares them (ledger Ruling R3).

// The step's identity and contract come from the seam (pl = component/pipelines):
// pl.Repository, pl.Service, pl.Failure, pl.Where, pl.StepResult. StepRun is
// what the AGENT computes from a pl.StepRequest and the runner needs; it is the
// pipelineStep forward's payload.
type StepRun struct {
	RunID          string                `json:"runId"`     // v1:pipelines:run, bare -- the log subject
	WorkRunID      string                `json:"workRunId"` // v1:work:run, bare -- producedByRunId
	StepKey        string                `json:"stepKey"`   // == v1:work:step key -- producedByStepKey
	Attempt        int                   `json:"attempt"`
	OwnerUserID    string                `json:"ownerUserId"`
	Repository     pl.Repository         `json:"repository"`
	SHA            string                `json:"sha"`
	InstallationID int64                 `json:"installationId,omitempty"` // 0 = anonymous clone of a public repository
	Image          string                `json:"image"`
	Command        string                `json:"command"`
	Env            map[string]string     `json:"env"`               // StepRequest.Environment() minus the secrets
	Secrets        map[string]string     `json:"secrets,omitempty"` // resolved values; never logged
	Services       map[string]pl.Service `json:"services,omitempty"`
	Caches         []string              `json:"caches,omitempty"`    // known: go, npm
	Artifacts      []string              `json:"artifacts,omitempty"` // relative paths or globs
	TimeoutSeconds int                   `json:"timeoutSeconds"`      // effective: min(step timeout, run remaining)
	DeadlineCode   string                `json:"deadlineCode"`        // pl.CodeStepTimeout | pl.CodeRunCeiling
	GoTimings      bool                  `json:"goTimings,omitempty"` // parse the output with pl.ParseGoTestOutput
}

// The runner answers pipelineStep with a pl.StepResult (JSON). LogTail is ONE
// string of at most the last 40 lines; Notes (ClassNote codes) when the seam adds
// the field, else the note sentence ends the log tail.

type StatusRequest struct {
	JobName string `json:"jobName"`
}

type StatusReply struct {
	State  string         `json:"state"` // running | finished | absent | stale
	Result *pl.StepResult `json:"result,omitempty"`
}

type AckRequest struct {
	JobName string `json:"jobName"`
}

type CancelRequest struct {
	RunID string `json:"runId"`
}
```

`config.go`:

```go
type Config struct {
	Namespace          string        // MEMQL_PIPELINES_NAMESPACE, default "memql-pipelines"
	CloneImage         string        // MEMQL_PIPELINES_CLONE_IMAGE, required on the workbench (refuse to run steps when empty)
	CacheClaim         string        // constant "memql-pipelines-cache"
	StepServiceAccount string        // constant "memql-pipelines-step"
	RunCeiling         time.Duration // MEMQL_PIPELINES_RUN_MAX_MINUTES, default 120, clamp 5..1440
	LogStoreMaxLines   int           // MEMQL_PIPELINES_LOG_STORE_MAX_LINES, default 2000, clamp 100..100000
	ArtifactMaxBytes   int64         // MEMQL_PIPELINES_ARTIFACT_MAX_BYTES, default 64 MiB, clamp 1 MiB..256 MiB
	ArchiveMaxBytes    int64         // constant 64 MiB
	DefaultStepTimeout time.Duration // constant 20 min (D11)
	JobTTL             time.Duration // constant 30 min (backstop; the ack deletes sooner)
	ScheduleTimeout    time.Duration // constant 10 min
	PollInterval       time.Duration // constant 2 s
	HeartbeatInterval  time.Duration // constant 10 s
	HeartbeatStale     time.Duration // constant 45 s
	NodeID             string        // MEMQL_NODE_ID or hostname (the same rule as workbench selfNodeId)
}

func ConfigFromEnv(getenv func(string) string) Config
```

Register `MEMQL_PIPELINES_NAMESPACE`, `MEMQL_PIPELINES_CLONE_IMAGE`, `MEMQL_PIPELINES_RUN_MAX_MINUTES`, `MEMQL_PIPELINES_LOG_STORE_MAX_LINES`, `MEMQL_PIPELINES_ARTIFACT_MAX_BYTES` in `scripts/secrets/manifest.yaml` (component `pipelines`, scope `node`, `optional: true`, defaults as above, one-sentence descriptions; follow the `MEMQL_WORK_SYSTEM_RUN_RETENTION_DAYS` entry), then `make env-registry-sync`.

`names.go`:

```go
// JobName is deterministic per (run, step, attempt): a retried step gets a fresh Job, a
// resumed driver re-attaches to the same one. "mp-" + 24 hex of sha256(runId|stepKey|attempt).
func JobName(runID, stepKey string, attempt int) string
func SecretName(jobName string) string // jobName + "-env"

// Labels every object the runner creates carries; LabelRun selects a run for cancel.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by" // value "memql-workbench"
	LabelRun       = "memql.io/pipelines-run"       // RunID (bare ids are DNS-label safe; hash them if longer than 63)
	LabelAttempt   = "memql.io/attempt"
	AnnotStepKey   = "memql.io/step-key"
	AnnotWorkRun   = "memql.io/work-run"
	AnnotOwner     = "memql.io/owner-user"
	AnnotRunner    = "memql.io/runner"        // "<nodeId> <RFC3339 heartbeat>"
	AnnotLogCursor = "memql.io/log-cursor"    // RFC3339Nano of the last line captured
	AnnotOutcome   = "memql.io/outcome"       // pl.StepResult JSON, written before the reply is sent
)
```

`kubetypes.go`: the minimal JSON structs the runner sends and reads, field names exactly the Kubernetes API's (`Job{APIVersion, Kind, Metadata, Spec, Status}`, `JobSpec{BackoffLimit *int32, ActiveDeadlineSeconds *int64, TTLSecondsAfterFinished *int32, Template PodTemplateSpec}`, `PodSpec{RestartPolicy, ServiceAccountName, AutomountServiceAccountToken *bool, EnableServiceLinks *bool, SecurityContext, InitContainers, Containers, Volumes}`, `Container{Name, Image, Command, Args, WorkingDir, Env, VolumeMounts, RestartPolicy *string, StartupProbe, SecurityContext}`, `EnvVar{Name, Value, ValueFrom}`, `Secret`, `Pod{Status{Phase, Conditions, InitContainerStatuses, ContainerStatuses}}`, `ContainerStatus{Name, State{Waiting{Reason,Message}, Running{StartedAt}, Terminated{ExitCode, Reason, Message, StartedAt, FinishedAt}}}`, `JobStatus{Conditions{Type, Status, Reason, Message}, StartTime, CompletionTime, Active, Succeeded, Failed}`, `ObjectMeta{Name, Namespace, Labels, Annotations, ResourceVersion, UID, OwnerReferences, CreationTimestamp}`). Only fields the code reads or writes; `omitempty` everywhere a zero would change API semantics.

`jobspec.go`:

```go
// BuildSecret holds the clone token (key GIT_TOKEN, mounted ONLY into the clone init
// container) and each resolved secret (keys = the env names, mounted into the step container
// by explicit secretKeyRef). Created before the Job, owned by it once the Job exists.
func BuildSecret(cfg Config, run StepRun, jobName, cloneToken string) Secret

// BuildJob is a pure function of its inputs.
func BuildJob(cfg Config, run StepRun, jobName string) (Job, error)
```

BuildJob rules (each one is a test case in `jobspec_test.go`):
- `backoffLimit: 0`, `activeDeadlineSeconds: run.TimeoutSeconds` (refuse <= 0), `ttlSecondsAfterFinished: cfg.JobTTL`.
- Pod: `restartPolicy: Never`, `serviceAccountName: cfg.StepServiceAccount`, `automountServiceAccountToken: false`, `enableServiceLinks: false`, pod `securityContext.seccompProfile.type: RuntimeDefault`.
- Init `clone`: image `cfg.CloneImage`; `command: ["/bin/sh","-c", cloneScript]`; env `CLONE_URL`, `SHA`, `GIT_TOKEN` (secretKeyRef, optional:true so an anonymous public clone works with an empty key absent); mounts `workspace` at `/workspace`; `allowPrivilegeEscalation: false`. `cloneScript`:

```sh
set -eu
cd /workspace
git init -q .
if [ -n "${GIT_TOKEN:-}" ]; then
  auth="$(printf 'x-access-token:%s' "$GIT_TOKEN" | base64 | tr -d '\n')"
  git -c "http.extraheader=AUTHORIZATION: basic ${auth}" fetch -q --depth=1 "$CLONE_URL" "$SHA"
else
  git fetch -q --depth=1 "$CLONE_URL" "$SHA"
fi
git checkout -q FETCH_HEAD
echo "memql: checked out $SHA"
```

- One native sidecar per service (sorted by name for determinism): an init container named `svc-<name>` with `restartPolicy: Always`, the service image, its env as plain values, and when `Ready != ""` a `startupProbe: {exec: {command: ["/bin/sh","-c", Ready]}, periodSeconds: 2, failureThreshold: 90}`.
- Main `step`: image `run.Image`; `command: ["/bin/sh","-c", stepWrapper]`; `workingDir: /workspace`; env = `run.Env` (sorted) + `MEMQL_STEP_COMMAND` (= run.Command) + `MEMQL_STEP_ARTIFACTS` (space-joined, only when non-empty) + `MEMQL_ARTIFACT_MARKER` (= `"::memql-artifacts::" + 16 hex derived from jobName`) + cache env + one `secretKeyRef` per secret key; mounts `workspace` and, when `len(run.Caches) > 0`, `cache` at `/cache`; `allowPrivilegeEscalation: false`.
- Cache env: `go` -> `GOMODCACHE=/cache/go/mod`, `GOCACHE=/cache/go/build`; `npm` -> `npm_config_cache=/cache/npm`; an unknown cache name is an error (`BuildJob` refuses; the seam's compiler should already have).
- Volumes: `workspace` emptyDir; `cache` PVC `cfg.CacheClaim` only when caches are declared.
- Labels/annotations from `names.go`; `metadata.namespace: cfg.Namespace`.
- Refusals (error, never a half-built Job): empty image, empty command, artifact path absolute or containing `..` or whitespace or any of `;&|$\`'"<>`, a secret name not a valid env name, a secret key colliding with a contract env name.

`wrapper.go`:

```go
// stepWrapper runs the manifest's command and, when artifacts are declared, frames them on
// stdout as base64 of a tar.gz between two marker lines. POSIX sh only; tar and base64 are
// needed only when artifacts are declared. The step's own exit status is what the
// container exits with, whatever the framing does.
const stepWrapper = `cd /workspace || exit 70
/bin/sh -c "$MEMQL_STEP_COMMAND"
rc=$?
if [ -n "${MEMQL_STEP_ARTIFACTS:-}" ]; then
  echo "$MEMQL_ARTIFACT_MARKER begin"
  # Word-split on purpose: the list is validated server-side (no spaces, no metacharacters)
  # and globs are allowed to expand.
  tar -czf - -- $MEMQL_STEP_ARTIFACTS 2>/dev/null | base64
  echo "$MEMQL_ARTIFACT_MARKER end"
fi
exit $rc`
```

- [ ] **Step 1: Failing tests** -- one `t.Run` per BuildJob rule above (assert on the struct, then `json.Marshal` and assert the key fields' JSON names: `activeDeadlineSeconds`, `automountServiceAccountToken`, `enableServiceLinks`, `restartPolicy` on the sidecar init container); `TestJobNameIsDeterministicAndDNSSafe` (same inputs -> same name; different attempt -> different; `len <= 63`, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`); `TestTheCloneTokenReachesOnlyTheCloneContainer` (no container but `clone` references key `GIT_TOKEN`); `TestSecretsAreRefsNeverValuesInTheJob` (no secret VALUE appears anywhere in `json.Marshal(job)`); `TestConfigFromEnvDefaultsAndClamps`.
- [ ] **Step 2: Run** `go test ./integrations/pipelinesteps/ -count=1` (from `integrations/`, or `go test github.com/znasllc-io/memql/integrations/pipelinesteps/...`) -- FAIL.
- [ ] **Step 3: Implement** the files above.
- [ ] **Step 4: Run** -- PASS; `make env-registry-check` PASS.
- [ ] **Step 5: Commit** `Issue #5493: the step's Job and Secret as pure functions -- init clone at the SHA, the manifest image, native sidecars, caches, no cluster credential`.

### Task 4: integrations/pipelinesteps -- the Kubernetes calls and the status classifier (#5493)

**Files:**
- Create: `integrations/pipelinesteps/kube.go`, `classify.go`
- Test: `integrations/pipelinesteps/kube_test.go` (httptest fake API server), `classify_test.go`

**Interfaces (produces):**

```go
type Kube struct{ api *deploycontrol.ClusterAPI; ns string }

func NewKube(api *deploycontrol.ClusterAPI, namespace string) *Kube
func (k *Kube) CreateSecret(ctx context.Context, s Secret) error                       // 409 -> nil (re-attach)
func (k *Kube) OwnSecret(ctx context.Context, secretName string, job Job) error        // merge-patch ownerReferences -> the Job (controller=false, blockOwnerDeletion=false)
func (k *Kube) CreateJob(ctx context.Context, j Job) (Job, bool, error)                // created=false when it already existed (then the existing Job is returned)
func (k *Kube) GetJob(ctx context.Context, name string) (Job, error)                   // deploycontrol.IsNotFound for absent
func (k *Kube) AnnotateJob(ctx context.Context, name string, annots map[string]string, resourceVersion string) (Job, error) // merge-patch; resourceVersion "" = unconditional
func (k *Kube) DeleteJob(ctx context.Context, name string) error                       // propagationPolicy=Background; 404 -> nil
func (k *Kube) DeleteSecret(ctx context.Context, name string) error                    // 404 -> nil
func (k *Kube) DeleteRun(ctx context.Context, runID string) (int, error)               // deletecollection jobs + secrets by LabelRun; returns jobs matched
func (k *Kube) JobPod(ctx context.Context, jobName string) (*Pod, error)               // newest pod with label job-name=<jobName>; nil when none yet
func (k *Kube) FollowLog(ctx context.Context, pod, container string, since time.Time) (io.ReadCloser, error) // follow=true&timestamps=true[&sinceTime=]
func (k *Kube) TailLog(ctx context.Context, pod, container string, lines int) (string, error) // tailLines=, not followed
```

`classify.go` -- a pure function the runner's poll loop calls:

```go
type Phase int

const (
	PhasePending   Phase = iota // not scheduled yet / pulling / init running
	PhaseRunning                // the step container is running (follow its log)
	PhaseSucceeded              // terminal
	PhaseFailed                 // terminal, Failure set
)

type Observation struct {
	Phase    Phase
	ExitCode int      // step container's exit code when terminated, else -1
	Failure  *pl.Failure // set when PhaseFailed
	Detail   string   // human sentence for the log ("waiting: ImagePullBackOff ...")
}

// Classify reads the Job and its pod. created is when the runner created (or adopted)
// the Job; now and cfg decide the scheduling timeout. deadlineCode is the StepRun's.
func Classify(job Job, pod *Pod, created, now time.Time, cfg Config, deadlineCode string) Observation
```

Classification table (each row one `classify_test.go` case, named for the row):

| Condition | Result |
|---|---|
| Job condition `Failed` with reason `DeadlineExceeded` | Failed, `deadlineCode` |
| any container `waiting.reason` in {`ErrImagePull`, `ImagePullBackOff`, `InvalidImageName`} | Failed, `pipeline_image_pull_failed` (message names the image) |
| `waiting.reason == CreateContainerConfigError` | Failed, `pipeline_job_rejected` |
| clone init container terminated with non-zero exit | Failed, `pipeline_clone_failed` |
| a `svc-*` sidecar terminated, or its startupProbe failed so the pod failed before `step` started | Failed, `pipeline_service_failed` |
| pod `PodScheduled=False` reason `Unschedulable` for longer than `cfg.ScheduleTimeout` since `created` | Failed, `pipeline_job_unschedulable` |
| `step` container terminated exit 0 (or Job `Complete`) | Succeeded, ExitCode 0 |
| `step` container terminated exit N != 0 | Failed, ExitCode N, Failure nil (a command failure carries no code: the exit code IS the answer) |
| `step` container running | Running |
| otherwise | Pending |

- [ ] **Step 1: Failing tests**: `classify_test.go` table above (build Job/Pod fixtures as Go values); `kube_test.go` against an `httptest.Server` that records method, path, query and body and answers canned JSON: `TestCreateJobReturnsTheExistingOneOnConflict`, `TestDeleteRunSelectsByRunLabel` (asserts `DELETE .../jobs?labelSelector=memql.io%2Fpipelines-run%3D<id>&propagationPolicy=Background` and the secrets collection), `TestFollowLogAsksForFollowAndTimestamps`, `TestAnnotateJobSendsMergePatchWithResourceVersion` (content type `application/merge-patch+json`, body carries `metadata.resourceVersion`).
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement.** Paths: `apis/batch/v1/namespaces/<ns>/jobs[/<name>]`, `api/v1/namespaces/<ns>/secrets[/<name>]`, `api/v1/namespaces/<ns>/pods?labelSelector=job-name%3D<name>`, `api/v1/namespaces/<ns>/pods/<pod>/log?container=<c>&follow=true&timestamps=true[&sinceTime=<RFC3339>]`. Query values through `url.Values`.
- [ ] **Step 4: Run** -- PASS.
- [ ] **Step 5: Commit** `Issue #5493: the runner's Kubernetes calls over ClusterAPI and a pure Job/pod classifier`.

### Task 5: integrations/pipelinesteps -- log capture, masking, caps and artifact frames (#5495)

**Files:**
- Create: `integrations/pipelinesteps/capture.go`, `artifacts.go`
- Test: `integrations/pipelinesteps/capture_test.go`, `artifacts_test.go`

**Interfaces (produces):**

```go
// LineSink is the log store (logger.CurrentSink() on the workbench node).
type LineSink interface{ Write(logger.Line) }

type Capture struct { /* unexported */ }

type CaptureOptions struct {
	RunID, WorkRunID, StepKey string
	Secrets        []string // values to mask
	Marker         string   // the StepRun's artifact marker
	StoreMaxLines  int
	ArchiveMax     int64
	ArtifactMax    int64
	ArchivePath    string // a temp file the capture owns
	Sink           LineSink
	StoreRate      int // lines per second to the store, default 200 (shares a per-node bucket)
}

func NewCapture(o CaptureOptions) (*Capture, error)
// Feed consumes one raw log line as Kubernetes returns it with timestamps=true
// ("<RFC3339Nano> <text>"); it returns the line's timestamp for the log cursor.
func (c *Capture) Feed(raw string) time.Time
// Note writes a runner-authored line (clone section header, adoption notice, cap notice).
func (c *Capture) Note(text string)
func (c *Capture) Close() (CaptureResult, error)

type CaptureResult struct {
	Lines        int
	StoreCapped  bool
	ArchivePath  string // complete up to ArchiveMax, then a truncation line
	ArchiveBytes int64
	Tail         string // the last 40 lines joined by \n, masked
	Artifacts    []byte   // decoded tar.gz, nil when no frame
	ArtifactNote *pl.Failure // pl.CodeArtifactMissing (no frame / empty) or pl.CodeArtifactTooLarge
}

type ArtifactFile struct {
	Path  string
	Bytes []byte
}

// ExtractArtifacts unpacks a tar.gz within limits: regular files only, no absolute paths,
// no "..", total bytes <= max; declared paths that matched nothing are returned in missing.
func ExtractArtifacts(tgz []byte, declared []string, max int64) (files []ArtifactFile, missing []string, err error)
```

Rules (one test each):
- `TestCaptureMasksSecretValues`: every secret value of length >= 4 is replaced with `***` in the store line, the archive, the tail (Review Focus 1).
- `TestCaptureSplitsLongLinesAndRepairsUTF8`: a 10 000-byte line becomes 3 store lines of <= 4096 bytes; invalid UTF-8 becomes U+FFFD (Review Focus 5).
- `TestCaptureCapsTheStoreCopy`: after `StoreMaxLines` lines the store gets exactly one more line naming `pipeline_log_capped` and the Library, and nothing after; the archive keeps everything; `StoreCapped` true.
- `TestCaptureArchiveTruncatesWithAMarker`: beyond `ArchiveMax` the archive ends with one line saying how many bytes were dropped.
- `TestCaptureStoreLinesCarryTheRunSubject`: `Subject == RunID`, `SubjectConcept == "v1:pipelines:run"`, `Component == "pipelines.step"`, attributes `{"stepKey": ..., "workRunId": ...}`, `At` = the Kubernetes timestamp.
- `TestArtifactFrameIsNotLogged`: lines between `<marker> begin` and `<marker> end` reach neither the store nor the archive nor the tail; their base64 is decoded into `Artifacts`.
- `TestArtifactFrameMissingIsANoteNotAFailure` (Review Focus 3): artifacts declared, no frame -> `ArtifactNote.Code == "pipeline_artifact_missing"`.
- `TestArtifactFrameOverTheCapIsDropped`: decoded bytes beyond `ArtifactMax` -> `pipeline_artifact_too_large`, `Artifacts` nil, capture continues.
- `TestExtractArtifactsRefusesTraversal` (Review Focus 5): entries `../x`, `/etc/passwd`, a symlink, a device -> skipped and reported; regular files returned.
- `TestExtractArtifactsReportsMissingDeclaredPaths`.

- [ ] **Step 1: Write the tests above.**
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement.** The store writer is paced by a small token bucket (`StoreRate`); a line refused by pacing is still archived (the store copy is a convenience, the Library copy is the record). Tail is a ring of 40 masked lines.
- [ ] **Step 4: Run** -- PASS.
- [ ] **Step 5: Commit** `Issue #5495: step output into the log store under the run subject, masked and capped, the full log archived, artifacts framed out of the stream`.

### Task 6: integrations/pipelinesteps -- the runner on the workbench node (#5493, #5495)

**Files:**
- Create: `integrations/pipelinesteps/ports.go`, `runner.go`
- Test: `integrations/pipelinesteps/runner_test.go` (a fake API server that simulates Job and pod status transitions and serves a scripted log stream)

**Interfaces:**

```go
// ports.go -- implemented in app/ (Task 8), faked in tests.
type RunFile struct {
	OwnerUserID, WorkRunID, StepKey string
	Name, MimeType                  string
	Bytes                           []byte
}

type StoredFile struct {
	FileID  string
	Omitted string // why nothing was stored ("" when stored)
}

type LibraryStore interface {
	StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error)
}

type TokenMinter interface {
	// CloneToken: a short-lived token that can read owner/name. installationID 0 = "" (anonymous).
	CloneToken(ctx context.Context, installationID int64, owner, name string) (string, error)
}

// runner.go
type Runner struct { /* cfg, kube, sink, library, tokens, clock, inflight */ }

func NewRunner(cfg Config, kube *Kube, sink func() LineSink, library LibraryStore, tokens TokenMinter) *Runner
func (r *Runner) Run(ctx context.Context, run StepRun) pl.StepResult
func (r *Runner) Status(ctx context.Context, req StatusRequest) StatusReply
func (r *Runner) Ack(ctx context.Context, req AckRequest) error
func (r *Runner) CancelRun(ctx context.Context, req CancelRequest) (int, error)
```

`Run` algorithm (the multi-replica contract; every branch a test):
1. `jobName := JobName(run.RunID, run.StepKey, run.Attempt)`. `GetJob`.
2. **Exists with `AnnotOutcome`** -> return it (the reply was lost; the agent asked again).
3. **Exists with a fresh `AnnotRunner` from another node** (heartbeat younger than `HeartbeatStale`) -> wait: poll `GetJob` every `PollInterval` until `AnnotOutcome` appears (return it), the heartbeat goes stale (go to 5), or the Job vanishes (return Failed `pipeline_node_lost`, ExitCode -1).
4. **Absent** -> mint the clone token (`tokens.CloneToken`; error -> Failed `pipeline_clone_failed` with the message, never a half-created Job), `CreateSecret(BuildSecret(...))`, `CreateJob(BuildJob(...))`; on `IsForbiddenQuota` wait `PollInterval * 5` and retry until `ctx` is done (Review Focus 4; write one Note "waiting for a free slot under the pipelines ceiling" once); then `OwnSecret`.
5. **Take ownership**: `AnnotateJob` with `AnnotRunner = "<cfg.NodeID> <now>"` conditioned on the read `resourceVersion` (a 409 means another replica won the race: go to 3). Start a heartbeat goroutine that re-annotates `AnnotRunner` and `AnnotLogCursor` every `HeartbeatInterval`.
6. **Watch**: every `PollInterval`, `JobPod` + `Classify`. While Pending, nothing to follow. On the first Running, `FollowLog(pod, "step", cursor)` where `cursor` is the Job's `AnnotLogCursor` (adoption) or zero (fresh); feed every line to `Capture`. When adopting, first write `Note("re-attached on <node>; lines before <cursor> are in the store already")`. A stream that ends while the container still runs is re-opened from the last cursor.
7. **Terminal**: after Succeeded/Failed, capture the clone container's output (`TailLog(pod, "clone", 200)`) and, on Failed, each `svc-*` container's last 50 lines, as runner Notes into the archive (they never enter the store).
8. **Finalize**: `capture.Close()`; store the archive (`<stepKey>.log`, `text/plain; charset=utf-8`) and each extracted artifact (name = its path with `/` replaced by `__`, mime by extension or `application/octet-stream`) through `LibraryStore` under `run.OwnerUserID`; an `Omitted` answer becomes a Note, never a failure. Build the `pl.StepResult` (status, exit code, failure, notes, started/finished from the step container's terminated state, `Where{Surface:"cluster", NodeID: cfg.NodeID, JobName}`, file ids, tail, counts; `Timings = pl.ParseGoTestOutput(archive)` when `run.GoTimings` and the seam exports it).
9. **Persist before replying**: `AnnotateJob(AnnotOutcome = json(outcome))` (unconditional); then return. The Job and Secret are deleted by `Ack` (the agent has the outcome) or by the Job's TTL.
10. **Cancellation**: `ctx` done because the forward was CANCELLED (WorkbenchForwardCancel) -> `DeleteJob` + `DeleteSecret`, status `cancelled` / `pl.CodeStepCancelled` with whatever was captured archived. `CancelRun` deletes every Job and Secret of the run by label and cancels this replica's in-flight `Run` contexts for it; a `Run` that sees its Job vanish without a cancel of its own returns `cancelled` too (another replica cancelled it).

`Status`: `GetJob`: absent -> `absent`; `AnnotOutcome` -> `finished` + outcome; fresh `AnnotRunner` -> `running`; else `stale`. `Ack`: `DeleteJob` + `DeleteSecret` (404s are success).

- [ ] **Step 1: Failing tests** (fake API server driven by a small state machine; a fake `LibraryStore` recording files; a fake sink; a fixed clock): `TestRunnerRunsAStepToSuccessAndArchivesItsLog`, `TestRunnerReportsTheCommandExitCode`, `TestRunnerReturnsAPersistedOutcomeWithoutRerunning`, `TestRunnerWaitsWhileAnotherReplicaHoldsAFreshHeartbeat`, `TestRunnerAdoptsAJobWhoseRunnerWentStale` (Review Focus 2: follows from the cursor, writes the re-attach note, does not create a second Job), `TestRunnerWaitsOutAnExceededQuota` (Review Focus 4), `TestRunnerCancelDeletesTheJobAndSecret`, `TestRunnerCloneTokenFailureCreatesNothing`, `TestRunnerStoresArtifactsUnderTheOwner`, `TestRunnerOmittedLibraryFileIsANote`, `TestAckDeletesJobAndSecret`, `TestStatusStates`.
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/integrations/pipelinesteps/... -count=1 -race` -- PASS.
- [ ] **Step 5: Commit** `Issue #5493: the workbench runner -- create or adopt, heartbeat, follow, capture, persist the outcome on the Job before replying`.

### Task 6b: the runner proves the namespace is isolated before it runs a step (#5493)

Ledger Ruling R12. On AKS as provisioned (no `--network-policy`) the memql-pipelines NetworkPolicy is inert: a step could reach IMDS (the kubelet identity), the mesh's in-cluster-only `/metrics` and the database Service. The runner therefore PROVES isolation empirically, with a reachable positive, before the first step it runs on each replica, and refuses every step while the proof fails.

**Files:**
- Create: `integrations/pipelinesteps/isolation.go`, `isolation_test.go`
- Modify: `integrations/pipelinesteps/runner.go` (gate `Run` on the proof), `jobspec.go` (the two probe Jobs as pure builders)

**Interfaces:**

```go
// IsolationProof is cached per Runner: a pass is trusted for cfg.IsolationTTL (1 h);
// a failure or an inconclusive probe is re-tried on the next Run (never cached as a pass).
type IsolationVerdict struct {
	Isolated bool
	Detail   string // human sentence for the refusal message and the log
	At       time.Time
}

func (r *Runner) proveIsolation(ctx context.Context) IsolationVerdict
func BuildIsolationListener(cfg Config, name string) Job               // clone image; `nc -lk -p 8080` or busybox httpd; no service account token; labels LabelManagedBy + memql.io/probe=listener
func BuildIsolationConnector(cfg Config, name, targetIP string) Job    // clone image; exits 0 when it CAN connect to targetIP:8080 within 5 s, 1 when it cannot
```

Algorithm: create the listener Job, wait until its pod has a podIP and is Running (bounded 90 s; inconclusive otherwise); create the connector Job with that IP; wait for its terminal state (bounded 60 s). Connector exit 1 (could not connect) -> Isolated. Exit 0 -> NOT isolated: every step is refused `pl.CodeIsolationUnenforced` (the seam adds the constant; until it does use the literal "pipeline_isolation_unenforced" behind a TODO-free constant in this package that a later commit swaps) with the Detail naming the fix (enable a network policy engine; on AKS `az aks update --network-policy`). Both probe Jobs are deleted after the verdict (defer, background propagation). The probe Jobs count against the ceiling quota; on `IsForbiddenQuota` the probe waits like a step does.

- [ ] Tests (fake API server): `TestIsolationProofPassesWhenTheConnectorCannotConnect`, `TestIsolationProofRefusesStepsWhenTheConnectorConnects`, `TestIsolationProofIsCachedOnlyOnPass`, `TestIsolationProofDeletesItsProbeJobs`, `TestIsolationProofInconclusiveIsNotAPass`.
- [ ] Verified live once on a THROWAWAY k3d cluster (never the shared `memql` one): with the component's NetworkPolicy applied the proof passes; with it deleted the proof refuses.
- [ ] Commit `Issue #5493: the runner proves memql-pipelines is isolated before it runs a step`.

### Task 7: integrations/workbench -- the forwarded pipeline actions, peer-loss, and the docker need (#5493, #5494)

**Files:**
- Modify: `integrations/workbench/forward_handler.go` (four actions, SYSTEM-class authority, async for `pipelineStep`)
- Modify: `integrations/workbench/forward_router.go` (`ForwardWatched`: returns `ErrWorkbenchPeerLost` when the serving peer stops being healthy or connected while waiting, WITHOUT sending a cancel)
- Modify: `integrations/workbench/environment.go` (`NeedDocker = "docker"`, not provided by the workbench), `integrations/agent/worker/scope.go` (`docker` -> label `docker=true`), `integrations/skills/runscript.go` (`needsBeyondWorkbench` gains docker)
- Test: `integrations/workbench/forward_pipeline_test.go`, `environment_test.go` (docker case), `integrations/agent/worker/scope_test.go`

**Interfaces:**

```go
// forward_handler.go -- implemented by *pipelinesteps.Runner through a small adapter in app/
// (Task 8); JSON in, JSON out, so integrations/workbench does not import pipelinesteps.
// The action names, owned here (ledger Ruling R3).
const (
	PipelineStepAction   = "pipelineStep"   // long: runs or adopts the step's Job to completion
	PipelineStatusAction = "pipelineStatus" // immediate: running | finished | absent | stale
	PipelineAckAction    = "pipelineAck"    // immediate: delete Job + Secret
	PipelineCancelAction = "pipelineCancel" // immediate: delete every Job of a run
)

type PipelineRunner interface {
	RunStep(ctx context.Context, argsJSON []byte) (outcomeJSON []byte)
	Status(ctx context.Context, argsJSON []byte) (replyJSON []byte, errorCode string)
	Ack(ctx context.Context, argsJSON []byte) (errorCode string)
	CancelRun(ctx context.Context, argsJSON []byte) (replyJSON []byte, errorCode string)
}

func (h *ForwardHandler) SetPipelineRunner(p PipelineRunner)

// forward_router.go
var ErrWorkbenchPeerLost = errors.New("workbench peer lost while waiting")

// ForwardWatched is Forward plus a liveness watch: every interval it re-checks the peer it
// sent to; an unhealthy or disconnected peer ends the wait with ErrWorkbenchPeerLost and NO
// cancel (the work is meant to survive and be adopted).
func (r *ForwardRouter) ForwardWatched(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string, interval time.Duration) (*nodev1.WorkbenchForwardResponse, string, error)
```

Handler rules (tests): the four actions are accepted only under `auth.ForwardedClassSystem` (anything else -> `forwarded_authority_refused`, nothing run); with no runner set -> `pipelines_not_configured`; `pipelineStep` runs in a goroutine registered in `inflight` (so `WorkbenchForwardCancel` cancels it) and `HandleForwardedRequest` returns immediately; the others answer synchronously. Response `payload_json` is the runner's JSON; `error_code` mirrors a refusal.

- [ ] **Step 1: Failing tests**: `TestPipelineStepDoesNotBlockTheReceiveLoop` (HandleForwardedRequest returns before a blocking fake runner finishes; the response arrives later via `send`), `TestPipelineActionsNeedSystemAuthority`, `TestForwardCancelReachesARunningPipelineStep`, `TestForwardWatchedReturnsPeerLostWithoutCancel`, `TestDockerIsAKnownNeedTheWorkbenchCannotMeet` (`parseEnvironmentHint` accepts docker; the mismatch lists it), `TestDockerNeedMapsToTheDockerLabel`.
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement.** In `HandleForwardedRequest`, fork for the four actions right after the authority gate, like `BuildAction`. The `pipelineStep` goroutine owns the `inflight` entry and its cleanup (the existing deferred delete must not run for it -- restructure so the async path removes its own entry).
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/integrations/workbench/... github.com/znasllc-io/memql/integrations/agent/worker/... -count=1` (the second with `-tags agent` as well) -- PASS.
- [ ] **Step 5: Commit** `Issue #5493: the workbench answers pipeline step, status, ack and cancel forwards without blocking its stream; docker joins the closed need set`.

### Task 8: the Library half, the OS labels, and the workbench wiring (#5495)

**Files:**
- Modify: `dsl/library/concepts.memql` (`file.source` and `artifact.source` gain `"pipeline"`; additive)
- Modify: `clients/os/src/apps/files/rows.ts` (`SOURCE_SENTENCES.pipeline = "Made by a pipeline run"`), `clients/os/src/items/provenance.ts` (`CLUSTER_SOURCES` + label "Made by a pipeline run"), `clients/os/src/apps/files/concepts.ts` (`SOURCE_VALUES` gains `"pipeline"`, keep the concept's order), `clients/os/src/apps/deployables/DeployablesApp.tsx` (`DEPLOYABLES_LOG_CONCEPTS` gains `"v1:pipelines:run"`)
- Create: `app/pipelines_library_store.go` (implements `pipelinesteps.LibraryStore`, mirrors `app/appsession_content_store.go`)
- Create: `app/pipelines_runner_adapter.go` (implements `workbench.PipelineRunner` over `*pipelinesteps.Runner`)
- Modify: `app/integrations_workbench.go` (construct the runner on the workbench node when `MEMQL_PIPELINES_CLONE_IMAGE` is set and `deploycontrol.InClusterAvailable()`; otherwise log once that pipeline steps cannot run on this node and leave the runner unset -> `pipelines_not_configured`) and `app/cluster_workbench.go` (`SetPipelineRunner` on the workbench `ForwardHandler`)
- Create: `app/pipelines_token_minter.go` (implements `pipelinesteps.TokenMinter` with the GitHub App's installation token for one repository, contents read; use the seam's minting function once it lands -- until then `component/packages/githubapp.InstallationToken`)
- Test: `app/pipelines_library_store_test.go`, `clients/os/test/files/pipelineSource.test.ts`, regenerate: `make concept-snapshot`, `make sdk-gen`, `make arch-model`, `make platform-graph`

`StoreRunFile` rules (tests): blank owner -> error (never a blank-actor row); blob path `library/<owner>/<fileId>/<name>` through `server.FileUploader`; `createLibraryFile` under `auth.ContextWithUserActor(ctx, owner)` with `source: "pipeline"`, `producedByRunId: WorkRunID`, `producedByStepKey: StepKey`, `format: server.LibraryFormatForMIME(mime)`; over the owner's quota (`StorageFootprint` vs `server.LibraryUserQuotaBytes()`) or above `server.LibraryMaxUploadBytes()` -> `Omitted` with the reason and nothing written; no storage configured -> `Omitted`. Then `setLibraryFileStatus(status: "ready")` like `integrations/compose/materialize.go`.

UI: the new source renders "Made by a pipeline run" in the Files app (list sentence and provenance origin), tone `reachable` like every cluster source; the Deployables per-app Logs section includes step output. Judge the two surfaces as rendered (light and dark, one populated row each) through the OS QA harness; no layout change.

- [ ] **Step 1: Failing tests**: the Go store tests above with a fake engine + uploader; the TS test asserting `fileStory`/`deriveProvenance` for `source: "pipeline"`.
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement**; regenerate the derived artifacts listed above.
- [ ] **Step 4: Run** `make test` (the whole tree) and `cd clients/os && npx vitest run test/files` -- PASS.
- [ ] **Step 5: Commit** `Issue #5495: pipeline files land in the owner's Library as source pipeline, bound to the work run and step; the workbench node wires the runner`.

### Task 9: the fleet half in the engine -- a pipeline-purpose dispatch (#5494)

**Files:**
- Modify: `integrations/agent/worker/dispatch.go` (`Request.Purpose`; `Result.WorkerId`, `Result.NodeId`, `Result.Labels`)
- Modify: `integrations/agent/worker/integration.go` / the action tables (`pipeline_step` in the `workerHost` action set; scope `full`, capability as `exec`)
- Modify: `integrations/agent/worker/safety_descriptor.go` (no classifier descriptor for the pipeline purpose -- see rule 3)
- Modify: `component/worker/hardware.go` (a docker runtime also derives the exact label `docker=true`)
- Modify: the invocation recorder (`argsRedacted` never carries `token` or `secrets` values for `pipeline_step`)
- Test: `integrations/agent/worker/pipeline_purpose_test.go`, `component/worker/hardware_test.go`

Gate rules for `Request.Purpose == PurposePipeline` (each a test):
1. Accepted only under internal origin (`auth.IsInternalOrigin(ctx)`); otherwise `denied_pipeline_purpose`.
2. `RunId` and `OwnerUserId` required; `AgentId` NOT required and `AgentAuthorization` NOT consulted -- the pipeline's `compute: cluster_and_fleet` (checked by the seam before Execute) and the machine's policy are the two consents.
3. The safety classifier is not consulted: the command is the repository's own manifest at a pinned SHA, forks are refused upstream, and two owners opted in. The kill switch (`computerUseEnabled`) still applies -> `kill_switch_engaged`.
4. `RequireLabels` must contain `pipelines=allowed` (set by Part 2's router); a request without it is refused `denied_pipeline_purpose` (no laptop by default).
5. The `Result` names the machine: `WorkerId` (registration id), `NodeId` (the replica that held the stream), `Labels`.
6. A machine whose stream a sibling replica holds and that cannot be reached is skipped (`FallbackNextMatching`), never a failure, while another candidate remains.

- [ ] **Step 1: Failing tests** for rules 1-6 using the dispatcher's existing fakes (`dispatch_routing_test.go`, `owner_scope_test.go` patterns), plus `TestDockerRuntimeDerivesTheDockerLabel`.
- [ ] **Step 2: Run** with `-tags agent` -- FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -tags agent github.com/znasllc-io/memql/integrations/agent/worker/... -count=1` and `go test github.com/znasllc-io/memql/component/worker/... -count=1` -- PASS.
- [ ] **Step 5: Commit** `Issue #5494: a pipeline-purpose fleet dispatch -- no agent, two owner consents, the kill switch, the machine named on the result`.

### Task 10: memql-cockpit -- the pipeline_step action and the pipelines policy (#5494)

Repository `memql-cockpit` (own worktree off its `origin/main`, own branch `feat/pipeline-step`, own PR; it adds an action NAME inside the existing `ToolDispatch.args_json`, so no engine pin bump).

**Files (cockpit):**
- Create: `internal/worker/tools/pipeline_step.go`, `pipeline_step_test.go`
- Modify: `internal/worker/tools/dispatcher.go` (`case "pipeline_step"`), `internal/worker/tools/policy.go` (`Pipelines PipelinesPolicy yaml:"pipelines"` with `Allow bool yaml:"allow"`, `Repos []string yaml:"repos"`, `WorkspaceRoot string yaml:"workspace_root"`, `MaxTimeoutSec int yaml:"max_timeout_sec"` default 3600), the consent classifier (`pipeline_step` is `interact`, admitted by the policy rather than a consent window), the registration label (`pipelines: "allowed"` merged into the advertised labels when `pipelines.allow` is true), `docs/` (the policy reference)

Action contract (args, mirrors the Job):

```json
{"action":"pipeline_step","pipeline_step":{
  "cloneUrl":"https://github.com/o/r.git","sha":"<40 hex>","token":"<short-lived or empty>",
  "repository":"o/r","command":"go test ./...","env":{"MEMQL_RUN_ID":"..."},
  "secrets":{"NAME":"value"},"artifacts":["dist/report.xml"],"timeoutSec":1200}}
```

Behaviour (each a test): refuse `denied_by_policy` unless `pipelines.allow` and (when `repos` is non-empty) the repository is listed; make a fresh directory under `pipelines.workspace_root` (default the shell workspace root + `/pipelines`); `git init` + `git -c http.extraheader=... fetch --depth=1 <cloneUrl> <sha>` + `checkout FETCH_HEAD` (token never written to disk or argv: pass the header through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0` env); run `/bin/sh -c command` with `os.Environ()` + `env` + `secrets` (INHERITS the machine environment, unlike `exec`), process group, `timeoutSec` clamped to `max_timeout_sec`; stream stdout+stderr as `ToolStream` chunks (secrets masked); after exit, tar.gz the artifacts (same limits as the engine's: 64 MiB) and return `{"exitCode":N,"durationMs":...,"artifactsTgzBase64":"...","artifactsMissing":[...]}`; remove the directory.

- [ ] **Step 1: Failing tests** (policy refusals, a local bare git repo as the clone source, env inheritance, timeout clamp, artifact packing, secret masking in chunks, cleanup).
- [ ] **Step 2: Run** `make test` in the cockpit -- FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test && make lint` -- PASS.
- [ ] **Step 5: Commit, push, open the cockpit PR** (`Part of znasllc-io/memql#5494`), merge it through the cockpit's flow before the engine PR merges.

### Task 11: the toolchain image, built by workflow_dispatch (#5496)

**Files:**
- Create: `deploy/toolchain-image/Dockerfile` (Debian bookworm base pinned by digest; Go `1.27.1` matching `toolchain go1.27.1`; Node 22; `protoc` at the version `scripts/dev/proto-gen.sh` pins; git, make, bash, curl, tar, coreutils, ca-certificates, postgresql-client; non-root user `memql` uid 1000 with a writable `/workspace` and `/cache`)
- Create: `deploy/toolchain-image/smoke-test.sh` (go version, node --version, protoc --version, git --version, psql --version; exit non-zero on any miss)
- Create: `.github/workflows/build-toolchain-image.yml` (dispatch-only on `main`, input `version`; build, load, smoke-test, push `ghcr.io/znasllc-io/memql-toolchain:<version>`, print the digest and "pin this digest in memql-package.yaml" in the step summary; job-scoped `packages: write`; actions pinned by 40-hex SHA copied from `build-db-image.yml`)
- Modify: `.github/dependabot.yml` (docker directory `/deploy/toolchain-image`), `ci.yml` changes filters or the path-bucket exemption so `TestEveryTrackedFileReachesAConsumedBucket` passes
- Test: `scripts/ci/toolchain_image_workflow_test.go` (dispatch-only trigger, `packages: write` job-scoped, smoke test before push -- the `db_image_wiring_test.go` / `mirror_fixture_workflow_test.go` shapes)

The Postgres service image is the existing `ghcr.io/znasllc-io/ci-timescaledb` (TimescaleDB + pgvector, built by `mirror-fixture-images.yml`, digest-pinned in `ci.yml`); the pipeline references the same digest so the pipeline and the GitHub lane test against one database image until `ci.yml` retires.

- [ ] **Step 1: Failing test** `scripts/ci/toolchain_image_workflow_test.go`.
- [ ] **Step 2: Run** -- FAIL.
- [ ] **Step 3: Implement**; build locally once (`docker build deploy/toolchain-image`) and run the smoke test to prove the Dockerfile (never pushed from this machine).
- [ ] **Step 4: Run** `go test github.com/znasllc-io/memql/scripts/... -count=1` -- PASS.
- [ ] **Step 5: Commit** `Issue #5496: the toolchain image (Go, Node, protoc) built by workflow_dispatch and smoke-tested before push`.

---

## Part 2 -- wiring onto the seam (after component/pipelines and component/pipelinerun land)

Rebase: `git rebase origin/main` once the seam's PR has merged (or, to start earlier, branch a
scratch worktree off the seam's pushed tip and replay later with
`git rebase --onto origin/main <seam-tip-sha>`). `integrations/go.mod` requires
`github.com/znasllc-io/memql/component/pipelines` (nested module) with the workspace replace.

### Task 12: the Executor on the agent node -- forward, watch, re-attach, fleet, cancel, ceilings (#5493, #5494, #5496)

**Files:**
- Create: `integrations/pipelinesteps/executor.go`, `adapt.go`, `fleet.go` (`//go:build agent`), `fleet_stub.go` (`//go:build !agent`)
- Create: `app/pipelines_executor.go` (agent node: build and `pipelines.RegisterExecutor`)
- Test: `integrations/pipelinesteps/adapt_test.go` (StepRequest -> StepRun), `executor_test.go`, `executor_hop_test.go` (in-process: an executor on "agent" + two runners on "workbench-a/b" behind a fake forward transport)

```go
type Forwarder interface {
	ForwardWatched(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string, interval time.Duration) (*nodev1.WorkbenchForwardResponse, string, error)
	Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error)
}

type FleetRouter interface {
	RunStep(ctx context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error)
}

type Executor struct { /* cfg, fwd, fleet, authority func(ctx) ctx, inflight map[runID][]cancel */ }

func NewExecutor(cfg Config, fwd Forwarder, fleet FleetRouter, systemAuthority func(context.Context) context.Context) *Executor
func (e *Executor) Execute(ctx context.Context, req pl.StepRequest) (pl.StepResult, error)
func (e *Executor) Cancel(ctx context.Context, runID string) error
```

`Execute`:
1. Deadline: `runDeadline := runStartedAt + cfg.RunCeiling`; if `now >= runDeadline` -> failed `pipeline_run_ceiling`, exit -1, no Job. Step timeout = `step.TimeoutSeconds` or `cfg.DefaultStepTimeout`; effective = min(step, runDeadline-now); `DeadlineCode` names whichever bound.
2. A step whose `Kind` is not `pl.StepCommand` is refused `pl.CodeExecutorError` (notify steps are the driver's). Any need with `!pl.IsNeed(n)` is refused `pl.CodeNeedUnknown` (the compiler should already have; the executor never routes on a name it does not know). `len(step.Needs) > 0` with `req.Compute != pl.ComputeClusterAndFleet` is refused `pl.CodeFleetNotConsented`. `len(step.Needs) > 0` -> `fleet.RunStep` (Task 12b). Else cluster.
3. Cluster: `ForwardWatched(workbench.PipelineStepAction, args=json(StepRun))` under `systemAuthority(ctx)` (a SYSTEM-class `ForwardedAuthority`, as `RunBuild` mints it). Concurrently every 30 s send `pipelineStatus{jobName}` to the same node. Outcome arrives -> `pipelineAck` (fire and forget) -> return. Status `finished` -> take that outcome, cancel the pending forward's wait (not the work), ack, return. `ErrWorkbenchPeerLost` or status `stale` -> re-forward `pipelineStep` with no pin (another replica adopts). Effective deadline + 2 min with nothing -> failed `pipeline_node_lost`.
4. Every outcome: `Timings = pipelines.ParseGoTestOutput(...)` is computed by the RUNNER before it replies (add to Task 6's finalize: parse the archive file when the step's command contains `go test`).
5. `Cancel(runID)`: cancel this node's in-flight `Execute` contexts for the run; `Forward(pipelineCancel{runID})` to any workbench (deletion is by label, cluster-wide); fleet in-flight dispatches cancel through their contexts (the dispatcher sends `ToolCancel`).

Task 12b, `fleet.go` (`//go:build agent`): needs -> `scope.go` labels + `pipelines=allowed` (+ `docker=true` for docker); mint the clone token on the agent; `Dispatcher.Dispatch(ctx, Request{Tool:"workerHost", Action:"pipeline_step", Purpose: PurposePipeline, OwnerUserId, RunId: workRunId, StepId: stepKey, Timeout, RequireLabels, OnStreamChunk: capture.Feed})` under internal origin + a bound forwarded authority for the owner (`ForwardedAuthorityForUser` -> `VerifyForwardedAuthority` -> `BindForwardedContext`, the `model_pull_runner.go` precedent); the same `Capture` (store + archive + tail + masking) fed from stream chunks; artifacts from `artifactsTgzBase64`; the Library through the same `LibraryStore` (the agent node wires the app store too); `Where{Surface:"fleet", WorkerID, NodeID, MachineLabels}` from the dispatch result. Codes: `no_worker_available` and every refused-before-start -> `pipeline_no_machine_for_need`; `kill_switch_engaged` -> `pipeline_fleet_disabled`; timeout -> the deadline code.

- [ ] **Step 1: Failing tests**: `TestExecuteRefusesAnUnknownNeedAndAnUnconsentedFleetStep`, `TestExecuteRefusesPastTheRunCeiling`, `TestExecuteNamesTheBindingDeadline`, `TestExecuteForwardsAClusterStepAndAcks`, `TestExecuteReattachesWhenTheWorkbenchIsLost` (Review Focus 2, hop test: workbench-a dies after the Job starts; workbench-b adopts; one Job, one Library log), `TestExecuteTakesAFinishedStatusWhenTheReplyWasLost`, `TestCancelReachesInFlightStepsAndDeletesJobs`, fleet: `TestFleetStepRoutesByNeedLabelsAndPipelinesLabel`, `TestFleetStepWithNoMachineIsTyped`, `TestFleetKillSwitchIsTyped`, `TestFleetRecordsTheMachine`; adapt: `TestStepRequestBecomesAStepRun` (env = `req.Environment()` minus secrets; secrets carried separately).
- [ ] **Step 2: Run** (`-tags agent` for the fleet tests) -- FAIL.
- [ ] **Step 3: Implement**; register in `app/pipelines_executor.go` on the agent node only.
- [ ] **Step 4: Run** `make test` and `go test -tags agent github.com/znasllc-io/memql/integrations/pipelinesteps/... -race -count=1` -- PASS.
- [ ] **Step 5: Commit** `Issue #5493/#5494: the agent's executor -- cluster steps forwarded to a workbench and re-attached on loss, fleet steps by need, cancel and the run ceiling`.

### Task 13: cancel-on-push in the trigger (#5496)

**Files:** the seam's trigger (`component/pipelinerun`, name the function when it lands), test beside it.

Rule: when the trigger opens a run with `mode == affected` for `(repository, pullRequest)`, every OTHER run of that pair with `mode == affected` and a non-terminal status gets `pipelinerun.RequestCancel(ctx, id, "superseded by <new run id>")`; a `full` run is never cancelled; a run of another pull request is untouched.

- [ ] **Step 1: Failing tests**: `TestANewPushCancelsTheRunningAffectedRunOfTheSamePullRequest`, `TestAFullRunIsNeverCancelledByAPush`, `TestAnotherPullRequestsRunIsUntouched` (db-gated if the trigger is; `MEMQL_REQUIRE_DB=1` against the throwaway Postgres on :15434).
- [ ] **Steps 2-4**: run, implement, run.
- [ ] **Step 5: Commit** `Issue #5496: a new push cancels the running affected-mode run of the same pull request, never a full-mode run`.

### Task 14: retention -- archive first, delete second (#5496)

**Files:** `dsl/pipelines/automations.memql` (`@trigger(schedule="0 50 3 * * *") automation pipelinesRunRetentionSweep { sweep := builtin pipelinesRetentionSweep(dryRun: false) }`), `dsl/pipelines/builtins.memql`, the Go handler beside the seam's plug-in (archive each terminal `v1:pipelines:run` older than `MEMQL_PIPELINES_RUN_RETENTION_DAYS` together with its `v1:work:run` and children through the work spine's `retireVerified` path -- read back and verify, then delete by exact key; no archive means no delete; the Library files stay, they are the owner's), `component/auth/maintenance_actor.go` (argued entry), the automation corpus golden, `scripts/secrets/manifest.yaml` (+ sync), env-vars.md.

- [ ] **Step 1: Failing tests**: `TestRetentionArchivesBeforeItDeletes`, `TestRetentionRefusesToDeleteWithoutAnArchive`, `TestRetentionKeepsRunsInsideTheWindowAndNonTerminalRuns`, the maintenance-actor gate, the corpus run.
- [ ] **Steps 2-4.**
- [ ] **Step 5: Commit** `Issue #5496: pipelines runs retained for MEMQL_PIPELINES_RUN_RETENTION_DAYS, archived first and deleted second`.

### Task 15: the engine's manifest references both images by digest (#5496)

**Files:** `memql-package.yaml` (repository root): `formatVersion: 1`, `name: memql`, `pipeline:` with `image: ghcr.io/znasllc-io/memql-toolchain@sha256:<built digest>`, `services.postgres.image: ghcr.io/znasllc-io/ci-timescaledb@sha256:d8336a5c9cc49bdbf1e935234e8c87ab64b6d6d195fd2ecedff7b00185557631` with its env and `ready`, `caches: [go, npm]`, and the D7 stages the compiler accepts. A test (`memql_package_manifest_test.go` at the root) compiles it with `pipelines.Compile` and asserts both images are `@sha256:` references.

The toolchain digest exists only after `build-toolchain-image.yml` runs on `main` (a dispatch workflow must be on the default branch before it can be run). Order: merge the epic PR, dispatch the build, then pin the digest (one-line follow-up through `merge-as-owner.sh`). Until the pin, the manifest names the image by tag and the test SKIPS with that sentence -- it never asserts an invented digest.

### Task 16: the cluster-e2e leg (#5497)

**Files:** `test/clustere2e/pipelines_substrate_test.go` (`//go:build clustere2e`), `test/clustere2e/testdata/pipelines/manifest.yaml`, `test/clustere2e/testdata/pipelines/check_run.json` (recorded), `.github/workflows/install-cluster-e2e.yml` (a `pipelines` leg: from-source images, then `go test -tags clustere2e -run TestPipelinesSubstrate ./test/clustere2e/` with `GOWORK=off`), `.github/workflows/pipelines-live.yml` (disarmed: dispatch-only, refuses with exit 4 unless armed, documented as not armed), `scripts/citags/tags_test.go` (the clustere2e exclusion reason updated).

One manifest end to end on k3d: a step with the Postgres sidecar (`psql -h localhost -c 'select 1'` from the ci-timescaledb image as the step image), a sharded step (`shards: 2`, each echoing `$MEMQL_PACKAGES`), a step needing `display` refused `pipeline_no_machine_for_need` (no machine carries the label), a fork pull request refused by the seam, and the check-run body the seam composes asserted against `check_run.json` (ids and timestamps normalised).

### Task 17: docs, the terminal header, and the finish

**Files:** `docs/public/operate/pipelines-substrate.md` (where a step runs, the Job contract, the fleet contract and the machine policy, logs, artifacts, lifecycle, retention, every env var, every code), `docs/public/operate/env-vars.md` (the six vars), `component/work/terminal.go` header (no longer "no clocks": pipelines steps and runs carry typed ceilings), `GLOSSARY.md` entry, delete this plan file in the final commit.

- [ ] Run: `make test`; `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<throwaway :15434> go test -count=1 <the db-gated trees touched>`; `make env-registry-check`; `make concept-snapshot-check`; `make frontdoor-paths-check`; `cd clients/os && npm run typecheck && npx vitest run`; `go test ./deploy/... -count=1` with kubectl on PATH.
- [ ] Commit `Epic #5478: the substrate plan is carried out; delete it`.

# Pipeline steps run on the owner's machines first, retry transient failures, and fail over across machines to the cluster

- **Date:** 2026-10-28
- **Status:** agreed with the owner in the 2026-10-28 brainstorm. The forks put
  to the owner and answered: carry the unmerged Codex fleet-first work forward
  rather than design beside it; retry infrastructure failures and commands that
  exit 75, never an ordinary failure; keep the attempts in the driver. The four
  design sections (the retry order, durable attempts, policy and dispatch,
  visibility and rollout) were presented and each approved. D10 below departs
  from the approved section 4 on one point, for a reason found while writing
  this record; the owner reviews it with this record. Corrected on 2026-10-29
  to match what was built: D5, D9 to D11 and sections 3 to 7, 9, 10, 12 and 13
  carry the decisions made during implementation, each with its reason. The
  maintained account is the "Pipeline step attempts, retries and failover"
  section of `docs/internal/ops/harness-decisions.md`.
- **What it is:** a pipeline step that can run on either side runs on the
  owner's fleet first and on the cloud cluster only for what the fleet cannot
  take. A step that fails for a transient reason is retried a bounded number of
  times on the same machine, then on another eligible machine, then on the
  cluster where the step can run there. Nothing whose outcome is unknown runs a
  second time unless the step says repeating it is harmless.
- **Why now:** the 0.31.7 release's first attempt failed on one stalled browser
  download on one Mac, and recovering cost a full run of about three hours.
  Every Linux step runs on the AKS build pool, which is what the owner pays
  Azure credit for, while two idle Linux workstations sit in the fleet. And the
  cost guard holds whole runs, fleet-only runs included.
- **Builds on:** the Codex fleet-first work in
  [memql#6294](https://github.com/znasllc-io/memql/pull/6294) and its rebased
  copy on `codex/local-clean-start-2026-10-28` (`placement: pool`, the captured
  compute placement policy, admission version 2); the pipelines program
  ([2026-09-16-pipelines-program-design.md](2026-09-16-pipelines-program-design.md));
  [2026-10-11-pipelines-local-green-and-fast-design.md](2026-10-11-pipelines-local-green-and-fast-design.md)
  (D9: the package targets linux/amd64). It must land beside the release-train
  switch (memql#6339) and failed-only release re-runs (memql#6369), which change
  `component/pipelinerun/driver.go` too.
- **Repositories:** `memql` (this change). `memql-znas` and the machines change
  in the rollout (section 12), not here.

## 1. Where it stands (read 2026-10-28)

- **Placement is fixed per step by the manifest.** `memql-package.yaml` pins 61
  steps `placement: cluster` and 15 `placement: fleet`; native execution or any
  `needs:` implies fleet (`component/pipelines/execution.go` `PlacementOf`).
  The executor branches once on `Step.RequiresFleet()`
  (`integrations/pipelinesteps/executor.go`). The cluster never takes a fleet
  step and the fleet never takes a cluster step.
- **Fleet routing** (`integrations/agent/worker/router.go` `Plan`): the owner's
  machines in registration order, kept when the stream is held, the capability
  and labels match (`pipelines=allowed`, `os=darwin` or `docker=true` per need),
  the repository scope accepts the repository, the action contract matches
  exactly (`ActionContracts.Supports` is `==`) and the native platform matches;
  ordered by the owner's routing policy, `firstFit` by default.
- **Busy:** each machine admits one pipeline command per OS user and refuses a
  second before starting it (`pipeline_capacity_busy`); the dispatcher moves to
  the next candidate. All busy: `Fleet.dispatchWhenCapacityAvailable` waits and
  routes again until the run's ceiling. It never spills to the cluster.
- **"Host commands" off is misread.** `ownerFeatures.headless` is not on
  `fleetcatalog.Candidate`, so the router keeps the machine. The holding replica
  refuses in `streamSession.dispatch` (`component/worker/server.go`,
  `admitOwnerFeature`) before anything is sent, but returns a Go error, which
  `translateResult` labels `worker_disconnected`; `pipelineRefusedBeforeStart`
  ignores a result that carries an error. The step fails `pipeline_node_lost`
  and is never routed elsewhere, on a local or a forwarded dispatch alike.
- **No step is retried.** The driver runs each step once
  (`component/pipelinerun/driver.go` `runStep`). `workjournal.Step` always
  writes `attempt: 1` and `idempotencyKey: run:key:1`, although
  `dsl/work/concepts.memql` documents `attempt` as incremented per retry, the
  driver's `resume` reads it back, cluster Job names include it and Library step
  files are stored per attempt. The stage workflow cannot retry: `executeStep`
  answers a finished step's status. Below the step, the cluster's clone retries
  its fetch three times (`integrations/pipelinesteps/wrapper.go`); a machine's
  `git fetch` runs once (`cockpit/internal/worker/tools/pipeline_step.go`).
- **After a step starts it never moves.** A dropped machine connection fails
  `pipeline_node_lost`; a driver takeover marks an in-flight fleet step
  `pipeline_execution_uncertain`; an in-flight cluster Job is adopted by name.
- **macOS cannot run on the cluster.** Containers must name a Linux platform,
  and native execution or host needs require fleet placement
  (`validateStepRuntime`). Neither AKS nor `memql-znas` provisions macOS
  capacity, so a macOS step's failover ends at the owner's Macs.
- **The cost guard** (`memql-znas` `deploy/k8s/instance/azure-cost-guard/`)
  writes `admission.json`, read on every claim through
  `MEMQL_PIPELINES_ADMISSION_FILE`. Version 1 holds whole runs. Version 2
  (`allowCloud`) is prepared in `memql-znas` but main's engine decodes the file
  with `DisallowUnknownFields`, so writing version 2 today would hold every run.
- **The fleet** (Fleet app, 2026-10-28, all online):

  | Machine | Platform | CPU, memory | Free disk | Cockpit | Pipeline steps |
  |---|---|---|---|---|---|
  | CyberPC | linux/amd64 | i9-13900KF, 32 cores, 31 GiB | 571 GiB | 0.18.3 (contract 3) | not allowed |
  | MSI Raider | linux/amd64 | Core Ultra 9 285HX, 24 cores, 62 GiB | 1.1 TiB | 0.20.1 (contract 4) | allowed |
  | Black MacBook Pro | darwin/arm64 | M4 Max, 16 cores, 64 GiB | 105 GiB | 0.20.1 (contract 4) | not allowed (owner, 2026-10-28) |
  | Gray MacBook Pro | darwin/arm64 | M4 Max, 14 cores, 36 GiB | 301 GiB | 0.20.1 from main (contract 4) | allowed |

  All four have Docker. The two Linux machines can run the package's
  linux/amd64 container steps as they are; fleet containers refuse
  architecture emulation, so the Macs take the macOS steps.

## 2. Decisions

### D1 -- Carry the Codex fleet-first work forward

The branch starts from `origin/main` and takes the fleet-first commits from
`codex/local-clean-start-2026-10-28`: `cbf2f39b4`, `eb186e726`, `1d592988b`,
`138658c8c`, `8ae6ec3f2` and `19fc82812`. Applied to current main they conflict
in six files (run admission, which main gave the train allowance; the env and
secrets manifests; two docs), which `d962310d1` resolved once already. Left
out: the 0.31.4 version commit `599128679` and the branch's unrelated work
(declared test inputs, Nexus, package recovery, Apps placement). memql#6294 is
closed when this change merges.

What it brings: `placement: pool` for a portable container step; the
installation's captured `pipelineComputePlacementPolicy`
(`fleetWaitSeconds`, `cloudFallback`), bound into each step's recovery
definition; admission version 2, where a cost hold closes only cloud Jobs and
the workbench rechecks it before each new Job or isolation probe, with the
train allowance preserved; and worker contract 5, with container CPU and memory
caps and host headroom checks.

### D2 -- Attempts live in the driver and are journaled

The driver runs a step's attempts. Each attempt is recorded before it starts
and its outcome before the next one starts (section 3). Rejected: attempts in
the DSL workflow (every installation's workflow would change, recovery would
have to rebuild loops over finished steps, and the safety checks would still
live in Go, so the rules would live twice); attempts inside the executor (the
journal would never see them, so a takeover loses its place and the check run
cannot show them).

### D3 -- An attempt's outcome falls in one of four classes

The executor, which knows whether a dispatch reached a machine and what the
machine or Job answered, classifies every failed attempt (section 4):
**nothing ran**, **retry requested** (the command exited 75), **outcome
unknown**, or **final**. The driver reads a result with no class as final, so
an older or unexpected answer is never retried.

### D4 -- The retry order

Retry the same place, then other machines, then the cluster (section 5). A
`fleet` step ends at the owner's machines; a `cluster` step retries on the
cluster only; a `pool` step may reach the cluster when the policy allows cloud.

### D5 -- An unknown outcome is retried only for a repeatable step

A command that may have run is run again only when the step declares that
repeating it is harmless: `repeatable: true`, which `verification: true`
implies (a verification command is deterministic, input-only and produces no
artifacts). Otherwise the step fails as it does today.

Repeatable means harmless to run again **even while an earlier copy may still
be running elsewhere** (made precise during implementation). A failover after
an unknown outcome on a machine does not wait for that machine's copy to stop:
the outcome is usually unknown because the machine is unreachable, so a
confirmed stop is often unprovable, and waiting for one would block exactly the
failovers that matter. The earlier copy's late answer and artifacts are never
filed.

### D6 -- The installation's captured policy sets the limits, under an engine cap

The policy result gains `retries` (section 6). It is captured per run with the
rest of the compute placement policy, so every replica applies the same limits
and a changed policy cannot alter a run already started. An engine constant of
**8 attempts per step** bounds any policy. The portable default retries on the
fleet and never spends cloud.

### D7 -- The manifest and the command have one say each

The manifest declares `repeatable`. A command says "transient, retry me" by
exiting 75 (`EX_TEMPFAIL`). Neither can widen the installation's limits or
spend cloud the policy did not allow.

### D8 -- The dispatcher can pin and exclude machines, for pipeline steps only

`worker.Request` gains a pinned registration (the same-machine retry) and
excluded registrations (machines already tried). Only the engine's pipeline
purpose may set them; any other purpose that carries one is refused. Design D4
of the fleet router holds: a model still cannot name a machine.

### D9 -- "Host commands" off is a refusal before start

For the pipeline purpose, the router reads `ownerFeatures.headless` and skips
a machine with Host commands off (an agent's call is refused before send and
follows its owner's fallback). A machine switched off after it was picked refuses with a typed
error, which both the local and the forwarded dispatch report as refused before
start, under `worker_feature_disabled`, so the dispatch moves to the next
candidate. This applies to every purpose, because the refusal always precedes
the send: an agent's call moves on too, under its owner's `fallback`.

### D10 -- Contract 5 switches in lockstep, behind an operator hold

Section 4 as approved said native steps would keep contract 4 so the engine
could deploy before the machines upgrade. Two facts found afterwards rule that
out: a machine's contract must match the engine's requirement exactly
(`ActionContracts.Supports` is `==`), so accepting both 4 and 5 would mean a
second requirement and a second argument encoding per step kind; and the
repository's pre-release rule forbids compatibility shims on Go wire contracts
(CLAUDE.md, Branch Workflow 3). So contract 5 switches in lockstep, as the
Codex work intended. The rollout holds new runs with the cost guard's operator
hold, lets in-flight runs finish, deploys the engine and upgrades every
machine's Cockpit together, releases the hold and watches the first run
(section 12). Rejected: the version-acceptance shim above.

### D11 -- Two PRs; the manifest and the installation follow the deploy

Production's engine compiles each pull request's `memql-package.yaml`, and the
manifest decoder refuses unknown keys (`KnownFields(true)`). A manifest using
`placement: pool` or `repeatable:` would fail every run until production runs
this engine. So this PR changes no manifest key; a follow-up PR moves the
portable checks that are not verification steps to `pool` and marks them
repeatable after the deploy (section 12).

### D12 -- Out of scope

Several steps at once on one machine (the largest remaining lever on spend; a
Cockpit change that touches the Codex host build lock, so its own follow-up
spec); model routing (another session); agent workbenches, which already prefer
a Fleet workbench; the Cockpit release itself (memql#6372's session); retries
inside the machine's fetch, which the same-machine retry covers; cloud macOS
capacity.

## 3. The attempt lifecycle

Every attempt of a step is a version of the step's one work-step row, written
through `workjournal`:

1. **Intent of attempt N**, before the executor is called: `status: running`,
   `attempt: N`, `idempotencyKey: <runId>:<key>:N`, and `binding.attempts`
   holding the records of attempts 1 to N-1.
2. **Outcome of attempt N with another attempt to come:** the status stays
   `running`; `binding.attempts` gains attempt N's record;
   `binding.nextAttemptAt` is when attempt N+1 may start. Only then does the
   driver acknowledge attempt N's resources and, for a cluster attempt, wait
   until its Job, Secret and pods are confirmed gone, so no waiting retry holds
   a cluster slot and two attempts never run at once.
3. **Final receipt:** `done`, `failed` or `cancelled`, with the final attempt's
   binding, log and artifacts, and the whole `binding.attempts` history.

Corrected during implementation: the time of the next attempt is
`binding.nextAttemptAt`, not the step's `resumeAt`. The binding is rewritten
whole on every update that names it, so the final receipt cannot keep a stale
timer, and the pipeline's row stays self-contained; `resumeAt` is a work-step
field the work spine's own sweeps read for parked work.

An attempt record is `{attempt, class, code, message, surface, workerId,
machineName, nodeId, jobName, startedAt, finishedAt, logFileId,
notRetriedHere}`, every string masked as a receipt's strings are. `machineName`
is the display name the machine's owner chose, which the check run may print
(section 9); `notRetriedHere` marks a machine that refused for a reason a delay
does not change (section 4). The history lives in `binding` (an open `object`),
not in a new concept field, so an engine rollback cannot leave rows carrying a
field the older concept does not declare.

Between attempts the row never reads as finished, so the stage workflow and the
check run, which treat a failed step as final, see a running step. The step's
in-memory state stays `running`, and progress reports say it is retrying.

**A replica that takes the run over** (`resume`, `adoptRows`) reads each step's
row:

| The row says | The new driver |
|---|---|
| attempt N started, no record for N | sends attempt N again as recover-only: on the cluster it adopts Job N by name; on a machine the outcome is unknown, so a repeatable step continues at N+1 and any other step fails `pipeline_execution_uncertain`, as today |
| attempt N recorded, N+1 not started | confirms attempt N's resources gone again, then continues at N+1 once `binding.nextAttemptAt` has passed |
| finished | keeps the receipt |

A cancel during a wait between attempts settles the step cancelled. A lost
lease during a wait stops the drive; the replica taking over continues from the
row.

## 4. Classifying an attempt

| Where | What came back | Class |
|---|---|---|
| machine | the machine's fetch failed (`pipeline_clone_failed`), or the clone token could not be minted | nothing ran |
| machine | `pipeline_cache_unavailable`, `pipeline_runtime_unavailable`, a capacity refusal that reached the result, `denied_by_policy`, `worker_feature_unavailable`, or the dispatcher's verdict that no machine started the step (`pipeline_no_machine_for_need`) | nothing ran |
| machine | `worker_feature_disabled`, `pipelines_not_allowed`, or a placed attempt's wait for a busy or offline machine that ran out | nothing ran, and that machine is not retried |
| machine | `pipeline_recovery_required` | outcome unknown, and that machine is not retried |
| machine | the connection ended after the dispatch left, a lost reply, or a takeover of an in-flight attempt | outcome unknown |
| machine | the command exited 0 and its artifacts did not arrive intact (`pipeline_artifact_unavailable`) | outcome unknown |
| cluster | the agent's own `pipeline_runner_unavailable`, made before any runner took the step, and never on a recovery | nothing ran |
| cluster | `pipeline_node_lost`, `pipeline_image_pull_failed`, `pipeline_service_failed`, `pipeline_job_unschedulable`, `pipeline_clone_failed`, or a runner's `pipeline_runner_unavailable` | outcome unknown |
| either | `pipeline_execution_uncertain`: a recovery that found no Job or receipt | outcome unknown |
| either | the command exited 75 | retry requested |
| either | any other exit, `pipeline_step_timeout`, `pipeline_run_ceiling`, a cancel, a refusal (`pipeline_fleet_disabled` included), `pipeline_step_disk_exceeded`, `pipeline_job_rejected`, `pipeline_isolation_unenforced`, `pipeline_artifact_too_large`, `pipeline_executor_error`, a secret failure | final |

A timeout is final: a hang and a stall look alike from outside, and a script
that knows it stalled exits 75 instead.

Corrected during implementation, each for one reason: a class may say "nothing
ran" only where something proves the command did not start. A wrong "nothing
ran" runs a non-repeatable step twice (D5); a missed one costs only a retry that
a repeatable step still gets.

- `pipeline_recovery_required` was "nothing ran". The Cockpit also returns it
  after a command (it could not record the cleanup of one that finished), so
  "nothing ran" could start a finished command again. It is outcome unknown, and
  the machine is still not retried. Follow-up: split the Cockpit code into one
  before the command and one after.
- The cluster's codes were "nothing ran". The runner reports a verdict while
  the Job and its pod are still alive; only the agent's acknowledgement or a
  cancel deletes them, and until then the kubelet retries a pull or restarts a
  sidecar and the scheduler keeps placing the pod, so the command can still
  start. A pull, service or scheduling failure is therefore always outcome
  unknown. So is `pipeline_clone_failed`: it is also the runner's failure to mint
  the clone token, which can race another Run's creation of the same Job, and
  the result does not say which spoke (the Job's clone already retries its fetch
  three times). A runner's `pipeline_runner_unavailable` is unknown because the
  Job exists or may exist; only the agent's own, made before any runner took the
  step, proves nothing ran, and never on a recovery. Follow-up: a runner that
  stops the pod when it decides a failure before start would let these codes
  read "nothing ran" again.
- `worker_feature_unavailable`, a Host-commands setting the holding replica
  could not read, was missing: nothing ran, and the same machine may be tried
  again after its delay.

## 5. Choosing the next attempt

A **place** is one machine, or the cluster as a whole. The driver names each
attempt's place to the executor: the step's own placement (first attempt), one
pinned machine, any eligible machine except those tried, or the cluster.

After **nothing ran** or **retry requested** at place P (exit 75 needs no
`repeatable`: the command itself says another run is safe and useful):

1. P again, when P has had fewer than `sameMachine` retries and is not marked
   "not retried" (section 4), after `delaysSeconds[i]` (the last value repeats);
2. otherwise, for a `fleet` or `pool` step when `otherMachines` is true, an
   eligible machine not yet tried, at once;
3. otherwise, for a `pool` step when `cloudFallback` is true, the cluster, if
   it has not been a place yet;
4. otherwise the step fails with its last attempt's failure.

After **outcome unknown** at place P, for a repeatable step:

1. an eligible machine not yet tried;
2. P again: once when P is a machine, since a machine that still holds the old
   attempt refuses until it has cleaned up (its pipeline reservation forbids a
   second command), which counts as nothing ran there; up to `sameMachine`
   times, with rule 1's delays, when P is the cluster and the step is
   `placement: cluster`, each after the previous attempt's Job, Secret and pods
   are confirmed gone. Either needs `sameMachine` of at least 1;
3. the cluster, for a `pool` step when `cloudFallback` is true, if it has not
   been a place yet;
4. otherwise the step fails.

Corrected during implementation: the cluster's "P again" after an unknown
outcome. Section 4's corrections made nearly every cluster failure an unknown
outcome, so without it a repeatable `cluster` step would have had no retries at
all. Confirmed deletion before each next attempt keeps two copies from running
at once; a repeatable step is harmless to run again by its declaration. A
machine stays "once". A `pool` step reaches the cluster at most once, for every
class: the owner approved "the cluster once, only for pool steps" as a cost
bound, so a pool step's attempt there, whatever ended it, is never followed by
another on the cluster.

A `pool` step's first attempt keeps the Codex behaviour inside one attempt:
`fleetWaitSeconds` for fleet capacity, then the cluster when `cloudFallback`
allows. A later attempt naming the fleet waits for busy machines at most
`max(1, fleetWaitSeconds)` for a `pool` step (then counts as nothing ran, so
rule 3 applies) and as a first attempt does for a `fleet` step. The floor of
one second is a correction: a wait of 0 means no bound beyond the run's ceiling.
A pinned dispatch to a machine that is offline or still busy waits for it at
most `max(delaysSeconds)`, and at least a second, then counts as nothing ran
there and is not retried there. A `cluster` step's retries stay on the cluster
under the same `sameMachine` count. Every attempt counts against the run's
ceiling (720 minutes in production), and no step makes more than 8.

Also decided during implementation: an **unpinned** fleet attempt that comes
back nothing ran and not retried there, or naming no machine, means the fleet is
exhausted for now, for the rest of the step's attempts. The next stop is rule 3,
and the machine the wait last named is never pinned: the wait names only the
last busy machine. A **pinned** attempt whose answer names no machine (an
offline pin that found no candidate) is charged to the machine it was pinned
to.

## 6. Policy and manifest surface

**The installation's policy.** `pipelineComputePlacementPolicy` returns:

```memql
return {
  fleetWaitSeconds: 60,
  cloudFallback: true,
  retries: {sameMachine: 2, delaysSeconds: [30, 120], otherMachines: true}
}
```

`retries` is optional. Parsing stays strict: `sameMachine` is an integer 0 to
3, `delaysSeconds` one to three integers each 0 to 900, `otherMachines` a
boolean, and no other field. Without `retries` the portable values apply. Once
`retries` is present all three limits are required, and a null is refused, so
the policy source always says how hard the installation retries (decided during
implementation; a partial block would silently take the rest from the engine).
The portable default in `dsl/pipelines/automations.memql` becomes
`{fleetWaitSeconds: 0, cloudFallback: false, retries: {sameMachine: 2,
delaysSeconds: [30, 120], otherMachines: true}}`: retries on the fleet, no
cloud. `cloudFallback` covers both a full fleet and a failed one; one switch
means "pool steps may spend cloud".

Also decided during implementation: because `retries` limits every command
step, the driver reads the policy for every run with a command step, and a
policy phase that errors refuses every such run (`pipeline_stage_invalid`,
scoped `computePlacement`, naming the field), not only a run with a pool step.
The run would otherwise execute without the operator's declared limits. A
snapshot with no placement phase, such as production's stored resolver today,
takes the portable defaults.

**The manifest.** `repeatable: true|false` at the pipeline level (the default
for its steps) and on a step (overriding it); absent means false. It means
harmless to run again even while an earlier copy may still be running
elsewhere (D5). `verification: true` implies it, and `verification: true` with
`repeatable: false` is refused as a contradiction. Driver-owned steps (notify,
repository change, SARIF) are not retried; they never reach the executor, and a
SARIF or repository-change step that names `repeatable` is refused rather than
ignored.

**The command.** Exit 75 means "transient, retry me" for any command step. The
editor host runners exit with `exitCodeFor` from `scripts/vscode/download.cjs`,
which answers 75 when the VS Code download stalls, keeps failing in transport
after its three attempts, or runs past its 20-minute total time limit, and 1 for
anything else; `scripts/ci/editor-host.sh` passes the status through. An engine
without this change sees an ordinary failure, so the script change is safe to
ship in this PR. The time limit asks for a retry only when it ends before the
step's own 25-minute timeout, and the setup before the download counts against
that timeout; with more than about five minutes of setup the step times out
first, which is final.

## 7. Dispatcher and router

- `worker.Request` gains `PinRegistration` and `ExcludeRegistrations`. The
  router narrows the plan to the pinned machine or drops the excluded ones, and
  records why in `Rejected`. A pinned machine held by a sibling replica is
  reached over the existing forward, whose receiver re-checks consent, the
  contract and Host commands.
- `purposeRefusal` refuses either field on any purpose but the pipeline's
  (`denied_pipeline_purpose`), after the purpose's own binding. (Corrected: the
  check is `purposeRefusal`, through `machineNarrowingBinding`, not
  `purposeBinding`.)
- `fleetcatalog.Candidate` gains `HostCommandsOff`, true when the owner's
  `ownerFeatures.headless` is explicitly false and for malformed settings,
  which is how admission reads them; a missing setting keeps today's behaviour.
  The router rejects a machine with it on for the pipeline purpose ("Host
  commands are off in Fleet settings").
- `streamSession.dispatch` returns a typed error for a disabled feature;
  `attemptLocal` and `ForwardHandler` report it as refused before start under
  `worker_feature_disabled`.
- `pipelinesteps.Fleet` sends the placement directive (pin, exclusions and a
  bounded wait) and returns the attempt's class with its result. (Corrected: no
  attempt number crosses to the machine; the attempt stays the driver's.)

## 8. Cost guard and admission

A cloud attempt is a new cluster Job, so it passes the Codex admission check the
workbench makes before every new Job: while the cost guard holds cloud
spending, a cloud failover waits within the run's ceiling instead of spending.
The train allowance carries over as `19fc82812` shaped it. Fleet attempts never
consult the cloud gate. Admission version 2 is written by `memql-znas` only
after production runs this engine (section 12).

## 9. Visibility

- **Check run:** a retried step says how it got there, for example "Passed on
  attempt 2 of 2. Attempt 1: `pipeline_clone_failed` on MSI Raider." A failed
  step lists every attempt with where it ran and why it ended, and a step
  waiting for its next attempt reads *Retrying*. Decided during implementation:
  the check run is public on GitHub for a public repository, so a machine is
  named only by the display name its owner chose in Fleet, else the first eight
  characters of its registration id, never the name the machine reported, which
  is usually its hostname. `pipelines.MachineWords` is the one place the rule is
  written, and the dispatcher's refusal sentences, which a failed step quotes,
  call it too.
- **Compute and Settings → Pipelines:** when Host commands off is the only
  reason a machine cannot take pipeline steps, the copy names it and points at
  the machine's Fleet switch instead of blaming `policy.yaml` (decided during
  implementation: a remedy that names the wrong cause sends the owner to the
  wrong file).
- **OS run page** (`clients/os/src/apps/deployables/pipelines/RunPage.tsx`): the
  step shows its attempt count, and its detail lists each attempt with its
  machine (through the page's existing machine-name lookup) and its own log.
  The `memql-ui-design` skill governs the change.
- **Fleet records:** each machine attempt is its own `v1:worker:invocation`,
  carrying `reroutedFrom` when it moved.
- **Logs:** each attempt's log is its own Library file; the Library store
  already keys step files by attempt.

## 10. Testing

Cross-replica cases come first (CLAUDE.md: multi-node is the default).

- **Two agent replicas, in process** (`integrations/agent/worker`): a pinned
  retry reaches the sibling holding the machine; excluded machines are skipped;
  a machine with Host commands off is skipped by the router, and one switched
  off after planning refuses before start on the holding replica, local and
  forwarded, and the dispatch moves on; pin and exclude are refused outside the
  pipeline purpose; a started attempt is never routed again.
- **Takeover, database-backed** (`component/pipelinerun`, `MEMQL_REQUIRE_DB=1`):
  a second driver resumes at N+1 after `binding.nextAttemptAt`; an in-flight machine attempt
  continues on another machine for a repeatable step and fails
  `pipeline_execution_uncertain` otherwise; an in-flight cluster attempt adopts
  Job N; between attempts the row never reads finished.
- **The retry order** (driver with a fake executor): same machine with its
  delays, then other machines, then the cluster for `pool`; `fleet` steps stop
  at the machines and macOS steps at the Macs; `cluster` steps retry on the
  cluster only; the 8-attempt cap and the run ceiling hold; a cancel during a
  wait settles cancelled; a held cloud gate makes a cloud failover wait; a
  pinned offline machine's wait is bounded.
- **End to end across replicas** (`test/pipelinehop`): a run driven on the
  agent replica whose fleet step fails "nothing ran" once and passes on attempt
  2, through the real journal and check run.
- **Unit tables:** each code and exit status to its class, on both surfaces;
  strict parsing of `retries` and its place in the recovery definition; the
  `repeatable` key, its pipeline default and the verification contradiction;
  the download script's exit 75.
- **Scheduled scans** (added during implementation, at the owner's request): an
  executor-level case that a pool step of a run with event `schedule` lands on
  an eligible fleet machine. Scheduled scans take the same route as every other
  run; nothing in placement or the retry order reads the event, apart from the
  installed policy, which receives it.
- **Verification:** `make test`, the db-gated trees from
  `scripts/ci/db-gated-packages.sh --trees` with `MEMQL_REQUIRE_DB=1`, and the
  carried Codex tests.

## 11. Documentation

Updated in this change: `docs/public/operate/pipelines.md` (`repeatable`, exit
75, retries and failover, the policy's `retries`, the outcome classes);
`docs/public/operate/pipelines-substrate.md` (where a step runs, the fleet,
losing a replica or machine, when each side gives up, codes);
`docs/public/operate/workers-runbook.md` (pipeline pin and exclude, Host
commands off); `cockpit/docs/pipelines.md` (when the engine sends a step again,
and the lockstep); `docs/internal/ops/harness-decisions.md` (a section on step
attempts: trigger, evidence, chosen path, alternatives, stop conditions, what
survives a takeover, regression tests, unverified behaviour); and the code
comments at each decision.

## 12. Rollout

1. **This PR**, through a release train the owner approves: the carried
   fleet-first work, attempts, retries and failover, the Host-commands fix, the
   exit-75 script change and the docs. No manifest key changes. With no other
   change, the deploy turns retries on through the portable default policy:
   production's stored resolver has no placement phase, so it takes the
   portable limits, as does an installed policy that names no `retries`.
   Placement does not change until a manifest marks a step `pool`.
2. **A Cockpit release carrying contract 5**, cut through the release pipeline
   memql#6372 establishes. Not installed yet: a contract-5 Cockpit is
   ineligible on the current engine. It is cut while production still runs the
   old engine, because its macOS steps need today's machines.
3. **A maintenance window:** hold new runs with the cost guard's
   `operatorHold`; let in-flight runs finish; deploy the engine (memql-znas pin,
   manual ArgoCD sync); upgrade Cockpit on all four machines (CyberPC also
   needs `pipelines.allow: true`; Black MacBook Pro when the owner turns
   pipelines back on); release the hold and watch the first run, holding again
   if it fails. (Corrected during implementation: a hold cannot let exactly one
   run through, because an allowance admits every queued run.) After the deploy,
   dry-run the installed policy: a policy phase that errors now refuses every
   run with a command step (section 6).
4. **memql follow-up PR:** `memql-package.yaml` moves the portable checks to
   `pool` (from `codex/fleet-first-manifest`) and marks them repeatable;
   release-publishing steps stay non-repeatable. It cannot move a
   `verification: true` step: reusable verification requires `placement:
   cluster` (`component/pipelines/verification.go`). Twenty steps carry it, the
   Go and database tests and the node builds and tests among them, so moving
   those waits on an owner decision: give up evidence reuse for them, or extend
   reusable verification to evidence a machine produced.
5. **memql-znas follow-up PR:** the stored resolver moves to the fleet-first
   profile with `retries`, and the cost guard to admission version 2.

## 13. Risks and accepted limits

- Retries lengthen a failing run, within 8 attempts and the run's ceiling.
- Exit 75 relies on scripts using it honestly; a misuse is retried, boundedly.
- A step wrongly marked repeatable can run twice. Release-publishing steps stay
  non-repeatable, and review owns the marking.
- An unknown outcome on the only eligible machine cannot fail over; a macOS
  step with one Mac allowing pipelines fails as today.
- A step waiting between attempts keeps its stage slot (16 in the core
  workflow, 4 in `znasDeliveryWorkflow`).
- The contract switch needs a short maintenance window (D10).
- Each machine still runs one step at a time (D12).
- Source tests prove the contract; live fleet-to-cloud behaviour is verified at
  rollout, not by this PR. Neither live fleet-to-cloud failover nor a
  production maintenance window has been exercised.
- Found during implementation: a repeatable cluster step's earlier attempt can
  start after its runner reported and before its Job was deleted, so it may have
  run in part before the next attempt; confirmed deletion keeps the two from
  overlapping (section 4's follow-up removes this). A non-repeatable step that
  meets a machine needing recovery fails as today (section 4's Cockpit
  follow-up). The editor host's download time limit asks for a retry only when
  setup leaves it room inside the step's timeout (section 6).

## 14. Follow-ups

- Several steps at once per machine, sized to its cores and memory (own spec).
- The two follow-up PRs in section 12.
- Retire `codex/fleet-first-ci`, `codex/fleet-first-manifest` and the
  fleet-first commits on `codex/local-clean-start-2026-10-28` once this lands.

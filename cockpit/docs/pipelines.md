# Pipeline steps on this machine

Let the cluster run eligible CI steps here, either as dedicated Fleet work or
through an installed policy that prefers this machine's available capacity.

The arrangement is made in two places: the repository's pipeline says a step
may run on the fleet, and this machine's `policy.yaml` says this machine will
take it. Engine half: epic memql#5478 (`workerHost.pipeline_step`, issue
memql#5494). This half: the `pipeline_step` action, the `pipelines` policy and
the `pipelines=allowed` registration label and the action's reported repository
scope. A machine that does not report a scope is not eligible for a pipeline
step; upgrade and reconnect it first. The runtime also reports
`actionContracts["workerHost.pipeline_step"] = 5`. The engine requires this
contract before sending a native/container request with the current resource rules; operator labels
are not evidence of an implemented action contract.

---

## The rules, before the setup

**Nothing runs here until this machine's owner says so.** `pipelines.allow` is
off on every machine, including every machine upgrading into this feature. A
laptop is never a default place for somebody's CI.

**Two consents, and this is one of them.** The cluster only routes a step to a
machine that registers the label `pipelines=allowed`, and this worker registers
it exactly when `pipelines.allow` is true -- a `pipelines` label written by hand
into `worker.yaml` is removed, because a machine that would refuse every step
routed on it must not carry it. The other consent is the pipeline's own, on the
cluster.

**It is a standing consent, not a consent window.** A step arrives when a push
lands, not while somebody is at the machine, so `memql worker consent grant`
has nothing to do with it, and `memql worker consent revoke` does not stop it.
Withdraw by removing `allow` (see below).

**What `allow` grants, plainly.** It lets the cluster's pipeline runner run
commands it chooses, as you, for the repositories it names. The command is
whatever the cluster sends -- this machine cannot see the pipeline it came
from -- and it runs as the user the worker runs as, with that user's
environment and files for an explicitly native step. A container step uses
its declared image, checkout, declared caches and supplied environment. The
explicit image-verification contract below instead uses a separately installed
fixed rootless builder; it accepts no repository command, service or credential. Native execution
is not a sandbox. `pipelines.repos` narrows which
repositories; the clone URL must name the same repository, so the list filters
what is actually cloned rather than a name the request states.

**Only the pipeline runner.** The cluster's pipeline runner dispatches a step
with no agent; an agent's tool calls always name theirs. A step an agent
dispatched is refused whatever the policy says.

---

## Setting it up

In `~/.memql/policy.yaml`:

```yaml
pipelines:
  allow: true
  repos:
    - acme/widgets
    - acme/tools
  workspace_root: ~/ci
  max_timeout_sec: 3600
```

| Key | Default | Meaning |
|---|---|---|
| `allow` | `false` | Whether this machine runs pipeline steps at all. |
| `repos` | empty: any repository | When it lists any, only these repositories' steps run here. `owner/name`, compared without regard to case or a `.git` suffix -- and the clone URL must name the same repository. |
| `workspace_root` | `fs.workspace_root/pipelines`, else `~/.memql/pipelines` | Where each step's fresh checkout is made, and removed again. |
| `max_timeout_sec` | `3600` | The longest a step may run, whatever it asks for. A step that names no timeout gets the cluster's own default, 1200 seconds, under this cap. |
| `container_memory_mib` | `2048` | Maximum command-container memory in MiB, from 256 through 32768. A request may use less. Docker memory-plus-swap equals that limit. Each service has a separate 2 GiB limit. Native commands have no hard RAM quota. |
| `container_cpu_milli` | `2000` | Maximum command-container CPU in milliCPU, from 1 through 256000. Each service has a separate two-CPU limit. Native commands have no hard CPU quota. |
| `min_free_disk_mib` | `20480` | Available space required on the workspace filesystem before checkout and again before the pipeline command. Set 1024 through 1048576 MiB for this machine; omitted or zero uses the default. The cluster cannot lower it. |

For a test or build that needs more than 2 GiB, set `container_memory_mib: 4096`
on a machine with enough free Docker memory. This uses existing machine
capacity. It creates no cloud resources and the cluster cannot override it.
Allow headroom for service containers, the local cluster and other work.

`SIGHUP` reloads it (`kill -HUP $(pgrep -f 'memql worker run')`). The block
**replaces** on reload rather than merging: removing `allow`, or narrowing
`repos`, takes effect for the next step that arrives. The worker then
re-registers for either an allow-flag or repository-list change so the
cluster's router sees the new answer -- at the first moment
nothing is running, because work in flight is never cut short. A step routed
here on the old answer in the meantime is refused by the new one.

---

## What a step does

1. **Admission.** A step an agent dispatched is refused first. Then `allow`,
   then `repos`; refused -> `denied_by_policy`, with a sentence naming the
   setting to change. Then the request itself: an https clone URL whose path
   names the step's `repository` (`https://github.com/o/r.git` names `o/r`),
   and so on (`bad_request`). Nothing is created before all of it passes.
2. **Runtime and capacity.** `execution` is explicitly `native` or `container`.
   `platform` names `darwin/arm64`, `darwin/amd64`, `linux/arm64` or
   `linux/amd64`. Native work must match the binary-reported host OS/architecture. A native
   `needs: [docker]` request also probes the daemon before checkout. Host needs
   never pass through a container boundary. Container work requires a
   Linux platform and an `image` pinned by `@sha256:...`; a live Docker probe
   must match it without emulation. The downloaded image's actual platform is
   checked before starting it too. No host-shell fallback occurs. Contract 3
   supports container services and caches; native steps cannot declare them.

   Up to four services use digest-pinned images on the same Linux platform.
   They share `localhost` with the command on an attempt-specific Docker bridge,
   with no published host ports. Each receives only its declared plain `env`,
   keeps its image entrypoint/user, and has two CPUs, 2 GiB memory and 512
   processes available. Docker enforces the declared readiness probe's
   five-second timeout; the worker waits up to three minutes for readiness,
   within the step's overall timeout. The command never starts after failed
   readiness, and a service that exits during it fails the step.

   Declared `go` and `npm` caches persist under the OS user's worker-capacity
   directory. Reuse is separated by local cluster enrollment, the runner's
   owner/repository/trust scope, clone host, platform, image and UID/GID. Pull
   request caches cannot populate trusted push/release caches. Only the
   selected cache directory is mounted at `/cache`. The worker retains at
   most 10 GiB between builds by evicting oldest directories; this is a
   retention budget, not a hard filesystem quota during a command. Caches
   remain advisory and may disappear before a later build.

   A kernel file lock reserves one build slot across all cluster enrollments
   and worker processes running as the same operating-system user, regardless
   of `MEMQL_HOME` or checkout location. Occupied capacity returns
   `pipeline_capacity_busy` before cloning. This bounds pipeline concurrency;
   it does not reserve resources consumed by agents or other OS users.

   Under that lock, the worker measures available space on the actual
   workspace filesystem. Low space or an unreadable measurement returns the
   same retryable capacity refusal with an explanation. After checkout it
   measures again; a refusal can be retried only after owned checkout cleanup
   succeeds. Failed cleanup retains the reservation and requires recovery.
   The engine waits within the run ceiling without recording a test failure,
   logging the capacity reason without repeating it on every poll.
   Free space or move the workspace to a larger disk to resume. The worker
   does not delete caches or images to make room. This is admission headroom,
   not a disk quota or a guarantee about a separate Docker data volume or
   storage consumed by programs outside the shared reservation.

   Local development builds use the same slot and retain a separate
   `local-build.json` record when their outcome is uncertain. Its presence
   blocks pipeline admission even after the process exits. Inspect it with
   `python3 scripts/lib/host_build_guard.py --inspect` from the local build
   checkout, then reconcile the recorded processes and image operations.
   The worker never removes this record or infers safety from a missing PID.
3. **A fresh directory**, `step-*` under `workspace_root`, readable only by
   you.
4. **The checkout**, the cluster Job's own clone script: `git init`, a depth-1
   `git fetch` of exactly the commit, `git checkout FETCH_HEAD`.
   - Over `https` only. `ssh` and scp-like URLs would fetch with this machine's
     SSH key, and plain `http` would send the token in the clear.
   - The step's short-lived token travels to git as an `http.extraheader` in
     git's `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_0` / `GIT_CONFIG_VALUE_0`
     environment: never on disk, never on a command line. No token means an
     anonymous fetch of a public repository.
   - Anonymous means anonymous: the fetch reads no global or system gitconfig
     (so no `insteadOf` can turn it into an SSH fetch with your key), resets
     every credential helper, carries none of the worker's `GIT_` variables,
     and never prompts. Proxies set through `HTTPS_PROXY` still apply.
5. **The command.** Native work uses `/bin/sh -c <command>` with the host's
   toolchains and environment, excluding `MEMQL_WORKER_TOKEN`. The runner sets
   `TMPDIR`, `TMP` and `TEMP` to scratch inside its owned checkout; requests
   cannot override them. Tools using these defaults have their temporary
   files removed with the workspace, including after failure or cancellation.
   Native commands still have the operator's filesystem access: a tool that
   deliberately writes elsewhere must declare and clean up those resources.
   Container work
   uses `/bin/sh` in the declared image with the checkout and any declared cache mounted, as the
   worker's UID/GID, and no Docker socket, privileged mode, host ports or host
   network. Its bounds are 512 processes and the admitted `cpuMilli` /
   `memoryMiB` request, capped by local policy (two CPUs / 2 GiB by default).
   Request environment and multiline secrets travel to the container shell
   over stdin, never to the host Docker client's environment or arguments.
   The worker checks Docker's terminal container state before accepting an
   exit status; losing the attached client is not a completed build.
6. **Output**, streamed to the cluster as it is written, stdout and stderr
   apart. Every secret value of four characters or more -- and each line of a
   multi-line one -- is replaced with `***` before it leaves this machine.
   Output leaves in whole lines; a line longer than 64 KiB is sent in parts cut
   where no secret straddles, and held back for as long as a secret longer than
   that may still be arriving in it.
7. **The timeout.** Past it, the whole process group is sent `SIGTERM`, and
   `SIGKILL` ten seconds later; the step fails as `timeout`. When the command
   ends, anything it left running in its group is killed, the way a pod's
   teardown ends what its container started.
8. **Artifacts**, the declared paths or globs relative to the checkout,
   directories taken whole, packed as a GNU `tar.gz` so long and Unicode
   names do not require PAX metadata. Only regular files travel,
   and never through a link that leads out of the checkout. At most 1024 files,
   256 MiB raw and 256 MiB compressed travel, subject to the engine's lower
   configured quota. A private archive is streamed in frames of at most
   256 KiB before the terminal result names its byte count and SHA-256 digest.
   Both sides spool to disk; the engine verifies the complete transfer before
   extracting anything. Over a size or file-count limit, the archive is
   left out and the result says `artifactsTooLarge`; the step keeps its own
   exit status. A declared path nothing matched is listed in
   `artifactsMissing`. The receiving engine verifies the complete archive and
   requires immutable storage receipts for every declared artifact. Missing or
   unverifiable artifacts fail the pipeline step even when its command exited
   zero; the command exit status remains available separately.
9. **Cleanup and recovery.** Container cancellation removes the exact attempt's
   command container, services, anonymous volumes and network with a separate
   thirty-second cleanup context. Every resource name is recorded before
   creation. Its reservation is
   cleared only after confirmed removal; an uncertain cleanup reports
   `pipeline_cleanup_uncertain` and retains the workspace and durable record.
   A replacement worker reconciles those recorded resources only after the
   original Docker daemon identifies itself. An unavailable or different
   daemon, a corrupt record, or interrupted native work keeps capacity blocked
   with `pipeline_recovery_required`. Native recovery needs operator inspection
   because this contract cannot prove that a process escaped no group.
   Completed work removes its checkout, including read-only trees, before
   releasing capacity or returning success. Filesystem failures retain a
   cleanup-only checkpoint and return `pipeline_cleanup_uncertain`. Once
   runtime teardown is confirmed, scratch cleanup can recover without Docker
   and without replaying the command. A worker reconciles its recorded attempt
   at startup and every minute while idle, including while disconnected from
   the cluster; it does not wait for another build to arrive. A local
   cleanup receipt does not authorize replaying an external publication or
   deployment; the engine must separately reconcile those effects.

The result is the step's exit code (or 128 plus the signal that ended it, as a
shell reports it), its duration, the artifacts and the missing paths:

```json
{"exitCode":0,"durationMs":41250,"artifactsTgzBase64":"H4sI...","artifactsMissing":[]}
```

| Failure | When |
|---|---|
| `denied_by_policy` | An agent dispatched the step, `pipelines.allow` is off, or `pipelines.repos` does not list the repository. |
| `bad_request` | The request is malformed: not an https clone URL, a clone URL that names another repository than `repository` (or no repository), not a full 40-character sha, no command, an environment name that is not one, a secret that shadows an environment variable, an artifact path that is absolute or contains `..`. |
| `pipeline_clone_failed` | git is missing (or is the macOS install stub), or the fetch or checkout failed; git's own reason is quoted. |
| `timeout` | The step ran past its timeout and was stopped. |
| `cancelled` | The cluster cancelled the step, the worker's stream to it was lost, or the worker is shutting down. |
| `exec_failed` | The workspace root could not be created, or `/bin/sh` could not start. |

A failing command is **not** a failure here: it is a result with a non-zero
`exitCode`.

---

## The environment a step sees

A step inherits the **worker's** environment, which is not your login shell's.

- **macOS (LaunchAgent).** The worker runs with launchd's `PATH`
  (`/usr/bin:/bin:/usr/sbin:/sbin`). Toolchains under `/opt/homebrew/bin` or
  `/usr/local/go/bin` are not on it. Add their directories to the PATH launchd
  gives your user's services (for example `sudo launchctl config user path
  "/opt/homebrew/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin"`, then log
  in again), or have the pipeline's command set `PATH` itself. On a Mac without
  the command-line developer tools, `/usr/bin/git` is an install stub that
  would raise a dialog; a step refuses rather than run it.
- **Linux (user systemd unit).** The installer's unit hardens the worker, and a
  step inherits it: `ProtectHome=read-only` leaves only `~/.memql` writable in
  your home, so toolchain caches there (`~/.cache/go-build`, `~/go/pkg/mod`,
  `~/.npm`) are read-only -- point them under `~/.memql` or the checkout from the
  step's command; `MemoryDenyWriteExecute=yes` stops JIT runtimes such as
  Node.js. Relaxing the unit (`systemctl --user edit memql-worker`) is a
  decision about this machine to make deliberately.

`shell.allow`, `shell.deny`, `shell.run_as_user` and the shell's `max_*`
limits do not apply to a step: the command is the pipeline runner's,
`pipelines` is its consent, and `max_timeout_sec` its limit. Shell resource
limits are applied inside the exec call's child before its command starts;
they do not change the worker or later pipeline steps. Limits imposed by the
worker's launch service still apply to every child. Restart the worker when
upgrading an older build that changed its own limits: lowered hard limits
cannot be repaired in that running process.

---

## Not doing this

- **A sandbox.** A step runs the pipeline runner's command as you; choose the
  repositories in `pipelines.repos` accordingly.
- **A consent window for steps.** The policy is the consent.
- **Guessing ownership after a crash.** Recovery uses the durable attempt
  record and the original Docker daemon, never an age-based sweep or a global
  Docker prune. Interrupted native commands and a crash while Git is still running
  still need operator reconciliation. The checkout identity is recorded before
  its directory is created, so these interruptions retain a specific ownership
  record; a pre-checkout crash is cleaned automatically. Do not
  delete a workspace merely because its name starts with `step-`.

## Local contract verification

The worker test suite clones fixture commits from a local repository. Set
`MEMQL_TEST_DOCKER_IMAGE` to an existing pinned Linux image to require the real
Docker cases; daemon failure then fails the tests rather than skipping them.
These verify the actual image/checkout, environment separation, multiline
secret masking, artifacts, cancellation/removal, and recovery of an orphaned
container, plus persistent caches and their enrollment/trust separation. Set
`MEMQL_TEST_PIPELINE_POSTGRES_IMAGE` to a pinned PostgreSQL-compatible image to
require the real service tests: an actual SQL query over localhost, service
credential/mount boundaries, cancellation and complete resource cleanup.
Separate process tests verify that cluster-specific workers cannot
reserve the same build slot and cannot discard an interrupted attempt record.
No test upgrades the installed worker or enables its pipeline policy.


## Shared host admission

Pipeline contract 5 carries container `cpuMilli` and `memoryMiB` through the
worker. An older worker is not eligible. Zero or absent values use the owner's
local defaults; a request above either local ceiling refuses before checkout.
The worker accounts for the command and every service together, keeping one CPU
and host RAM headroom outside that total. It reads host RAM and Docker metadata
before checkout and again before starting services or the command. Missing or
inconsistent evidence refuses. Docker admission subtracts current container RAM
usage and keeps at least 2 GiB or one quarter of its pool for runtime overhead;
this is conservative admission, not a reservation against unrelated applications.

Every enrollment under the same operating-system account shares one host build
lock with local builds, workbenches and local model calls. Inference shares that
lock with retained workspaces while excluding CI builds. Its admission accounts
for the workspaces' bounded memory so a goal can use a model, use its workspace,
then use the model again without discarding that workspace. A retained
`inference-active.json` witness contains only a version and random
attempt identity; interruption, cancellation or an uncertain runtime reply keeps
that fence. Its age or an exited PID does not prove completion. Only the original
call's confirmed terminal response clears its exact witness, even if the answer
then fails validation or cannot be delivered; otherwise operator
reconciliation is required. Existing resident-model and pressure rules remain.

Native macOS commands use this same shared admission and conservative CPU/RAM
headroom, but do not gain container hard limits. They remain an explicitly
trusted host capability. A Linux container lane requires the exact Linux Docker
architecture, digest-pinned image and current container contract; macOS native
execution is not interchangeable with Linux container execution.

Container attempts record the selected local Docker Unix socket and daemon
identity before creating resources. Measurements, execution and recovery stay
on that socket even if the current Docker context changes. Cleanup checks daemon
identity before and after removal. Retained older attempts without a recorded
socket remain fenced for operator reconciliation. Native commands declaring
`needs: [docker]` inherit that socket, but trusted native commands can explicitly
choose another endpoint; they do not receive the container isolation guarantee.

Upgrade the controller and opted-in workers together during an idle pipeline
window. Contract 4 and contract 5 do not substitute for each other: an older
controller cannot qualify the new worker, and the new controller refuses older
workers. Verify current registrations and their local resource ceilings before
resuming Fleet work. Enable pool placements and the installed placement policy
only after that coordinated upgrade; publishing the source alone does not
activate them.


## Isolated image verification on Linux

`imageBuild.verificationOnly: true` requests a real Dockerfile build and OCI
export for verification, then retains only the bounded
`.memql-image-build/verification.json` receipt. It does not publish an image or
produce a release artifact. The installation's pipeline selects source,
Dockerfile, context, target, platform and build arguments; the engine contains
no product-specific target list. Ordinary release image builds keep their
existing OCI artifact contract.

This path requires `linux/amd64`, pipeline action contract 5 and the separately
advertised `pipelineImageVerification: 1`. Existing pipeline/repository consent
still applies. Missing runtime tools or missing qualification suppress only this
feature, rather than preventing the worker's ordinary model or native work.
The controller checks the feature both when choosing a worker and on the agent
that actually holds its stream. A label is not evidence of this implementation.

The worker's existing service hardening remains unchanged. A separately
provisioned system socket starts one fixed non-root helper (`cockpit/cmd/memql-imageverify`), with root-owned
immutable helper, tool binaries, configuration and unit files. The service may
use subordinate UID/GID helpers; this is an explicit operator installation
choice, not a user-manager or policy bypass by the worker. It exposes only the
closed image-verification protocol to the configured Unix account. No manager
mutation, arbitrary command, shell script, Docker socket, host credential or
publication credential is accepted. Read-only systemd metadata binds the
actual helper PID, invocation, cgroup and fixed unit before any build can start.

CPU, memory, swap and process limits are read back from a delegated child
cgroup containing the helper, BuildKit, exporter and all build descendants.
The fixed outer service also has resource ceilings, an independent runtime
limit and control-group termination. The existing pipeline CPU/RAM settings are
still maxima. Private checkout/state usage and filesystem headroom are sampled
once per second; the selected state ceiling is at most 15 GiB. This is a
measured stop condition and can overshoot between samples, not a hard filesystem
quota. The builder has no shared cache. Its OCI export and build metadata stay
in private attempt storage and are removed after hashing; the evidence receipt
is the only declared artifact.

RootlessKit supplies separate PID, mount and network namespaces. A fixed filter
inside that network namespace permits public IPv4 and the isolated DNS relay,
and denies private/link-local/metadata ranges and IPv6. No host firewall is
changed. BuildKit keeps its normal per-build process sandbox and refuses
insecure entitlements. Qualification must show that source-controlled builds
cannot read the account's home, use host Unix sockets, or alter the outer
filter/cgroup. RootlessKit's loopback restriction alone is not that proof.

Before PREPARE, the worker retains the original request/source binding and the
actual service invocation in its existing account-wide capacity record. START
is permitted only after the service reports preparation and the worker verifies
its exact identity and effective child limits again. Disconnect, cancellation
or timeout stops the operation. The helper bounds graceful shutdown before
killing its original process group; systemd covers detached descendants. The
worker releases capacity only after the exact invocation is stopped, its cgroup
is empty, and private state is absent. An unloaded unit by itself is not proof.
An ambiguous START or failed cleanup stays fenced; another worker or cloud
fallback must not replay it.

A replacement worker may use the same fixed service solely to clean the retained
attempt, after proving the previous invocation cannot still run. Cleanup changes
its invocation identity but retains the original marker/source identity. A
crash during a second cleanup therefore remains recoverable. Partial preparation
is accepted only for an empty private directory or the exact expected marker
prefix; unknown state remains fenced for review. The marker survives mapped-ID
cleanup until the outer helper removes the final directory. Evidence paths are
reserved before source execution and written through retained directory
handles: a repository-provided directory, symlink or later replacement is
refused.

Availability additionally requires a root-owned qualification receipt binding
this installed helper/tool/config/unit set and current host runtime facts.
The installation also pins the operator-resolved immutable ELF loader/library
closure and `ld.so.cache`; changing a dependency invalidates the same proof.
Global `ld.so.preload` configuration refuses this optional builder rather than
being removed or ignored. Advertisement checks file identities through the
existing cache; admitted work hashes every selected dependency again.
Configuration, startup and portable tests cannot create that evidence. The
required real Linux fixtures cover target/export fidelity, aggregate resource
limits, namespace and network isolation, cancellation/timeout, worker restart,
ambiguous start/cleanup, and mapped state accounting/removal. Updating a tool,
unit, host namespace policy or relevant runtime invalidates the proof; an
unrelated DSL or controller revision does not. Per-request consent/source checks
remain fresh and separate.

On an AppArmor-enabled host, qualification also binds the existing distro
`/usr/bin/rootlesskit` profile's exact loaded attachment, mode and kernel policy
hash, plus its immutable source-file hash. The bounded per-profile read refuses
missing, ambiguous or inaccessible evidence; sysctls and an on-disk profile alone
do not prove what the kernel loaded. No new profile or system-wide relaxation is
installed. The distro's named user-namespace exception is not a claim that
AppArmor confines build instructions; the real service fixtures must still
establish the separate namespace, network and resource boundaries. See
`imageverify/service_apparmor_test.go` and the
[RootlessKit AppArmor requirements](https://rootlesscontaine.rs/getting-started/common/apparmor/).

Current source validation includes protocol bounds, original-marker recovery,
evidence symlink/swap refusal, resource evidence, actual exported-byte hashing,
and a Linux child that ignores termination while retaining output. Those tests
are in `imageverify/contract_test.go`, `protocol_test.go` and
`runtime_linux_test.go`; tool admission and lease tests are in
`tools/pipeline_image_verification_test.go`. These portable/container fixtures
are not real installed Linux/systemd/BuildKit qualification. Until the actual host fixtures
and all selected exact-source image checks pass, the feature must remain
unadvertised and the verification-only manifest changes unselected.

The service/fixture are reproducibly built from an exact reviewed source commit
for the operator's installation. This optional helper is not an extra Cockpit
release asset and does not alter the existing publication catalog. Its
`--render-image-verification-installation` port only renders the closed
configuration and fixed unit texts; it installs nothing. Its fixed read-only
`--check-image-verification-installation` port verifies immutable installed
code/dependencies and collects current host policy plus loaded socket metadata
before the short worker pause. It calls no build service, reads no credentials,
and cannot create or substitute for an attestation. The normal worker
binary has no setup or qualification bypass. An owner-run fixture may exercise
the same typed service before an attestation exists, while normal Run and
advertisement still require that attestation. Early qualification on older
workers requires a reviewed, temporary stop of the existing worker while idle:
older inference paths do not honor the newer durable build fence. Keep the
worker stopped after interrupted or uncertain cleanup; preserve its registrations
and settings, and restore it only after the exact service/cgroup/state check.

Runtime dependency identity includes the exact operator-resolved loader and
shared-library files, loader cache, and root-owned directory/SONAME alias chain.
Admission hashes the regular targets and compares every declared link; a cache
hit also checks link identity without rerunning an ELF resolver. A retargeted
link cannot borrow the unchanged old file's qualification. Global loader preload
refuses this optional capability. The operator captures this closed dependency
map during setup; `service_runtime_files_test.go` and
`service_runtime_aliases_test.go` cover file, cache and alias drift.

The explicit `TestInstalledImageVerificationQualification` in
`imageverify/installed_qualification_linux_test.go` records eight real-host
results, their evidence hashes, and a candidate `qualification.json`. It uses
the reviewed worker unit and inherited host reservation, and distinguishes the
installed runtime source from generated fixture commits. Portable controls
cannot produce that attestation. A failed or uncertain case preserves a failed
result and the worker stop; only complete evidence and confirmed cleanup permit
operator review and installation of the candidate. The network evidence uses a
known listening host endpoint and a DNS positive control; it does not claim live
nft readback.

The fixed `--validate-image-verification-attestation` setup port accepts only a
bounded candidate on stdin. As the configured nonroot owner, it rechecks the
immutable installation and fresh host/socket facts, applies the same closed
attestation validator as pipeline admission, and returns the exact candidate
byte hash. It does not write a file or independently prove the supplied fixture
hashes. The explicit root setup phase first joins all eight genuine evidence
files and cleanup results, then writes those same validated bytes only if the
fixed attestation path is absent. Normal worker admission still performs its
own fresh qualification checks; a successful validation response alone cannot
enable the capability.

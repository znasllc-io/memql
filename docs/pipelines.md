# Pipeline steps on this machine

Let the cluster run one of its CI pipeline steps here, when the step names a
need the cluster's own runners cannot meet -- a macOS toolchain, a GPU, Docker.

The arrangement is made in two places: the repository's pipeline says a step
may run on the fleet, and this machine's `policy.yaml` says this machine will
take it. Engine half: epic memql#5478 (`workerHost.pipeline_step`, issue
memql#5494). This half: the `pipeline_step` action, the `pipelines` policy and
the `pipelines=allowed` registration label and the action's reported repository
scope. A machine that does not report a scope is not eligible for a pipeline
step; upgrade and reconnect it first. The runtime also reports
`actionContracts["workerHost.pipeline_step"] = 3`. The engine requires this
contract before sending an explicit native/container request; operator labels
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
its declared image, checkout, declared caches and supplied environment. Native execution
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
| `container_memory_mib` | `2048` | Command container memory in MiB, from 256 through 32768. Docker's total memory-plus-swap limit is the same value, so no additional swap is available. Services keep separate 2 GiB limits; native commands are unaffected. |

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
   network. Its bounds are two CPUs, 512 processes and the local
   `container_memory_mib` setting (2 GiB by default).
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
   and never through a link that leads out of the checkout. Up to 64 MiB of
   files and 20 MiB compressed -- what one result can carry on the worker
   stream, whose messages are capped at 32 MiB. Over either, the archive is
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

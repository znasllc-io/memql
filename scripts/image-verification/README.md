# Image verification qualification setup

This is private source preparation, not an owner command ready to run. No
service, worker preference, package, firewall, model, or cloud resource has been
changed. The real Linux fixture and public source commit are still pending.

The eventual single copyable block has three explicit phases:

1. Verify an immutable public MemQL commit and its complete reviewed source
   inventory. Download the pinned prerequisites first. `qualify.py --phase=prepare`
   reserves the existing account build and inference locks and captures the
   actual selected worker unit's file and loaded settings without changing it.
   It builds the stdlib helper and fixture while the worker remains active.
2. Run the separately reviewed root provisioning block. It installs only the
   exact tools, configuration and fixed service/socket units produced by the
   pure renderer. This wrapper never runs a root installer, package manager,
   arbitrary command, or attestation installation.
3. `qualify.py --phase=qualify` continues only that exact prepared attempt.
   It checks the retained point-in-time worker/App observation, source, tool,
   unit/settings and reservation identities. Under the build and inference
   locks it refuses existing worker descendants, then immediately stops that
   selected unit and verifies its cgroup is empty. Only then does it invoke the
   fixed 45-minute real fixture with the inherited build lock. All eight
   retained evidence files, both binary hashes and the candidate attestation
   must match. Confirmed service/cgroup/mapped-state cleanup permits clearing
   the reservation and restarting only a previously active, unchanged worker.
   A previously inactive worker stays inactive.

Routine output is one JSON envelope on stdout. Real stage transitions and
failures go to stderr; full diagnostics stay in private prepare/qualify logs.
`--verbose` additionally prints the fixed commands. There are no percentages,
success claims from process exit alone, or silent retries.

## Binding and prerequisites

The final external plan binds the exact source SHA, complete imageverify Go file
inventory (including the real fixture), standalone helper main, unchanged
`scripts/lib/host_build_guard.py`, Go/systemctl executable hashes, the selected
worker unit name/path, eight fixture names and an actual owner-reviewed activity
observation. Host identity, distribution, username and observation belong in that
external plan; they are not defaults in these generic scripts.

The unit file and effective loaded settings hashes are captured locally during
prepare, then compared immediately before stopping and before resumption. The
owner does not need to approve an unknown hash in a separate conversation.
Drop-ins, transient units and a pending daemon reload refuse. Command outcome
timestamps/PIDs are excluded; arguments and environment values are never retained
as plain text.

The activity observation retains its original timestamp and hash. A copied setup
older than one hour refuses before the stop. That ceiling only bounds stale
setup; it is not an idle lease. Build/inference locks do not cover every App or
in-process worker action, and a zero-descendant check cannot exclude a new short
in-process action racing the stop. Use a fresh owned observation when handing off
the reviewed command, keep the stop window short, and report this limitation.
The existing per-home pause mutates enrollment and cancels work; it is not used
as a new drain mechanism. No enrollment or runtime setting is changed here.

Go builds use an isolated module, `GOWORK=off`, `GOPROXY=off`, `GOSUMDB=off`,
`GOTOOLCHAIN=local`, `GOENV=off`, CGO disabled and one compiler package at a time. A matching
verified local Go1.27.3 can be used. If missing, `prepare_go.py` extracts only the
previously downloaded official Linux/amd64 archive after its exact size and
SHA256 match. It performs no network access or system-wide installation. The
pin comes from [Go's official release metadata](https://go.dev/dl/?mode=json).
The actual official archive has not been downloaded/extracted in these tests.

Before either guarded phase, 20GiB must be available. During each child process,
the wrapper stops at less than5GiB available, the bounded operation deadline or
64MiB diagnostic log limit. It never removes caches, existing state or lock
inodes. The fixture/broker separately enforce the actual build resource ceilings.

Do not use `apt install nftables` as a convenience step. The reviewed plan may
extract an exact officially checksum-pinned `.deb` into private staging and
install only its verified tool/dependency closure into the fixed tool location.
[dpkg-deb extraction](https://manpages.debian.org/bookworm/dpkg/dpkg-deb.1.en.html)
extracts archive files; it is not package installation. No maintainer script,
nftables unit/configuration, service enable/start, or host rule load/flush belongs
in this setup. The actual distribution and dynamic-library closure still need
verification; extraction alone does not prove the tool can execute safely.

## Interrupted setup

The currently installed contract4 worker is not assumed to understand contract5
records or all new fences. Explicitly stopping it is the bootstrap boundary.
The normal build/inference locks are additional protection, not a claim that a
systemd service inherits them. A preparation failure leaves the reservation but does not claim the worker was
stopped. Once the fixture stop has been requested, failure, uncertain cleanup,
changed unit, or missing evidence leaves the reservation and worker stopped;
another fixture run refuses. Do not clear anything by age or PID absence.

No unit enable/mask setting is changed. An enabled worker can return after an OS
reboot. Reboot stops prior processes but does not prove mapped-state cleanup;
after reboot, inspect the retained attempt, current unit/idle state and exact
service/cgroup/state cleanup before explicit recovery or another fixture run.

This proposal leaves the41 release targets unchanged. No Linux installation,
isolation, real fixture, attestation, or Fleet advertisement is qualified by the
offline wrapper tests.

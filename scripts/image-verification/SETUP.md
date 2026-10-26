# Install and qualify the optional image verification service

This capability installs one fixed, nonroot image builder behind a private Unix
socket. It preserves the existing worker unit, sign-in, registrations and
configuration. The separate service permits subordinate-ID mapping; it does not
change the worker's `NoNewPrivileges` setting. Installation alone does not make
the capability available: the installed service must pass the real Linux
fixtures, and the final explicit `attest` phase publishes only that verified result.

Use an exact reviewed source archive and its independently recorded SHA256.
Extract it only after checking that digest. The `plan` phase compares every
helper/fixture source file and setup script with that archive, reads the actual
owner and immutable distro-tool identities, and writes a private local plan.
The source commit, host ID/version, selected existing worker unit and fresh owner-reviewed activity observation are explicit caller inputs.
The workspace follows the actual normal worker policy: `pipelines.workspace_root`,
then `fs.workspace_root/pipelines`, then `~/.memql/pipelines`. An existing
policy uses an already-installed safe YAML reader; an unavailable parser or
unsupported unit override requires an explicitly reviewed root and otherwise
refuses. The policy and unit bytes are bound across later phases. There is no
private file to transfer from another machine and no mutable branch
URL is execution authority.

The ordered phases are `plan`, `prepare`, `install`, `qualify`, and `attest`. Each returns
one JSON result; diagnostics go to stderr. `--help` lists the closed arguments.
The first two run as the selected nonroot account. `prepare` downloads the
source-bound official artifacts, checks complete archives and package control
scripts, and builds the helper and fixture under the shared resource guard.
Their bytes must match the recorded reproducible build. The existing worker
stays active. The root `install` phase requires
`--confirm=install-reviewed-image-verification` and the same plan digest. It
refuses any existing installation; it never upgrades or removes an installed
package, invokes apt, enables a unit, or changes a host firewall or AppArmor
profile. Only absent allowlisted packages may be installed; nft is extracted
as a single verified executable and is never installed as a package. Root
consumers use verified snapshots copied into a new immutable toolkit directory.

`qualify` first runs the installed helper's read-only preflight as the owner.
Missing loaded AppArmor, runtime-library, subordinate-ID or service evidence
refuses before the worker is stopped. The existing qualification wrapper then
rechecks the exact worker, current activity observation and account reservations,
records the stop intent, and pauses that worker only for the real fixtures.
An activity observation is point-in-time evidence, not a lease on future work.
Only complete evidence, confirmed cleanup and unchanged worker settings permit
restoring its original active state. An uncertain stop, timeout, cleanup or
package operation retains its journal and is not replayed automatically.

The fixed service ceiling is four CPUs, 8 GiB memory, 45 minutes and 15 GiB of
monitored private state. It uses the existing distro RootlessKit and runc;
unsupported options or policy refuse instead of weakening host restrictions.
The service socket is started for qualification, but is not enabled at boot by
this setup. The final root `attest` phase requires
`--confirm=publish-verified-image-qualification`. It reuses the existing fixture
receipt validator, then the installed Go validator checks the exact candidate
against fresh host and installation facts. Only those same verified bytes can
be written to the previously absent fixed attestation path. It preserves the
actual qualification source commit and refuses replacement or replay. This
second explicit sudo invocation may ask again if the earlier sudo approval
expires during qualification; the setup does not extend sudo authorization.
No passing offline test is proof that an actual Linux host is qualified.

The normal CI gate `TestImageVerificationSetupAndQualificationControls` runs
all `test_*.py` offline controls here. They cover source/artifact binding,
package refusal, immutable payload snapshots, aliases and loader drift,
whole-process-group cleanup, exact worker fencing and genuine receipt joins.
The real optional fixture is `TestInstalledImageVerificationQualification`;
it must retain all eight actual witnesses on the selected installed machine.

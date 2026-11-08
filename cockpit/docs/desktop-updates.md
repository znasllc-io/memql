# Desktop updates

The macOS menu and Linux tray retain their existing connection controls and
add About, installed build information, automatic release checks, and a
versioned update action. The arrow on the existing MemQL mark means a newer
stable Cockpit release is available. Network failure does not erase an update
already found. The user chooses when to install; periodic checks are read-only.

## Release and installation boundary

`memql update --check --json` reads the published release catalog in
`znasllc-io/memql`, follows pagination, ignores engine tags and prereleases,
and compares `cockpit/vX.Y.Z` by semantic version. Build metadata identifies
the installed source without changing version precedence. Checks run at menu
launch and hourly, with no overlapping requests.

`memql update --install --version=X.Y.Z --json` pins the version the person
selected and rechecks it before download. It selects assets for the current
platform, architecture and build variant. The release checksum must match,
the archive must contain safe paths, and the downloaded executable must
report the selected version and variant before replacement. On macOS, the
whole app is replaced together, including the menu helper; code signature
and bundle identity are verified. A stable signing identity cannot silently
be replaced by another identity or an ad-hoc build. For the established local
development workflow, the updater may sign the verified incoming worker and
menu with the already configured certificate in the user's Keychain. It must
then verify both against the installed signing requirements. It never creates
a key or weakens a requirement; an unavailable identity stops the update.

The update runs as the current user against the installed worker's executable
and existing service. It does not execute an installer downloaded from `main`,
enroll again, edit service definitions, enable a paused home, change consent,
or grant macOS permissions. An unsupported or protected installation keeps its
update indicator and displays a remedy instead of claiming it can update.

## Protecting work and reconnecting

The worker's owner-only control socket reports active work across all homes.
Tool calls, app sessions, interactive sign-ins, model calls and model downloads
prevent an update. After download and verification, `prepare-update` checks
the expected worker PID and acquires a two-minute admission reservation.
Checking for active work and accepting new work share one lock across the
homes, so a new request cannot slip between the idle check and reservation.
New requests receive their normal terminal response explaining that Cockpit
is updating. Pings and cancellation traffic continue.

This reservation is in memory. `cancel-update` releases it on failure; restart
removes it, and expiry releases it if the updater disappears. It never writes
paused state to the enrollment registry. The new worker starts with the same
registry, credentials, machine identity and policy. Its existing registration
handshake advertises the new stamped version and current capabilities to each
enabled cluster. A previously paused connection stays paused.

Replacement succeeds only after the new worker reports the selected version
and the same home identities and connection choices. If service restart or
worker readiness fails, the updater restores the prior files and attempts to
restart the previous worker. Reconnection has a separate bounded wait. An
unavailable cluster is reported as retrying rather than making a successful
local installation look like a connected worker.

## Progress and recovery

`memql update --status --json` reads the last local update receipt. Menus show
download, verification, installation, worker restart and reconnection progress.
Duplicate installs are locked out. Menus relaunch from the installed location
after a successful replacement and retain an honest distinction between an
updated worker and connected servers.

Before replacement, a private recovery record retains the selected version,
installed path and existing home identities/connection choices. If the updater
is interrupted after replacement, the next check uses that evidence to offer
**Restart to finish update**. This is an explicit action: it verifies the
installed version and saved homes, reserves the old worker when it is running,
and restarts the service without downloading the release again. A background
check never restarts a worker. An unrelated install or changed home registry
requires repair rather than guessing which settings to restore.

If an update fails, read its message and retry after fixing the named cause.
Finish active work first; start an unavailable worker before retrying. A
protected installation needs the existing installer under an account with
write access. Do not pair the machine again just to update it. A signing
identity change on macOS requires the installer migration and fresh permission
verification. A local certificate that is locked or unavailable must be restored
before retrying. The updater does not reset the privacy database or approve
Accessibility and Screen Recording on the user's behalf.

## Verification boundaries

The updater tests use a fixture release service and simulated service control
to exercise release selection, artifact verification, replacement, rollback
and reconnection. Worker tests exercise admission and paused-home persistence;
native menu tests exercise available, pending, failure and completion states.
macOS tests also build and sign scratch bundles, reject tampered executables,
and exercise whole-bundle exchange and rollback without installing an app.
These checks do not constitute a published-release upgrade against a live
cluster. Release qualification must exercise the actual packaged app on both
supported desktop platforms, verify the resulting worker version in Fleet,
and check macOS permissions against the restarted worker.

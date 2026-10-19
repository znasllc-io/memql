# macOS menu bar companion

## Native MemQL app (development; planned for 0.15)

Computer-use installations use a real `MemQL.app`. Its **main executable is
the Go worker**, with bundle identifier `com.znasllc.memql-worker`, display name
`MemQL`, and a native `.icns` generated from the canonical nine-node mark.
The Swift menu helper is embedded at
`Contents/Library/LoginItems/MemQL Menu.app`. Both appearances use the same mark;
the permission window shows one switch per grant and keeps PID/path/build
details under **Troubleshooting**.

The worker LaunchAgent targets `MemQL.app/Contents/MacOS/MemQL` directly, and
the installed `memql` CLI symlink points to that same executable. Status reports
`bundle_id` and `bundle_path` from the actual worker's Core Foundation main
bundle, rather than inferring identity from a filename. Finder reveals the app
reported by that worker. Opening MemQL from Finder opens its embedded menu;
invoking the `memql` CLI keeps normal command-line behavior.

The menu registers both bundles with Launch Services. The worker plist includes
`AssociatedBundleIdentifiers`; the active executable remains the app's main
worker. Registration and labels do not imply TCC grants: macOS must approve the
actual bundle, and only current-worker preflight results can confirm access.
Raw executable grants are not silently moved or reset during migration.

The token installer selects this layout for computer-use version 0.15 and
later, with the exact matching `memql-app-darwin-{arm64,amd64}.tar.gz` and SHA256
sidecar. Older releases and headless workers retain the standalone companion.
User-local installs use `~/Applications/MemQL.app`; system installs use the
root-owned `/Applications/MemQL.app`, preserving the system CLI's ownership
boundary. `--no-menu` skips the helper service; `--no-service` installs files
without starting either service.

Installation separates file placement from per-user service activation:

```sh
scripts/macos/build-app.sh --worker=/absolute/built/worker --version=VERSION
scripts/macos/install-app-files.sh --app=/absolute/built/MemQL.app \
  --destination=/absolute/Applications/MemQL.app --cli-path=/absolute/bin/memql
# Register after placement; this mode opens no window and starts no worker.
"/absolute/Applications/MemQL.app/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit" --register-bundles
scripts/macos/activate-app.sh --app=/absolute/Applications/MemQL.app
```

Existing service arguments, environment and log locations are preserved.
Previous bundles/CLI links and agent plists are retained for rollback. Known
legacy worker agents and the old standalone menu are retired; unrelated apps,
enrollments, credentials, policies and models are not modified. Activation
records the bundle fingerprints and avoids restarting an unchanged install.
The uninstaller removes only the standard matching bundle for the selected
prefix; custom destinations and rollback copies remain.

This layout fixes app attribution and presentation; it does **not** turn an
ad-hoc signature into a stable distribution identity. Developer ID provisioning
and signed-upgrade validation below remain necessary. The bundle's minimum
macOS version is read from the actual worker Mach-O build metadata.

## Uninstall and local lifecycle testing

Fleet composes the macOS uninstaller with the selected cluster URL; every flag
is optional, and a person copying the line need know nothing else:

```sh
scripts/install/uninstall-mac.sh --cluster=https://api.example.com
# Explicit whole-machine worker removal:
scripts/install/uninstall-mac.sh --all-homes
# Only the per-user install (~/.memql/bin, ~/Applications/MemQL.app); never sudo:
scripts/install/uninstall-mac.sh --cluster=https://api.example.com --user-local
# The plan only, nothing changed:
scripts/install/uninstall-mac.sh --cluster=https://api.example.com --dry-run
```

With neither `--cluster` nor `--all-homes` the script decides from the
machine's enrollment files: one enrolled cluster is removed as if
`--cluster=<its URL>` were given, none removes the runtime as `--all-homes`
would, and several are refused (exit 2) with the exact command for each. A
`--cluster` URL that matches no enrollment is refused (exit 3) with the
enrolled ones listed; matching ignores whitespace and a trailing slash and
compares the host case-insensitively, as the worker does. Without
`--user-local` the install shape is detected: the per-user shape, the system
shape (`/usr/local/bin`, `/Applications/MemQL.app`; sudo, asked for only when
one is present and after the script has said so), or both.

A cluster removal uses the worker's YAML decoder and enrollment identity rules
(`memql worker unpair --cluster-url`, 0.15.0+; an older installed binary
proceeds as `--all-homes` when the requested cluster is the only enrollment,
and refuses with exit 4 when others exist). It removes duplicate homes for that
cluster and repairs the legacy token mirror. If another home remains (including
a disabled home), the shared app, CLI, menu, policy and state stay. A running worker is stopped before the change and reloaded
for its remaining homes; a stopped worker is not started. The last enrollment
removes the shared runtime, current and known legacy agents, and standard app.
Uninstall waits up to ten seconds for launchd to remove each stopped service.
A directly opened embedded menu is terminated only after its UID, exact executable
path and mapped executable are verified; a helper that stays running prevents
app deletion. Other enrollments keep the shared menu running.
No cluster token files means a repeated removal can finish partial file cleanup.
Before deleting an installed bundle, full/last-home removal uses `tccutil reset`
for only Accessibility and ScreenCapture and the known installed worker/menu
bundle IDs. Failure retains the resolvable app and reports partial cleanup.
Another standard MemQL installation sharing those IDs blocks the reset. Run the
uninstaller as the current user, without sudo; privileged file deletion is separate.
If tokens remain but the worker binary is missing, a scoped removal of the only
enrollment proceeds as full removal and says so; with other enrollments present
it refuses (exit 4). An unreadable registry is refused (exit 5) before anything
is touched: restore the file or explicitly choose `--all-homes`.

Default removal keeps policy, state/logs, CLI credentials, cluster settings,
certificates, models and rollback backups. `--purge` additionally removes worker
policy, owned state and model runtime files, and refuses when another home remains.
Credential and backup directories, unrelated apps and custom app locations are
preserved. A successful `tccutil` call resets authorization decisions; it does
not promise that every Settings display row disappears. Refresh Settings and
remove only any remaining MemQL row manually. If the app was already deleted,
its bundle ID may no longer resolve: remove residual rows through Settings.
Uninstall never uses a service-wide reset, modifies another app’s approvals, or
edits a privacy database directly. Sibling-home removal never resets permissions.

Updates compare the installed and incoming designated signing requirements.
An unchanged requirement preserves existing authorization decisions. A changed
requirement clears only this user’s obsolete worker Accessibility and Screen
Recording approvals before replacing the app. The installer announces the reset
and asks for fresh approval. Another standard installation sharing the bundle
identifier blocks this repair; unknown signing evidence never triggers a reset.
A failed reset stops the update for retry; enrollments and other apps are retained.
Explicit prerelease versions are compared exactly for download idempotence so a
new local build is not mistaken for an already-installed build with the same
numeric version. Only fresh worker OS preflight results can establish readiness.

For the current installed app, the supported user-run recovery is:

```sh
/usr/bin/tccutil reset Accessibility com.znasllc.memql-worker
/usr/bin/tccutil reset ScreenCapture com.znasllc.memql-worker
```

Then turn on each switch in MemQL’s permission window, approve the current app
in System Settings, and restart once for Screen Recording. The GUI alternative is to remove MemQL’s
old row in each category and add the exact current app from Show MemQL in Finder.
These actions revoke/reset old approvals; they do not grant access themselves.
See Apple’s [app-scoped reset documentation](https://developer.apple.com/documentation/xcode/resetting-access-to-protected-resources-in-macos).
Stable continuity across different builds still requires a real signing identity.

For a local Fleet test, freeze the current installer, `lib.sh`, uninstaller,
computer-use binary, app archive and SHA sidecar behind a loopback-only server.
Set `MEMQL_INSTALL_RAW_BASE` to its `/scripts/install` directory,
`MEMQL_INSTALL_VERSION` to the exact dev version, and
`MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=1`; supply `--download-base` for its versioned
release directory. Raw binary HTTP downloads require that explicit opt-in and a
literal loopback address; redirects are refused. Published defaults remain HTTPS.
`python3 scripts/macos/local_install_test.py --base-url URL --version VERSION`
exercises the piped command in disposable homes, without services or OS bundle
registration. Run app package tests separately for simulated agent activation.

## Released 0.14 companion

Cockpit 0.14.0 and later include the companion in the macOS token installer
used by MemQL OS. Both headless and computer-use installations get the menu;
`--no-menu` skips it and `--no-service` skips both LaunchAgents. Older explicitly
selected worker releases skip the companion. The menu reads the worker
LaunchAgent's executable path, supporting both `/usr/local/bin` and user-local
installs without adding a second worker.

Each release publishes `memql-menubar-darwin-arm64.tar.gz` and
`memql-menubar-darwin-amd64.tar.gz`, each with a `.sha256` sidecar. The installer
verifies the digest, allowed archive paths, code signature, and matching worker
version. Packages contain the app, its installer, and the pinned capability
runtime; a user machine does not need Swift, Python, or a source checkout.
The ordinary macOS uninstall script also removes the companion and its agent;
custom destinations and previous app backups are kept.

For development, build the worker with `make cockpit-computeruse-host`, then build and install
the current user's menu companion with `make menubar-install`. Xcode command
line tools and the repository's pinned `../memql` sibling are required. The
build/install scripts use that sibling's shared capability-script runtime.

The menu app lives at `~/Applications/MemQL Cockpit.app` and starts at login as
`com.znasllc.memql-cockpit-menubar`. Its icon uses the canonical nine-node MemQL
mark from the engine's `brand/mark.svg`, rendered as a macOS template image.
The companion does not start another worker and does not acquire TCC grants.
The existing `com.znasllc.memql-worker` LaunchAgent remains the worker.

## Connections

Each server has **Pause connection** / **Allow connecting**. Pausing cancels
only that home's stream and in-flight work; other homes keep running. The
worker writes that home's `enabled` setting atomically to `workers.yaml`,
retaining its enrollment token, other settings and comments. A paused home
stays paused across worker relaunch; a supervisor with every home paused stays
available to receive local resume commands. Duplicate enrollments for one server must be resolved in `workers.yaml` before
changing their connection policy; the menu refuses an ambiguous pause.
Menu status distinguishes paused,
pausing, connected, connecting/retrying, and an unavailable background worker.

**Quit menu bar — worker keeps running** quits only the companion. It does not
change any server policy. The companion returns at the next login, or launch
`MemQL Cockpit.app` again.

**Open MemQL OS** uses a home's explicit `os_url` setting, or the known local
`https://os.memql.localhost/` ingress for `api.memql.localhost`. It never guesses
an OS URL for another API host. `os_url` must be an HTTP(S) URL without embedded
credentials, a query string or a fragment.

## Permission reporting

The worker reports passive current-process `AXIsProcessTrusted` and
`CGPreflightScreenCaptureAccess` results on Register and every 15-second
heartbeat. The menu also queries the running worker, rather than testing its
own process. Setup in Terminal can inherit Terminal's grants and does not prove
that the detached worker has permission. Grant the installed worker executable
in macOS Privacy & Security, not the menu companion. macOS may require a worker
restart after changing a grant; the UI never promotes a denied result merely
because setup succeeded or a checkbox was clicked.

The additive wire contract is `PermissionDecision` unknown=0, granted=1,
denied=2. `PermissionStatus` fields 5/6/7 carry accessibility/screen recording/X11
states, field 8 is `checked_at`, field 9 is `probe_context=worker-process`.
`Heartbeat.permissions` is field 8. The worker emits valid protobuf wire data
against the older engine pin and also works after a generated-code pin bump.
Legacy bools are true only for measured grants. Unsupported checks remain
unknown with an explanatory detail.

Linux computer-use builds report X11 granted only after a bounded passive
`xdpyinfo` query succeeds. A failed connection or missing display reports denied;
Wayland, missing `xdpyinfo`, or a timed-out probe report unknown. macOS TCC checks
are unknown on Linux. No input event or screenshot is part of reporting.
Linux semantics have fixture tests; live Linux validation is separate.

## Guided permission setup (development; not in 0.14.0)

**Set Up Permissions…** opens a native window with one switch for Accessibility
and one for Screen Recording, drawn like MemQL OS's switch. It opens once
automatically when a supported worker first reports missing access; opening the
window does not request any grant. Turning a switch on invokes
`AXIsProcessTrustedWithOptions` or `CGRequestScreenCaptureAccess` **inside the
running worker** through its private control socket, which puts MemQL in
macOS's list, and opens that Privacy & Security page. Acknowledgment means the
request was queued, not approved. Only one OS request may be pending at a time.
No app can approve these grants itself: the password or Touch ID prompt
belongs to System Settings.

While System Settings is open for an ask, the window floats beside it, the menu
reads the worker every second, and the switch shows the position asked for
under a line saying what to do there. Accessibility lands live: the switch
comes on and the window returns to the front as soon as the worker reports it.
Screen Recording reaches only a process started after the change, so its ask
offers **Restart MemQL** at once and stays open, past System Settings quitting
and the window closing, until a different worker process reports. Otherwise a
switch shows only the worker's report. While any of its windows is open the
menu is a regular app with a Dock icon, so a window behind System Settings can
always be found; it returns to the menu bar when the last one closes.

**Troubleshooting** holds **Show MemQL in Finder**, which reveals the installed
executable with symlinks resolved, **Restart MemQL…**, and the worker's
version, PID and executable. macOS can retain an entry for an older binary
after an upgrade; inspect the file instead of trusting its display name.
Recovery changes to existing grants remain explicit actions in System Settings.
During installation, a verified signing identity change resets only the
obsolete worker approvals through tccutil.

**Restart MemQL** asks for confirmation because it interrupts active work
and every server connection. It checks the service's loaded PID and program,
then uses `launchctl kickstart -k` for the current user's worker label. It does
not rewrite enrollments or connection policy. The window waits for a fresh
report from a different PID; a new process with denied access remains denied.
Reports older than 15 seconds, unavailable workers, and unsupported probes
cannot confirm readiness. Headless and older workers show an update hint.

The browser bridge is **`memql-cockpit://permissions`**, optionally ending in
`/`, with no credentials, port, query, fragment, or action path. It only opens
the window. Fleet must gate this link on a future released companion version
and offer **Install or update Cockpit** if the handler is unavailable. Opening
the URL is never evidence of installation or granted permissions. There is no
HTTP bridge and no remote grant/restart endpoint.

Local status adds `executable`, `bundle_id`, `bundle_path`, `permission_requests` and
`permission_request_pending`. The explicit CLI equivalent is:

```sh
# Read PID from the actual service status first; substitute that PID below.
memql worker control --action=status
memql worker control --action=request-permission --permission=accessibility --pid=123
memql worker control --action=request-permission --permission=screen_recording --pid=123
```

The server rejects unknown permissions, missing/stale PIDs, unsupported builds,
and overlapping OS requests. Passive status and heartbeat paths never prompt.

### Signing and upgrades

The current release binaries are ad-hoc signed. Their designated requirements
can contain the build's code hash, so an unchanged path or filename does not
establish continuity across upgrades. A fixed identifier added to another
ad-hoc signature does not solve this. See Apple's
[code-signing requirements](https://developer.apple.com/documentation/technotes/tn3127-inside-code-signing-requirements)
and [DTS confirmation of permission loss with ad-hoc builds](https://developer.apple.com/forums/thread/819406).

Distribution signing needs a provisioned **Developer ID Application** identity
and a stable worker signing identifier (planned: `com.znasllc.memql-worker`).
The menu retains `com.znasllc.memql-cockpit-menubar`. Before enabling signed
releases, provision the certificate/private key securely in CI, sign both native
architectures before hashing and packaging, verify the designated requirement
and Team ID, and validate notarization and upgrade behavior on a test Mac.
Release signing must fail if requested credentials or signature verification
are missing; it must not fall back silently to ad-hoc signing. The initial
migration from ad-hoc to Developer ID can require user approval again.

This change does not provision signing credentials or promise grant retention.
No certificate or workflow-signing configuration is modified by onboarding.

## Logs and local control

**Show Logs** opens a native window with text search, server and severity
filters, and follow/pause. It shows the latest 300 entries from a bounded tail
of the local worker log. The worker strips secrets and URL paths/queries and
admits only diagnostic attributes into structured entries. Logs remain local;
there is no upload/export endpoint. When the service is down, the CLI can read
the current user's default worker log directly for troubleshooting.

The current OS user controls their own worker. There is no cluster-role gate.
The local socket is `~/.memql/control/worker.sock`, inside an owner-only `0700`
directory, with mode `0600` and peer-UID checks on both ends. The server never
accepts arbitrary log paths. CLI equivalents:

```sh
memql worker control --action=status
memql worker control --action=logs
memql worker control --action=home --home=example --enabled=false
memql worker control --action=home --home=example --enabled=true
```

The control service refuses to share a live socket with a second worker.
Single-home CLI overrides are not controlled by the menu; use the normal
`workers.yaml` supervisor for menu controls.

## Local rollback

Keep a copy of the previous worker executable before updating it. Changing an
ad-hoc signed executable changes its code identity and may require macOS grants
to be renewed. Replacing the worker must preserve its symlink, configuration,
policy and service arguments. The menu installer changes only its own app and
LaunchAgent and leaves a previous app bundle in the staging directory beside
the destination when replacing one.

## Release inventory

The release workflow attaches a CycloneDX JSON and XML SBOM for each of the
eight worker binaries after all builds finish. Each inventory is read from the
actual artifact and includes its binary hashes, build settings, and Go modules.
The tool does not execute downloaded binaries. Relationships and licenses not
present in build metadata are not inferred. These files replace the old
module-wide SBOM job, which failed while resolving unused engine modules at
unpublished placeholder versions; they do not inventory native system libraries
or the Swift companion.

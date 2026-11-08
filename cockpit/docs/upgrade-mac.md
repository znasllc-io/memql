# Upgrade an existing Mac Cockpit

Use the tokenless upgrade command once for a native MemQL.app that predates
menu updates. Finish active work first: older workers cannot report whether
they are busy. `--confirm=restart-worker` explicitly permits their one-time
stop and restart.

Download `cockpit/scripts/install/upgrade-mac.sh` from the chosen published
`cockpit/vX.Y.Z` tag and run it as the signed-in desktop user:

```bash
bash upgrade-mac.sh --version=X.Y.Z --confirm=restart-worker
```

Replace `X.Y.Z` with the published Cockpit version. The wrapper and its helper
runtime must come from that same release. It discovers the existing per-user
worker LaunchAgent, verifies the selected native release's checksum and code
signature, and delegates the upgrade to that release's updater. Existing
`0.22.1+...` computer-use installations are supported. The executable path,
build variant, enrollment, worker identity, saved servers, paused connections
and policy remain in place. No enrollment token is needed.

The updater preserves compatible signed identities, including an existing
local signing certificate when its private key is available. It refuses a
signed identity change it cannot preserve. Ad-hoc releases may require macOS
permission approval again; the upgrade never resets privacy permissions or
changes consent. No setup wizard runs.

Only an existing running menu from this app restarts after success. A stopped
or absent menu stays stopped or absent. Repeating the command is safe: an
already current worker is left in place, and an interrupted upgrade resumes
only from its verified saved recovery record. A menu restart failure retains
the worker update result and names the remaining step. The updater reports
installation separately from reconnection, so an offline cluster does not
erase a successful upgrade.

This command supports user-writable native apps managed by the canonical
per-user worker LaunchAgent. It refuses administrator-protected applications,
headless Mac binaries, and unrecognized services. Use an administrator-managed
upgrade for protected installations, or explicitly move to a user-local
installation before enabling menu updates. Do not run it with `sudo`.

After this one-time upgrade, future releases appear in the menu bar. **Update
and Restart** reserves an idle worker, installs the matching native release,
then verifies that the worker resumes its saved connections.

Offline fixtures in `scripts/install/upgrade_mac_test.py` verify exact version
and path routing, checksum/signature refusal, paused menu handling, standalone
downloads, and preservation of successful update results after a later error.
The updater tests cover service identity, worker admission, interrupted
replacement recovery, and saved connections. No test modifies an installed
application or the user's live services; a packaged-release update on a real
Mac remains a release qualification check.

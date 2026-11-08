# Upgrade an existing Linux Cockpit

Use the tokenless bootstrap command once when the installed Cockpit predates
menu updates. Finish active work first: older workers cannot report whether
they are busy, and the explicit confirmation permits this one-time restart.

Download `cockpit/scripts/install/upgrade-linux.sh` from the chosen published
`cockpit/vX.Y.Z` tag and run it as the signed-in desktop user:

```bash
bash upgrade-linux.sh --version=X.Y.Z --confirm=restart-worker
```

Replace `X.Y.Z` with the published Cockpit version. The wrapper and its helper
runtime must come from that same release. It discovers `memql-worker.service`,
keeps its selected executable path and build variant, verifies the selected
release's checksum, and delegates replacement to that release's updater.
It requires no token and does not pair again, rewrite service definitions,
enable a paused server, change app permissions, or edit enrollment and policy.
An existing active tray restarts after success; an inactive tray stays inactive.

Running the same command again downloads the verified release helper, which
leaves a healthy, current worker installed. If an update was interrupted, the
helper validates its saved recovery record before resuming. A later menu failure
retains the successful worker update in the command's result and names the
menu restart remedy.

The new updater verifies the existing installation, preserves its saved homes,
retains rollback information, and checks the restarted worker. An update is
reported separately from cluster reconnection. Later releases appear in the
tray, where **Update and restart** uses the normal idle reservation.

This command supports existing user-writable installations. A protected
installation, such as a root-owned binary under `/usr/local/bin`, is refused.
Use that installation's administrator-managed upgrade procedure, or explicitly
migrate it to a user-local installation before enabling menu updates. Do not
run the bootstrap with `sudo`: the worker, credential store and systemd user
service belong to the signed-in user. The command never silently weakens an
installation's file ownership or changes its service path.

The offline fixtures in `scripts/install/upgrade_linux_test.py` check variant
and path preservation, checksum and archive rejection, explicit confirmation,
protected-install refusal, and active-versus-inactive tray handling. They use
a simulated updater; the updater's own tests cover replacement, rollback,
legacy-worker bootstrap, and reconnection. A packaged-release update on a real
Linux desktop remains a release qualification check.

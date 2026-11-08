# Install and uninstall presentation

The Fleet machine installers use the same minimal design direction as MemQL
OS: a clear hierarchy, concise copy, and one clear next action. The
`frontend-design` skill informs the design and visual review, including for
terminal output. Keep the established MemQL identity; a terminal is not a
reason to invent a second logo or fill the screen with implementation details.
Both installers and uninstallers use the renderer in `lib.sh`.

- **Artwork:** `native/macos/mark.svg` is the source. `render-mark.py` samples
  its actual geometry into terminal cells; never hand-draw an approximation.
  Run `python3 scripts/install/render-mark.py --write` after changing it.
  Non-Unicode, narrow, plain, and redirected output use the wordmark alone.
- **Progress:** one active row with a signal moving between three nodes.
  This is indeterminate activity, not a made-up percentage. Completed rows
  state the result. An open circle means a follow-up is needed.
- **Color:** use the terminal's foreground and background, with a green accent
  and muted secondary text. Honor `NO_COLOR`, `TERM=dumb`, and `--plain`.
- **Copy:** say what happened and what the person needs to do. A current
  binary is “up to date.” Starting a service is not proof of a cluster
  connection; Fleet confirms that. Pending model approval is distinct from
  a failed install. Preserve optional setup and recovery functionality.
- **Diagnostics:** quiet by default; `--verbose` streams redacted details.
  Each run creates a mode-0600 log and names it in the summary. Preserve
  actionable failures and exit codes. Keep password prompts visible.
- **Removal:** distinguish a cluster disconnection (shared Cockpit stays), a
  complete uninstall, nothing left to remove, a preview, and a partial failure.
  Missing-file notices belong in the log. Retain the full removed/kept ledger
  and exact recovery commands there and in `--verbose` output. Dry runs use a
  temporary log and never change enrollment, services, or user configuration.

Logs live in `~/Library/Logs/MemQL` on macOS and
`${XDG_STATE_HOME:-~/.local/state}/memql/{install,uninstall}` on Linux.
`MEMQL_INSTALL_LOG_DIR` can redirect diagnostic storage, including in test
fixtures. Choose a directory outside any worker data you intend to purge.

## Preview without installing

```bash
bash scripts/install/preview.sh --scenario=models-pending
```

Other install scenarios: `installed`, `current`, `failure`. Uninstall scenarios:
`disconnected`, `uninstalled`, `uninstall-failure`, `uninstall-plan`.
Add `--plain` or
`--verbose` to inspect fallbacks. This uses the real renderer and synthetic
steps. It only writes a temporary diagnostic log; it never downloads a
release, edits an enrollment, starts a service, or sets up models.

Review the preview in a terminal with both light and dark backgrounds before
shipping a visual change. Keep the mark proportional, stage text aligned,
and the final command on one physical line for copying.

## Verification

Native `make test` includes this directory's Go test wrapper, which runs the
offline discovery, renderer, shell-library and menu fixture suites. Missing
Bash or Python fails that check. Fixtures use temporary homes and inert
binaries; they never install a published release or enroll this machine.

The Python fixtures inherit the Go suite's deadline through
`MEMQL_INSTALLER_TEST_DEADLINE_UNIX_MS`. They spend one shared budget, with a
small reserve to terminate fixture process groups and report captured output
before Go stops the suite. Standalone Python runs have one ten-minute budget.
The former 8/10/15/20-second child limits could reject healthy installers on a
busy native worker even while the enclosing lane still had time: the November
7 baseline pipeline timed out the Linux current-version presentation fixture
at ten seconds. This changes fixture scheduling only; production network and
operation timeouts stay in place. `fixture_process_test.py` verifies inherited
and expired budgets, the cleanup reserve, retained diagnostics, and termination
of descendants that keep output pipes open after their parent exits.
Cleanup operations share the reserve rather than each starting another timeout.
Terminal fixtures also terminate their process group after EOF: macOS can report
EOF with a live orphan, and can reject signals to a zombie-only group with
`EPERM`. Only a successful, bounded inspection showing that exact group is absent
or contains only zombies permits ignoring that error; live members or a failed
inspection preserve the denial. The presentation suite covers the live orphan,
and the process helper tests cover both harmless and genuine permission errors.

```bash
python3 scripts/install/render-mark.py --check
python3 scripts/install/fixture_process_test.py
python3 scripts/install/ui_test.py
python3 scripts/install/release_discovery_test.py
bash scripts/install/lib_test.sh
bash scripts/install/menubar_test.sh
shellcheck -x --source-path=scripts/install scripts/install/*.sh
```

`install_ui_stage` invokes callbacks in the current shell so enrollment,
binary, and native-app state survive. Do not wrap callbacks in `if`, `!`, or
`||`: Bash disables `errexit` inside those functions. The exit handler stops
animation, restores the cursor, drains diagnostics, and preserves the failing
command's exit code. Keep these paths tested on macOS Bash 3.2 and Linux.

## Release discovery and publication

Fleet's Mac/Linux installers and `cockpit/install.sh` share `release-version.py`.
They read every page of the MemQL release catalog and select the highest stable
`cockpit/vX.Y.Z` version. Engine tags, drafts and prereleases do not qualify;
publication order cannot make an older maintenance release become latest.
A next-page link must stay in that catalog: the same host and path, or GitHub's
`/repositories/<id>/releases` name for it, which it uses once a catalog passes
100 releases. A failed or incomplete catalog refuses installation instead of
choosing from partial results. Explicit version pins bypass discovery. A checkout uses its
own helper; a piped entry point fetches it from the same raw source base, never
from the operator's current directory or a leftover temporary helper.

Publication must finish all supported platform assets and checksum manifests
before the release becomes public. No published Cockpit release means the
publisher must complete that first release; a tag or source migration alone
does not make an installable version. There is no fallback to the retired
repository, an engine release, or an unchecked binary. The standalone installer
also requires the archive's checksum before installing it.

Published requests use bounded HTTPS transfers. Offline fixtures can supply
`MEMQL_INSTALL_RELEASE_API=file://...`; an HTTP fixture needs the explicit
`MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=1` opt-in and a literal loopback address,
with redirects refused. This override never changes production defaults.

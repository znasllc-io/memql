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

```bash
python3 scripts/install/render-mark.py --check
python3 scripts/install/ui_test.py
bash scripts/install/lib_test.sh
bash scripts/install/menubar_test.sh
shellcheck -x --source-path=scripts/install scripts/install/*.sh
```

`install_ui_stage` invokes callbacks in the current shell so enrollment,
binary, and native-app state survive. Do not wrap callbacks in `if`, `!`, or
`||`: Bash disables `errexit` inside those functions. The exit handler stops
animation, restores the cursor, drains diagnostics, and preserves the failing
command's exit code. Keep these paths tested on macOS Bash 3.2 and Linux.

# MemQL for VS Code

Language support for MemQL (`.memql`) files, powered by the offline
`memql-lsp` language server (which embeds the same MemQL Sense brain the
Cockpit uses). Works fully offline against local files -- no cluster, no auth.

## Features

- Syntax highlighting (TextMate grammar, generated from the DSL spec) refined
  by semantic tokens from the server.
- Live diagnostics (errors and warnings) as you type, including cross-reference
  resolution: unknown import modules/ids and signature concepts that resolve to
  nothing.
- Context-aware completion (constructs, concepts, functions, annotations, ...),
  including segment-aware `use`-line completion (namespaces -> kinds -> ids) and
  kind-filtered in-body invocation completion.
- Hover documentation and signature help.

## Runtime panel

An activity-bar panel connects the extension to a running cluster: pick a
cluster from `~/.memql/clusters.yaml` (the same file the memQL Cockpit
uses), browse every registered concept grouped by domain, and inspect rows
-- paged, with live detail -- without leaving the editor. It requires a
trusted workspace, since it reads credentials and opens a network
connection. See [VS Code Runtime Panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md).

## Cluster lifecycle

A cluster's whole life is reachable from the Clusters view: add it, repair
it, remove it from the editor, or take it off the machine.

**Add.** The **+** in the view title opens the **Add a cluster** page. What
it offers depends on what is already here -- it looks for an install receipt,
a `local: true` entry, and whether that cluster answers -- so a machine with
nothing on it is offered an install, and a machine that already has a local
cluster is offered a repair or an uninstall instead. Registering a cluster
that runs somewhere else is one of the choices on the same page. Nothing in
this surface hands you a command to paste into a terminal.

**Repair.** The install graph, run again. Every step verifies before it acts
and skips whatever is already satisfied, which is what makes re-running an
install a repair rather than a second install: there is one graph and one run
path, and only the wording differs. Reachable from the page, and from the
cluster panel's primary control when the cluster is registered but not
answering.

**Remove.** The trash can beside a cluster row. It drops the registry entry
from `~/.memql/clusters.yaml`, deletes the credential this editor stored for
that cluster, and closes the connection if it was the live one. **Nothing on
the machine is touched**: the cluster keeps running, its data is untouched,
and you can add it back at any time.

**Uninstall.** The row's context menu, on local clusters only. It reverses
the install receipt -- the k3d cluster, the hosts-file entries, the mkcert
CA, the pinned tools -- and there is no undo, because a deleted k3d cluster
takes its database with it.

Remove and Uninstall are separate commands with separate labels, menus and
confirmations, and that separation is deliberate. One is a routine edit to a
list and the other is irreversible; a single action that asked which you
meant would put the irreversible one a click away from the routine one.
Uninstall is contributed only on rows this editor installed and never as an
inline icon, so aiming at the trash can cannot land on it. It confirms
against an itemised dry run rather than a yes/no prompt: every artifact the
receipt names, what will happen to it, and which steps will ask for
elevation. Anything the install *found* rather than created -- an mkcert CA
that was already on the machine -- is listed as preserved and left alone.

The same install runs from a terminal for scripted and CI use, and is not
deprecated: `npm run install-cli -- install` and `... -- uninstall`. It is
not a second implementation. `src/install/session.ts` holds the
orchestration and both the page and `src/install/cli.ts` are callers of it,
so there is no second run path to drift out of step.

Operator-facing detail, including what the page collects before it starts:
[VS Code Runtime Panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md).

## Install / update the extension locally

One command builds the extension (building a fresh `memql-lsp`) and
(re)installs it into VS Code:

```bash
make vscode-install          # from the repo root
```

Then reload the editor to pick up the new server: run **Developer: Reload
Window** (Cmd/Ctrl+Shift+P), or restart VS Code. Because the language
intelligence lives in the bundled `memql-lsp` binary, this is the loop to run
every time you change the server or the extension -- `--force` overwrites the
installed build, so re-running it just updates in place.

Options (via `scripts/vscode/install.sh`, or `EDITOR_CMD=` on the make target):

```bash
make vscode-install EDITOR_CMD=cursor        # install into Cursor / code-insiders / codium
bash scripts/vscode/install.sh --no-build    # reinstall the last-built .vsix (skip the rebuild)
bash scripts/vscode/install.sh --help
```

If the editor CLI is missing, run "Shell Command: Install 'code' command in
PATH" from the VS Code command palette.

## Requirements

The `memql-lsp` binary, for the LANGUAGE FEATURES only. The extension resolves
it in this order:

1. The `memql.lsp.serverPath` **user** setting, if set (see Settings below --
   a workspace-scoped value is refused).
2. A bundled platform binary at `bin/<platform>-<arch>/memql-lsp` (added at
   packaging time -- `make vscode-install` / `make vscode-package` bundle it).
3. `memql-lsp` on your `PATH`.

Build it from the memql repo with `go build -o bin/memql-lsp ./cmd/memql-lsp`.

When none of the three resolves, the extension says so and keeps going: only
highlighting, diagnostics, completion, hover and signature help are lost. The
runtime surface -- the Clusters, Concepts and Runs views, and connecting to a
cluster -- needs nothing from the language server and is unaffected. (The Run
CodeLens does need it, because the constructs it offers are read from the
server.)

## Development

```bash
make vscode-deps                 # from the repo root -- see below
cd editors/vscode && npm ci && npm run compile
```

`make vscode-deps` is not optional on a clean checkout. The extension consumes
`sdk/ts` and `sdk/ts-viewkit` as `file:` dependencies, and their `main` /
`types` point into `dist/` -- which does not exist until those packages are
built. Skipping it leaves the symlinks resolving to nothing and `tsc -p ./`
fails.

Press `F5` to launch an Extension Development Host, set `memql.lsp.serverPath`
to your built binary, and open a folder of `.memql` files.

### Testing

```bash
make vscode-test        # unit lane -- bare node --test, seconds, no Electron
make vscode-test-host   # host smoke lane -- downloads and drives a real VS Code
```

The two lanes answer different questions. `vscode-test` covers the modules that
do not import `vscode`; it is fast and dependency-light and must stay that way.
It also covers `package.json` itself, because the tree's context menus are
decided by `when` clauses the workbench evaluates and no host API can read back
the entries it drew -- a clause edited to match no row would otherwise remove an
action from the product with nothing noticing (`test/clusterMenus.test.ts`).
`vscode-test-host` (`editors/vscode/test-host/`) launches a real Extension
Development Host to assert what a unit test structurally cannot reach -- that
activation survives the host's runtime, that every command the manifest
contributes is actually registered, that the activity-bar contributions were
accepted, that a file watcher fires for a path outside the workspace, and that
each webview opens. It needs a display, and falls back to `xvfb-run` when
`DISPLAY` is unset. CI runs it against both the declared `engines.vscode` floor
and current stable, because that floor is where this bug class actually fires.

Neither lane dials a cluster. Everything downstream of a connection is verified
by hand against the [manual verification
checklist](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel-verification.md).

## Settings

- `memql.lsp.serverPath` -- absolute path to the `memql-lsp` binary. **User
  settings only.** A value in workspace settings (`.vscode/settings.json`) is
  refused, and the extension shows a warning saying it was: an opened folder is
  not trusted to name an executable this extension then runs, so honouring one
  would hand any repository arbitrary code execution. Set it in User Settings.
- `memql.lsp.trace.server` -- `off` | `messages` | `verbose`.

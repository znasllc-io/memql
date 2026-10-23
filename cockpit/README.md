> Cockpit now ships from the MemQL monorepo's `cockpit/` directory. Build and
> test it against the engine in the same checkout. Its command, native apps,
> consent and worker compatibility remain separate from engine node binaries.
> Releases use `cockpit/vX.Y.Z` in `znasllc-io/memql`.

<p align="center">
  <img src="assets/logo.svg" alt="MemQL Cockpit" width="500">
</p>

<h1 align="center">MemQL Cockpit</h1>

<p align="center">
  <strong>The fleet worker runtime and cluster CLI for <a href="https://github.com/znasllc-io/memql">MemQL</a>.</strong><br>
  Installed as the <code>memql</code> command on the machines you enroll in your fleet.
</p>



<p align="center"><sub><em>Designed and built with Claude as co-author.</em></sub></p>

> **Status: Alpha / pre-1.0 — not production-ready.** MemQL Cockpit is under
> active development and tracks MemQL core. The worker contract and
> configuration are still evolving; expect breaking changes between releases.

---

## Fleet workbenches

Fleet workbenches are isolated containers on an enrolled machine. They are
separate from Cloud workbenches and from commands that run on your own computer.
Enable them during Fleet setup with `--workbench`, or run:

```sh
memql worker setup --workbench
```

Docker must be running locally with Linux containers, at least 3 CPUs and 4 GiB
of memory. Setup downloads a pinned Python runtime and enables `workbenches.allow`
in the local policy. The runtime provides Python and shell/file tools without
network access, host mounts, a Docker socket or signed-in apps.

Two workspaces share the operating-system account's capacity across Cockpit
processes and enrollments. Each is limited to 1 CPU, 1.5 GiB memory and a 256 MiB
workspace. Admission preserves memory headroom. CI builds and Fleet workspaces
cannot occupy that capacity together; interrupted work is cleaned up before
capacity is reused. Workspaces expire after five idle minutes or one hour.

Choose recipients in **Fleet → Machine settings → Sharing**. Model inference
and Fleet workbenches have independent audiences and Pause controls. Pausing
keeps your recipients and finishes admitted work, while blocking new borrowed
calls. Commands/files, signed-in CLIs/apps and computer use stay owner-only.

## What is MemQL Cockpit?

MemQL Cockpit turns a machine you own into a **worker** in your MemQL fleet:
agents on the cluster can dispatch shell / filesystem / HTTP work to it
(headless), and — on the opt-in computer-use build — drive its mouse, keyboard
and screen. Machines are managed from the MemQL Portal's Fleet section:
pairing, labels, routing policy, activity.

The installed command is **`memql`**:

- **Enroll a machine** — `memql cluster add <domain>` registers the cluster
  (identity discovery + OAuth sign-in; RFC 8628 device flow on headless / SSH
  boxes) and `memql worker pair <code>` redeems a pairing code from the portal.
- **Run as a service** — a per-user LaunchAgent on macOS
  (`com.znasllc.memql-worker`) or user-systemd unit on Linux
  (`memql-worker.service`), auto-started at login.
- **Two build variants, one command** — headless (default, CGO-free) ships
  from releases as tar.gz archives; computer-use (`-tags computeruse`, CGO +
  RobotGo) ships as prebuilt `memql-computeruse-<os>-<arch>` release assets,
  built natively per platform. `memql --version` names the variant.

It communicates with MemQL clusters over gRPC (`MemqlService.Stream` and
`WorkerService.Stream`) and does not embed the MemQL engine.

> Note: the MemQL engine repo also builds a binary named `memql`, but it ships
> only inside container images and runs in pods. This CLI is what gets
> installed on operator machines — different channels, no PATH overlap.

## Install

```bash
# one-liner (detects OS/arch, checksum-verified)
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/install.sh | sh

# pin a version, or an install dir
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/install.sh | MEMQL_COCKPIT_VERSION=v0.10.0 sh
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/install.sh | BIN_DIR=/usr/local/bin sh
```

For a source build, clone the MemQL repository and build `cockpit/` in that
checkout as described below. Cockpit uses the engine from the same commit; a
standalone `go install` cannot supply that workspace.

Worker-machine installers (binary + service + worker.yaml in one step) live in
`scripts/install/` — the MemQL Portal's Fleet page composes the exact
one-liner for you when you add a machine. Their `--computeruse` flag installs
the prebuilt computer-use binary from the same release; building it from
source (below) remains the alternative. Their `--inference` flag runs
`memql worker setup --inference --non-interactive` once the worker is up, and
never fails the install over it — a machine that paired fine and could not set
up local models is still a working worker. Their `--version=X.Y.Z` flag pins
the release to install (`0.16.0` and `v0.16.0` both work; the default is the
latest release); `--download-base` still wins for *where* to download from
when both are given. Re-running the installer over an older install upgrades
the binary in place and refreshes the enrollment for the same cluster (the
same token and cluster is a no-op refresh); a newer installed version is
never downgraded. The closing block shows the installed version, worker status,
and any remaining model setup. Add `--verbose` for live diagnostic output or
`--plain` for output without artwork, color, or animation. Each run saves a
private, credential-redacted install log under `~/Library/Logs/MemQL` on macOS
or `${XDG_STATE_HOME:-~/.local/state}/memql/install` on Linux; failures name the
log to inspect. Worker runtime logs remain in `~/.memql/state/worker.log`.
An existing `~/.memql/workers.yaml` the installer cannot read is
refused (exit 5) before anything is downloaded or written, rather than
rewritten without the enrollments it holds (see the registry bullet under
Uninstall).

On Linux desktops, installation also adds the **MemQL Cockpit** tray icon and
starts it at desktop sign-in. It uses StatusNotifierItem/DBusMenu, including on
Pop!_OS COSMIC with its system tray applet. The menu shows worker connections,
opens MemQL OS and redacted logs, and pauses or resumes a server connection.
It does not start a second worker. SSH installs prepare the menu for the next
desktop sign-in. `--no-menu` on the worker installer skips the companion;
`--no-service` skips both the worker service and companion.

For a source install, or to repair the Linux launcher, run `memql menu --install`.
`memql menu --start` restarts the installed companion in the current desktop
session. `memql menu --uninstall` removes only the companion; the normal Linux
uninstaller removes it when removing the last enrollment or all homes. The
launcher and service honor `XDG_CONFIG_HOME` and `XDG_DATA_HOME`. The desktop
must provide a tray host to display the icon. macOS keeps its existing native
[menu bar companion](docs/macos-menu.md).

The [installer design guide](scripts/install/README.md) describes the terminal
conventions and includes a safe local preview command.

### Uninstall

Uninstall uses the same terminal presentation as install. It distinguishes
disconnecting one cluster while Cockpit stays for other enrollments from
removing the shared runtime. Add `--verbose` for per-file diagnostics and the
full removed/kept ledger, or `--plain` to disable artwork and animation. The
summary names a private uninstall log; `--dry-run` uses temporary diagnostic
storage and leaves the installation unchanged.

One line, like the install. It stops and removes the service, removes the
binary and its symlink, and removes `~/.memql/workers.yaml` and the legacy
`worker.yaml` — the files that hold the tokens:

```bash
# Linux
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/scripts/install/uninstall-linux.sh | bash -s -- [--cluster=URL|--all-homes] [--purge] [--user-local] [--dry-run]

# macOS
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/scripts/install/uninstall-mac.sh | bash -s -- [--cluster=URL|--all-homes] [--purge] [--user-local] [--dry-run]
```

Every flag is optional; MemQL OS composes the line with `--cluster=<url>`
and the person copying it need know nothing else.

- **Which enrollment.** With neither `--cluster` nor `--all-homes` the script
  decides from the machine: one enrolled cluster is removed as if
  `--cluster=<its URL>` had been given; no enrollment (never paired, or
  already unpaired) removes the worker runtime as `--all-homes` would;
  several enrollments are refused (exit 2) with the exact command to run
  for each, so you copy rather than guess. `--cluster=URL` removes that one
  enrollment through `memql worker unpair --cluster-url` and keeps the
  shared runtime while another enrollment remains; the last one takes the
  runtime with it. A URL that matches no enrollment is refused (exit 3)
  with the enrolled ones listed — a wrong URL never removes someone else's
  enrollment. URLs match the way the worker matches them: whitespace and a
  trailing slash ignored, case-insensitive host, so
  `https://api.memql.localhost/` and `https://api.memql.localhost` are one
  enrollment. The value must be the cluster's http(s) URL (no userinfo,
  query or fragment); anything else is a bad parameter (exit 2) before
  anything runs. `--all-homes` removes every enrollment and the runtime. An
  installed `memql` older than 0.15.0 cannot split enrollments: when the
  requested cluster is the only one the run proceeds as `--all-homes` and
  says so; when others exist it refuses (exit 4) and names the upgrade.
- **Which install shape.** The script detects what is on the machine: the
  per-user install (`~/.memql/bin`, and `~/Applications/MemQL.app` on
  macOS), the system install (`/usr/local/bin`, `/Applications/MemQL.app`;
  needs sudo, which is asked for only when one is present, after the
  script has said so), or both after a mode switch. `--user-local`
  restricts the run to the per-user shape and never asks for sudo.
- **`--dry-run`** prints the whole plan — the matched enrollment, the shapes
  found, every path and service that would go, whether sudo would be
  needed — and changes nothing: exit 0, or the same refusal (same code,
  same message) the real run would give before touching anything. The
  commands a refusal prints keep `--dry-run`, so a copied one is still a
  dry run.
- **Partial and older installs** are fine: a missing plist or unit, a
  missing binary, a legacy service label, an enrollment file from the
  older single-home layout (`worker.yaml`) — each step reports "removed"
  or "nothing to do", none aborts the rest, and the closing summary says
  what was removed, what was kept and why. That includes a scoped removal
  whose legacy service file will not delete: the enrollment is gone, the
  worker for the others is back up, and the file is under Kept with the
  remedy (PARTIAL, exit 5). Its heading is SUCCESS,
  PARTIAL (something removed, something left), FAILED (a step ran and
  failed; Kept names what it left, such as a worker this run stopped) or
  REFUSED (nothing was changed).
- **`--purge` and the state directory.** The purge target is the machine's
  state root as the enrollment files name it (the registry first, the
  legacy mirror's per-home `<root>/homes/<id>` collapsed to `<root>`),
  judged by one fence whatever the spelling: never `~/.memql` itself
  (`~/.memql/` and `~/.memql//` included), never the `credentials`,
  `backups`, `certs` or `certificates` trees in any letter case, never
  outside `~/.memql`, never through a symlinked ancestor, never a
  `state_dir` that names a regular file. A state directory that is itself
  a symlink is removed as a link; what it points at is not touched. A file
  or tree that cannot be removed is reported under Kept and the run still
  finishes (exit 5); it never aborts mid-purge.
- **An enrollment registry the script cannot read.** The scripts read
  `~/.memql/workers.yaml` the way the worker and the installer write it:
  `homes:` at the start of a line, then a block list of `- id: ...` items
  (or `homes: []`), in LF or CRLF lines. Every other spelling the worker
  would still accept -- a flow or JSON document, a quoted, spaced,
  explicit, anchored, tagged or escaped `homes` key, an indented document,
  a byte-order mark or UTF-16, CR-only / NEL / LS / PS line breaks,
  flow-style items, a second document -- is refused (exit 5, "not an
  enrollment registry this script can read; nothing was changed") with
  nothing touched, by the no-flag run, `--cluster` and `--dry-run` alike.
  It is never read as "no enrollment", which would remove every token.
  `--all-homes` is the way past it, because it reads nothing; the
  installer refuses the same files rather than rewrite them. A registry
  with no `homes` key anywhere in it reads as no enrollment.

On macOS, last-home or full removal resets only the installed MemQL apps’
Accessibility and Screen Recording decisions; shared sibling enrollments keep
their permissions. Reset failures are reported, and cached Settings rows may
need manual removal. `--purge` is refused while another enrollment remains. CLI credentials,
cluster settings, certificates and backups are retained even with a purge.

Without `--purge`, `~/.memql/policy.yaml` and the state directory (logs,
ledgers) stay, and the script says so. The machine's registration on the
cluster is not touched from here — revoke it from MemQL OS
(Fleet -> Machines).

Exit codes, both scripts: 0 everything that existed was removed; 2 bad
parameter (or several enrollments and none named); 3 refused (a URL nothing
matches, `--purge` with siblings, an aliased enrollment file, a loaded worker
with no regular service plist to reload from); 4 a prerequisite is missing
(sudo for a system install; an installed `memql` 0.15.0+ for a scoped removal
while other enrollments exist; `lib.sh` unreachable when piped); 5 a step
failed (a service that would not stop or start again, an unreadable registry,
a file that could not be removed). A non-zero exit always names what remains
and what to do about it, and a summary heading of PARTIAL or FAILED means
something was changed -- REFUSED alone means nothing was.

## Commands

```
memql cluster add <domain|url>    Register a cluster (discovery + OAuth login)
memql cluster list | remove       Manage saved clusters
memql login | logout <cluster>    (Re-)authenticate / drop credentials
memql access [<cluster>]          What this cluster says you are: role slug,
                                  name and rank, groups, account scope (--json)
memql creds <subcommand>          Inspect / migrate the credential store
memql worker pair <code>          Redeem a pairing code, write worker.yaml, run
memql worker run                  Run the worker (what the service invokes)
memql worker setup                Computer-use permission pre-flight (TCC / X11)
memql worker setup --inference    Turn this machine into an inference machine:
                                  install a model runtime, pull the models,
                                  write models.allow, signal the worker
memql worker models               What local models this machine offers, or why
                                  it offers none (--pull / --allow change it)
memql worker config | consent     Show config / manage consent
memql lint [path]                 Validate a .memql file or DSL tree
memql setup project [flags]       Stamp a new product workspace from the template
memql --version                   Version + build variant
```

`memql worker setup --non-interactive` reports missing permissions with honest
exit codes and never prompts — for scripted installs. The codes are the
capability-script contract's: 2 bad invocation, 3 refused because a required
confirmation could not be asked for, 4 a prerequisite is absent, 5 something
failed.

One command takes a qualifying machine from bare to serving:

```bash
memql worker setup --inference
```

It checks the hardware floor, installs the runtime this platform can serve
from (macOS: Ollama natively; Linux: the container with the GPU passed
through) after printing the exact commands and asking, pulls `llama3.1:8b`
and `nomic-embed-text` with byte counts on screen, writes `models.allow`, and
signals the running worker. Nothing in it runs `sudo` — where a fix needs
root, the command is printed for you. Details:
[docs/local-models.md](docs/local-models.md).

## Build

From the MemQL repository root:

```bash
cd cockpit
make cockpit             # headless -> bin/memql
make cockpit-computeruse # computer-use variant -> bin/memql-computeruse
                         #   (CGO; macOS Xcode CLT / Linux libxtst-dev etc.)
make test                # Cockpit module tests; root make test covers the engine
make dist                # versioned tar.gz archives + SHA256SUMS into dist/
```

The engine is consumed from this same monorepo checkout. The root
[`memql-package.yaml`](../memql-package.yaml) declares Cockpit checks and release
builds alongside the engine; there is no sibling checkout or separate engine
pin. Read the module guidance in [CLAUDE.md](CLAUDE.md) before touching `go.mod`.

Cockpit remains a separate installed artifact with independent versions. Its
release tags are `cockpit/vX.Y.Z` in `znasllc-io/memql`; the worker command and
release asset names stay the same. Building a candidate does not install it or
change a cluster’s selected release.

Computer-use setup and permissions: [docs/computer-use.md](docs/computer-use.md).

Running the cluster's CI pipeline steps on this machine (off unless
`policy.yaml` says `pipelines.allow: true`): [docs/pipelines.md](docs/pipelines.md).

## License

[MIT](LICENSE)

## Editor integration

See [Cockpit and the editor extensions](docs/editor-boundaries.md) for ownership,
shared registry transactions, separate credentials, and backup/edit conflicts.

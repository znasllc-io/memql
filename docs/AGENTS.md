# Agent Stack

**Status:** stable · **Applies to:** local developer machines (macOS, Linux)

The agent stack is the set of Claude Code extensions this repo expects on a
developer's machine: a GitHub MCP server, the Superpowers plugin, and the CCPM
skill. It is installed and checked by two scripts:

```bash
make setup-agents     # install what is missing (idempotent)
make verify-agents    # read-only status check; exits 1 if anything is missing
```

Neither script touches application code, cluster state, or the database. They
configure the developer's local Claude Code installation and the repo's
`.gitignore`, nothing else.

---

## What the stack is

| Component | What it is | Where it lands |
|---|---|---|
| **GitHub MCP server** | Remote MCP server (HTTP transport) at `api.githubcopilot.com` giving Claude Code first-class GitHub access: issues, pull requests, Actions runs, code search. | Claude Code user config (`claude mcp add -s user`) |
| **Superpowers** | Plugin from Anthropic's `claude-plugins-official` marketplace. Adds a broad skill library. | `~/.claude/plugins/`, user scope |
| **CCPM** | Spec-driven project management skill: PRD to epic to GitHub issues to parallel agents. | Cloned to `~/.claude/agent-stack/ccpm`, symlinked into `~/.claude/skills/ccpm` |

Everything installs at **user scope**, not project scope. The stack follows the
developer across repositories, and nothing it installs is committed here.

### Why CCPM installs differently from Superpowers

Superpowers is a Claude Code **plugin** and installs through the plugin
marketplace system. CCPM is not a plugin: `automazeio/ccpm` ships a bare
`skill/ccpm/` directory with no `.claude-plugin/marketplace.json`, so
`claude plugin marketplace add` cannot install it. Per its README, it installs
by symlinking that directory into a skills path, which this Claude Code version
auto-loads. That is why `ensure_ccpm_skill` clones and links rather than calling
the plugin CLI.

If you were pointed at a CCPM *plugin marketplace* repository, note that the
commonly cited `jeffersonwarrior/ccpm` does not resolve (HTTP 404, as does that
account). `automazeio/ccpm` is the actively maintained project.

---

## Prerequisites

| Tool | Why | Install |
|---|---|---|
| `claude` | The CLI whose config the scripts manage. | https://claude.com/claude-code |
| `gh` | GitHub CLI; CCPM's workflow depends on it. | `brew install gh` / `apt install gh` |
| `git` | Clones the CCPM skill. | `brew install git` / `apt install git` |

`jq` is used when present but is not required; the scripts fall back to a
text-based check so a machine without `jq` still gets correct results.

Setup refuses to proceed if `claude`, `gh`, or `git` is missing, and prints the
install command for each. It does not attempt to install them itself.

---

## Credentials

`GITHUB_PAT` is required **only** to configure the GitHub MCP server. The
Superpowers and CCPM steps do not use it and will complete without it.

Resolution order:

1. `GITHUB_PAT` in the environment.
2. `GITHUB_PAT=` in a `.env` file at the repo root.

```bash
export GITHUB_PAT=<token>              # this shell only
echo 'GITHUB_PAT=<token>' >> .env      # persisted; .env is gitignored
```

Create a token at https://github.com/settings/personal-access-tokens. The `repo`
scope is sufficient for issue, PR, and Actions access.

### Preferred: borrow the token from `gh`

If you are already authenticated with the GitHub CLI, do not mint or store a
second token -- derive it from `gh` at shell start, so the credential lives only
in the OS keychain that `gh` already manages and never sits in plaintext in a
dotfile:

```bash
# ~/.bashrc (or ~/.zshrc)
if command -v gh >/dev/null 2>&1; then
    GITHUB_PAT="$(gh auth token 2>/dev/null)" && export GITHUB_PAT
    [ -n "${GITHUB_PAT:-}" ] || unset GITHUB_PAT
fi
```

`gh auth login` already requests `repo`, which is what the MCP server needs.
Re-authing or rotating `gh` then propagates automatically -- there is no second
copy to remember to update.

### Handling of the token

- Never echoed. Status lines report only its length (`40 chars`), never a
  prefix or suffix.
- Never written to a tracked file. Setup verifies `.env` and `.mcp.json` are
  in `.gitignore` and appends whichever is missing.
- Any command output that could contain it is filtered through a redaction
  step before display, so a token echoed back in an error body cannot reach
  the terminal or a CI log.
- `.env` is **parsed, not sourced**. An `.env` file is arbitrary shell;
  sourcing it would execute whatever it contains and import every unrelated
  variable. Only the `GITHUB_PAT` line is read.

Two limits worth knowing:

- Adding an MCP server passes the token as a command-line argument to `claude`,
  where it is briefly visible to other users on a shared machine via `ps`. This
  is inherent to the CLI's interface, not something the script chooses.
- A `.gitignore` entry does not untrack an already-committed file. If setup
  reports a tracked `.env`, run `git rm --cached .env` and **rotate the token**;
  it is in the repository history.

---

## What setup does

Each step probes current state before acting and is a no-op when already
satisfied, so a second consecutive run changes nothing and reports `SKIP`
throughout. Status lines are colored on a TTY and plain when piped or in CI
(`NO_COLOR` is honored).

| Step | Probe | Action when unsatisfied |
|---|---|---|
| Preflight | `command -v` per tool | Report install instructions, stop |
| Credentials | `GITHUB_PAT` env, then `.env` | Warn; only the MCP step depends on it |
| Repository hygiene | `grep -qxF` per `.gitignore` entry | Append only the missing entry |
| GitHub MCP server | `claude mcp get github` | `claude mcp add`, falling back to `add-json` |
| Marketplace | name present in `claude plugin marketplace list --json` | `claude plugin marketplace add --scope user` |
| Plugin | id present in `claude plugin list --json` | `claude plugin install <id> --scope user` |
| CCPM skill | symlink resolves to the expected target | Clone, then symlink |

Options:

```bash
make setup-agents                  # install what is missing
make setup-agents ARGS=--update    # also refresh already-installed components
```

`--update` is opt-in by design. Refreshing on every invocation would mutate the
tree on a run that is supposed to be a no-op, which contradicts the idempotency
guarantee. Updating is an explicit request.

**Restart Claude Code after a run that installed anything.** Plugins and skills
are loaded at session start.

### Existing files are never overwritten

If a path the stack wants (for example `~/.claude/skills/ccpm`) already exists
and is not the expected symlink, setup moves it to `<path>.bak` (or
`.bak.1`, `.bak.2`, ...) and warns. Nothing under `.claude/` is destroyed.

---

## What verify does

`make verify-agents` runs the same probes and installs nothing. It exits 0 when
every component is present and 1 when any is missing, which makes it usable as a
gate. It is deliberately read-only: a check that repairs what it finds can never
fail, so it would prove nothing.

Because setup and verify import their probes from the same library
(`scripts/lib/agents.sh`), the two cannot drift into disagreeing about what
"installed" means.

---

## Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `claude not found on PATH` | Claude Code not installed, or installed outside `PATH`. | Install from https://claude.com/claude-code, reopen the shell. |
| `gh not found on PATH` | GitHub CLI missing. | `brew install gh` / `apt install gh`, then `gh auth login`. |
| `cannot configure MCP server 'github': GITHUB_PAT is not set` | No token in the environment or `.env`. Everything else still installed. | Export `GITHUB_PAT` or add it to `.env`, re-run `make setup-agents`. |
| `could not add MCP server 'github'` | Token invalid, expired, or lacking scope. Both the flag and JSON forms failed. | Verify the token at https://github.com/settings/personal-access-tokens, reissue with `repo` scope, re-run. |
| MCP server configured but tools do not appear | Claude Code has not reloaded, or the server needs authentication. | Restart Claude Code. Check `claude mcp get github`; re-authenticate with `claude mcp login github`. |
| MCP shows `Failed to connect` | Network or upstream outage; a corporate proxy blocking `api.githubcopilot.com`. | Retry. Confirm the host is reachable. Note `claude mcp list` health-checks every server, so unrelated servers may also report failures. |
| `could not add marketplace from anthropics/claude-plugins-official` | No network access to github.com. | Check connectivity and proxy settings, re-run. |
| `added <source> but no marketplace named '<name>' appeared` | Upstream renamed the marketplace in its manifest. | Run `claude plugin marketplace list --json`, then update `AGENTS_MARKETPLACE_NAMES` in `scripts/lib/agents.sh`. |
| `could not install plugin` | Marketplace present but the plugin name changed or was withdrawn. | `claude plugin list --json --available` to see what the marketplace offers. |
| Plugin installed but its skills are absent | Session predates the install. | Restart Claude Code. |
| `expected skill directory missing` | Upstream changed the CCPM repository layout. | Inspect `~/.claude/agent-stack/ccpm`; update `AGENTS_CCPM_TARGET` in `scripts/lib/agents.sh`. |
| `CCPM symlink is dangling or points elsewhere` | The clone was moved or deleted. | `make setup-agents` relinks it; the stale link is backed up to `.bak`. |
| `<path> exists but is not a symlink` | A real directory occupies the skill path. | `make setup-agents` backs it up to `.bak` before linking; move it yourself first if you want it kept elsewhere. |
| `.env is TRACKED by git` | `.env` was committed before it was ignored. | `git rm --cached .env`, commit, and **rotate the token**. |

---

## Uninstall

```bash
claude mcp remove github -s user
claude plugin uninstall superpowers
rm ~/.claude/skills/ccpm
rm -rf ~/.claude/agent-stack/ccpm
```

Removing the marketplace itself is optional:
`claude plugin marketplace remove claude-plugins-official`. Note it also
serves the other official plugins, so removing it is rarely what you want.

---

## Files

| Path | Role |
|---|---|
| `scripts/setup-agents.sh` | Installs and verifies the stack. |
| `scripts/verify-agents.sh` | Read-only status check. |
| `scripts/lib/agents.sh` | Shared stack definition, probes, and status output. |

To change what the stack contains, edit the definitions block at the top of
`scripts/lib/agents.sh`; both scripts pick the change up.

These are not
[capability scripts](internal/design/capability-script-contract.md) and
deliberately do not source `scripts/lib/capability.sh`: that contract requires a
single JSON envelope on stdout for a DSL action executor, whereas these are
human-facing and emit colored status lines.

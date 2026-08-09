#!/usr/bin/env bash
set -euo pipefail

# Script: setup-agents.sh
# Purpose: Install and verify the agent stack -- the GitHub MCP server, the
#          Superpowers plugin, and the CCPM skill -- on a clean machine.
#
#          Every step probes current state first and is a no-op when already
#          satisfied, so a second consecutive run changes nothing and says so.
#          Probes are shared with verify-agents.sh via scripts/lib/agents.sh.
#
# Usage:   bash scripts/setup-agents.sh [--update] [--help]
#          make setup-agents
#
# Secrets: GITHUB_PAT comes from the environment or a gitignored .env at the
#          repo root. It is never echoed, never written to a tracked file, and
#          any command output that might contain it is piped through scrub().
#
# Exit:    0 all steps satisfied; 1 one or more steps failed.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/agents.sh
source "$REPO_ROOT/scripts/lib/agents.sh"

# --update opts into refreshing already-installed components. It is OFF by
# default because "keep everything current" and "a second run changes nothing"
# are contradictory goals: an unconditional `git pull` on every invocation would
# mutate the tree and defeat the idempotency guarantee. Updating is an explicit
# request, not a side effect of setup.
DO_UPDATE=0

function show_help() {
    cat <<'EOF'
Usage: scripts/setup-agents.sh [options]

Install the agent stack (GitHub MCP server, Superpowers plugin, CCPM skill).
Idempotent: each step is a no-op when already satisfied.

Options:
  --update   Also refresh components that are already installed (git pull the
             CCPM skill, update marketplaces). Off by default so a repeat run
             changes nothing.
  --help     Show this message.

Environment:
  GITHUB_PAT   GitHub personal access token. Read from the environment, or from
               a gitignored .env at the repo root. Required.
EOF
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

function preflight() {
    section "Preflight"
    require_tools claude gh git
}

# load_pat resolves GITHUB_PAT from the environment or a gitignored .env.
#
# Returns non-zero when it cannot, but is NOT a hard gate: only the MCP step
# needs the token, so the marketplace / plugin / skill steps still run and
# succeed without it. Aborting everything on a missing PAT would leave the
# machine less set up than it could be, over a credential three of the four
# steps never touch.
function load_pat() {
    section "Credentials"

    if [ -n "${GITHUB_PAT:-}" ]; then
        status PASS "GITHUB_PAT from environment ($(describe_secret "$GITHUB_PAT"))"
        return 0
    fi

    local envfile="$REPO_ROOT/.env"
    if [ -f "$envfile" ]; then
        # Extract only GITHUB_PAT rather than `source`-ing the file: an .env is
        # arbitrary shell, and sourcing it would execute whatever it contains
        # and import every unrelated variable into this process.
        local line value
        line="$(grep -E '^[[:space:]]*(export[[:space:]]+)?GITHUB_PAT=' "$envfile" | tail -1 || true)"
        if [ -n "$line" ]; then
            value="${line#*=}"
            value="$(printf '%s' "$value" | tr -d '\r')"
            value="${value%\"}"; value="${value#\"}"
            value="${value%\'}"; value="${value#\'}"
            if [ -n "$value" ]; then
                export GITHUB_PAT="$value"
                status PASS "GITHUB_PAT from .env ($(describe_secret "$GITHUB_PAT"))"
                return 0
            fi
        fi
    fi

    # Reported as a WARN here, not a FAIL: whether a missing token actually
    # matters is decided by ensure_mcp_github (it does not, if the server is
    # already configured). Failing here would double-count one problem and
    # mislabel a no-op run as broken.
    status WARN "GITHUB_PAT not set (needed only to configure the MCP server)"
    note "Set it for this shell:  export GITHUB_PAT=<token>"
    note "Or persist it in .env:  echo 'GITHUB_PAT=<token>' >> $REPO_ROOT/.env"
    note ".env is gitignored. Create a token (scope: repo) at:"
    note "  https://github.com/settings/personal-access-tokens"
    return 1
}

function ensure_gitignore() {
    section "Repository hygiene"

    local entry
    for entry in $AGENTS_GITIGNORE_ENTRIES; do
        if probe_gitignore_entry "$REPO_ROOT" "$entry"; then
            status SKIP "$entry already in .gitignore"
            continue
        fi
        # Append only the missing entry; never rewrite the file.
        {
            printf '\n# Agent stack -- local credentials and MCP config (never commit).\n'
            printf '%s\n' "$entry"
        } >>"$REPO_ROOT/.gitignore"
        status PASS "$entry added to .gitignore"
    done

    # A token already committed is not fixed by a .gitignore entry -- git keeps
    # tracking a file once it is in the index. Surface that rather than implying
    # the ignore rule protects it.
    for entry in $AGENTS_GITIGNORE_ENTRIES; do
        if git -C "$REPO_ROOT" ls-files --error-unmatch "$entry" >/dev/null 2>&1; then
            status WARN "$entry is TRACKED by git -- .gitignore does not untrack it"
            note "Untrack it:  git rm --cached $entry"
            note "If it ever held a real token, rotate that token now."
        fi
    done
}

function ensure_mcp_github() {
    section "GitHub MCP server"

    # Probe BEFORE requiring the token: an already-configured server stored its
    # own copy of the credential, so re-running without GITHUB_PAT exported is a
    # legitimate no-op rather than an error.
    if probe_mcp "$AGENTS_MCP_NAME"; then
        status SKIP "MCP server '$AGENTS_MCP_NAME' already configured"
        return 0
    fi

    if [ -z "${GITHUB_PAT:-}" ]; then
        status FAIL "cannot configure MCP server '$AGENTS_MCP_NAME': GITHUB_PAT is not set"
        note "This is the only step that needs the token; the rest completed."
        note "Set it, then re-run:  export GITHUB_PAT=<token> && make setup-agents"
        return 1
    fi

    local out
    # Primary: the documented flag form.
    if out="$(claude mcp add -s user --transport http \
                "$AGENTS_MCP_NAME" "$AGENTS_MCP_URL" \
                --header "Authorization: Bearer $GITHUB_PAT" 2>&1)"; then
        status PASS "MCP server '$AGENTS_MCP_NAME' added (flag form)"
        return 0
    fi
    note "flag form failed: $(printf '%s' "$out" | scrub | head -3 | tr '\n' ' ')"

    # Fallback: some Claude Code versions only accept the JSON form.
    local json
    json="$(printf '{"type":"http","url":"%s","headers":{"Authorization":"Bearer %s"}}' \
        "$AGENTS_MCP_URL" "$GITHUB_PAT")"
    if out="$(claude mcp add-json "$AGENTS_MCP_NAME" "$json" --scope user 2>&1)"; then
        status PASS "MCP server '$AGENTS_MCP_NAME' added (add-json form)"
        return 0
    fi

    status FAIL "could not add MCP server '$AGENTS_MCP_NAME'"
    note "$(printf '%s' "$out" | scrub | head -5 | tr '\n' ' ')"
    note "Check the token is valid and has not expired, then re-run."
    return 1
}

function ensure_marketplaces() {
    section "Plugin marketplaces"

    # bash 3.2: no associative arrays, so walk two parallel lists by index.
    local -a sources names
    # shellcheck disable=SC2206  # deliberate word-splitting of the space-separated lists
    sources=($AGENTS_MARKETPLACE_SOURCES)
    # shellcheck disable=SC2206
    names=($AGENTS_MARKETPLACE_NAMES)

    local i source name out rc=0
    for i in $(seq 0 $((${#sources[@]} - 1))); do
        source="${sources[$i]}"
        name="${names[$i]}"

        if probe_marketplace "$name"; then
            if [ "$DO_UPDATE" -eq 1 ]; then
                if out="$(claude plugin marketplace update "$name" 2>&1)"; then
                    status PASS "marketplace '$name' updated"
                else
                    status WARN "marketplace '$name' update failed (already installed, continuing)"
                    note "$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
                fi
            else
                status SKIP "marketplace '$name' already added"
            fi
            continue
        fi

        if out="$(claude plugin marketplace add "$source" --scope user 2>&1)"; then
            # Trust the probe, not the exit code: confirm the manifest really
            # declared the name we key installs off. A mismatch here would make
            # every later `plugin install <p>@<name>` fail confusingly.
            if probe_marketplace "$name"; then
                status PASS "marketplace '$name' added from $source"
            else
                status FAIL "added $source but no marketplace named '$name' appeared"
                note "The manifest's name may have changed. Inspect with:"
                note "  claude plugin marketplace list --json"
                rc=1
            fi
        else
            status FAIL "could not add marketplace from $source"
            note "$(printf '%s' "$out" | head -3 | tr '\n' ' ')"
            note "Check network access to github.com and re-run."
            rc=1
        fi
    done
    return "$rc"
}

function ensure_plugins() {
    section "Plugins"

    local -a ids
    # shellcheck disable=SC2206
    ids=($AGENTS_PLUGIN_IDS)

    local id out rc=0
    for id in "${ids[@]}"; do
        if probe_plugin "$id"; then
            if [ "$DO_UPDATE" -eq 1 ]; then
                if out="$(claude plugin update "${id%%@*}" 2>&1)"; then
                    status PASS "plugin '$id' updated (restart Claude Code to apply)"
                else
                    status WARN "plugin '$id' update failed (already installed, continuing)"
                fi
            else
                status SKIP "plugin '$id' already installed"
            fi
            continue
        fi

        if out="$(claude plugin install "$id" --scope user 2>&1)"; then
            status PASS "plugin '$id' installed"
        else
            status FAIL "could not install plugin '$id'"
            note "$(printf '%s' "$out" | head -3 | tr '\n' ' ')"
            rc=1
        fi
    done
    return "$rc"
}

# backup_path moves an existing path aside to <path>.bak (or .bak.N when that
# is taken) so nothing under .claude/ is ever silently destroyed.
function backup_path() {
    local path="$1" bak="$1.bak" n=1
    while [ -e "$bak" ] || [ -L "$bak" ]; do
        bak="$path.bak.$n"
        n=$((n + 1))
    done
    mv "$path" "$bak"
    status WARN "existing $path backed up to $bak"
}

function ensure_ccpm_skill() {
    section "CCPM skill"

    # CCPM is an Agent Skill, not a plugin: automazeio/ccpm ships skill/ccpm/
    # with no marketplace manifest, so `claude plugin marketplace add` cannot
    # install it. Clone + symlink into ~/.claude/skills/ is the documented path,
    # and this Claude Code version auto-loads that directory.
    if probe_ccpm && [ "$DO_UPDATE" -eq 0 ]; then
        status SKIP "CCPM skill already linked"
        return 0
    fi

    local out
    if [ -d "$AGENTS_CCPM_SRC/.git" ]; then
        if [ "$DO_UPDATE" -eq 1 ]; then
            if out="$(git -C "$AGENTS_CCPM_SRC" pull --ff-only 2>&1)"; then
                status PASS "CCPM source updated"
            else
                status WARN "CCPM source update failed (using existing checkout)"
                note "$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
            fi
        else
            status SKIP "CCPM source already cloned"
        fi
    else
        mkdir -p "$(dirname "$AGENTS_CCPM_SRC")"
        # GIT_TERMINAL_PROMPT=0: the clone's output is captured, so a credential
        # prompt would be invisible and hang the script forever. Fail fast into
        # the error path below instead.
        if out="$(GIT_TERMINAL_PROMPT=0 git clone --depth 1 "$AGENTS_CCPM_REPO" "$AGENTS_CCPM_SRC" 2>&1)"; then
            status PASS "CCPM source cloned to $AGENTS_CCPM_SRC"
        else
            status FAIL "could not clone $AGENTS_CCPM_REPO"
            note "$(printf '%s' "$out" | head -3 | tr '\n' ' ')"
            return 1
        fi
    fi

    if [ ! -d "$AGENTS_CCPM_TARGET" ]; then
        status FAIL "expected skill directory missing: $AGENTS_CCPM_TARGET"
        note "The upstream layout may have changed. Inspect $AGENTS_CCPM_SRC."
        return 1
    fi

    if probe_ccpm; then
        status SKIP "CCPM skill already linked"
        return 0
    fi

    # Never clobber: a real file or directory here is someone else's work.
    # A symlink pointing elsewhere is ours to retarget, but back it up anyway so
    # the previous target is recoverable.
    mkdir -p "$(dirname "$AGENTS_CCPM_LINK")"
    if [ -e "$AGENTS_CCPM_LINK" ] || [ -L "$AGENTS_CCPM_LINK" ]; then
        backup_path "$AGENTS_CCPM_LINK"
    fi

    ln -s "$AGENTS_CCPM_TARGET" "$AGENTS_CCPM_LINK"
    status PASS "CCPM skill linked at $AGENTS_CCPM_LINK"
}

# ---------------------------------------------------------------------------

function main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --update) DO_UPDATE=1; shift ;;
            --help|-h) show_help; exit 0 ;;
            *) printf 'ERROR: unknown option: %s\n' "$1" >&2; show_help >&2; exit 2 ;;
        esac
    done

    printf '%sAgent stack setup%s\n' "$AGENTS_C_BOLD" "$AGENTS_C_OFF"

    # Preflight IS a hard gate: without the CLIs every later step fails in a way
    # that reads like a different problem.
    if ! preflight; then
        summary "Setup:" || true
        printf '\nInstall the missing tool(s) above and re-run.\n' >&2
        exit 1
    fi

    # Credentials are not a gate -- see load_pat. A missing token is surfaced by
    # the one step that needs it.
    load_pat || true

    # The remaining steps are independent -- run them all so one failure does
    # not hide the state of the rest, then report once at the end.
    local rc=0
    ensure_gitignore   || rc=1
    ensure_mcp_github  || rc=1
    ensure_marketplaces || rc=1
    ensure_plugins     || rc=1
    ensure_ccpm_skill  || rc=1

    if ! summary "Setup:"; then
        rc=1
    fi

    if [ "$rc" -eq 0 ]; then
        printf '\nAgent stack ready. Restart Claude Code to load new plugins and skills.\n'
        printf 'Verify any time with: make verify-agents\n'
    else
        printf '\nSome steps failed. See the FAIL lines above, fix, and re-run.\n' >&2
        printf 'Details and remediation: docs/AGENTS.md\n' >&2
    fi
    exit "$rc"
}

main "$@"

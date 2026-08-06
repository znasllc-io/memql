#!/usr/bin/env bash
set -euo pipefail

# Script: verify-agents.sh
# Purpose: Read-only status check for the agent stack. Reports what is present
#          and what is missing, and exits 1 if anything required is absent.
#
#          STRICTLY read-only: it probes state and never installs, updates, or
#          writes anything. That is what makes it a trustworthy gate -- a check
#          that repairs what it finds can never fail, so it would prove nothing.
#          Probes are shared with setup-agents.sh via scripts/lib/agents.sh, so
#          the two can never disagree about what "installed" means.
#
# Usage:   bash scripts/verify-agents.sh [--help]
#          make verify-agents
#
# Exit:    0 every component present; 1 one or more missing.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/agents.sh
source "$REPO_ROOT/scripts/lib/agents.sh"

function show_help() {
    cat <<'EOF'
Usage: scripts/verify-agents.sh

Read-only status check for the agent stack (GitHub MCP server, Superpowers
plugin, CCPM skill). Installs nothing. Exits 1 if anything is missing.

Run `make setup-agents` to install what this reports as missing.
EOF
}

function check_tools() {
    section "Preflight"
    require_tools claude gh git
}

function check_credentials() {
    section "Credentials"

    # A WARN, not a FAIL: `claude mcp add` stored its own copy of the token when
    # the server was configured, so the MCP server keeps working whether or not
    # GITHUB_PAT is exported in this shell. It is only needed to run setup.
    if [ -n "${GITHUB_PAT:-}" ]; then
        status PASS "GITHUB_PAT set in environment ($(describe_secret "$GITHUB_PAT"))"
    elif [ -f "$REPO_ROOT/.env" ] \
        && grep -qE '^[[:space:]]*(export[[:space:]]+)?GITHUB_PAT=' "$REPO_ROOT/.env"; then
        status PASS "GITHUB_PAT present in .env"
    else
        status WARN "GITHUB_PAT not set (only needed to run setup-agents)"
    fi
}

function check_gitignore() {
    section "Repository hygiene"

    local entry rc=0
    for entry in $AGENTS_GITIGNORE_ENTRIES; do
        if probe_gitignore_entry "$REPO_ROOT" "$entry"; then
            status PASS "$entry is gitignored"
        else
            status FAIL "$entry is NOT in .gitignore"
            rc=1
        fi
        if git -C "$REPO_ROOT" ls-files --error-unmatch "$entry" >/dev/null 2>&1; then
            status FAIL "$entry is TRACKED by git"
            note "Untrack it:  git rm --cached $entry"
            rc=1
        fi
    done
    return "$rc"
}

function check_mcp() {
    section "GitHub MCP server"

    if probe_mcp "$AGENTS_MCP_NAME"; then
        status PASS "MCP server '$AGENTS_MCP_NAME' configured"
        return 0
    fi
    status FAIL "MCP server '$AGENTS_MCP_NAME' not configured"
    note "Fix: make setup-agents"
    return 1
}

function check_marketplaces() {
    section "Plugin marketplaces"

    local -a names
    # shellcheck disable=SC2206  # deliberate word-splitting of the space-separated list
    names=($AGENTS_MARKETPLACE_NAMES)

    local name rc=0
    for name in "${names[@]}"; do
        if probe_marketplace "$name"; then
            status PASS "marketplace '$name' added"
        else
            status FAIL "marketplace '$name' missing"
            rc=1
        fi
    done
    [ "$rc" -eq 0 ] || note "Fix: make setup-agents"
    return "$rc"
}

function check_plugins() {
    section "Plugins"

    local -a ids
    # shellcheck disable=SC2206
    ids=($AGENTS_PLUGIN_IDS)

    local id rc=0
    for id in "${ids[@]}"; do
        if probe_plugin "$id"; then
            status PASS "plugin '$id' installed"
        else
            status FAIL "plugin '$id' not installed"
            rc=1
        fi
    done
    [ "$rc" -eq 0 ] || note "Fix: make setup-agents"
    return "$rc"
}

function check_ccpm() {
    section "CCPM skill"

    if probe_ccpm; then
        status PASS "CCPM skill linked at $AGENTS_CCPM_LINK"
        return 0
    fi

    # Distinguish the failure modes -- "never installed" and "installed but the
    # link broke" need different fixes, and a bare "missing" would hide that.
    if [ -L "$AGENTS_CCPM_LINK" ]; then
        status FAIL "CCPM symlink is dangling or points elsewhere"
        note "Points to: $(readlink "$AGENTS_CCPM_LINK")"
        note "Expected:  $AGENTS_CCPM_TARGET"
    elif [ -e "$AGENTS_CCPM_LINK" ]; then
        status FAIL "$AGENTS_CCPM_LINK exists but is not a symlink"
        note "setup-agents will back it up to .bak before replacing it."
    else
        status FAIL "CCPM skill not installed"
    fi
    note "Fix: make setup-agents"
    return 1
}

function main() {
    case "${1:-}" in
        --help|-h) show_help; exit 0 ;;
        "") ;;
        *) printf 'ERROR: unknown option: %s\n' "$1" >&2; show_help >&2; exit 2 ;;
    esac

    printf '%sAgent stack status%s\n' "$AGENTS_C_BOLD" "$AGENTS_C_OFF"

    # Without the CLIs there is nothing meaningful to probe -- every later check
    # would report "missing" for the wrong reason.
    if ! check_tools; then
        summary "Verify:" || true
        printf '\nInstall the missing tool(s) above, then run: make setup-agents\n' >&2
        exit 1
    fi

    local rc=0
    check_credentials
    check_gitignore    || rc=1
    check_mcp          || rc=1
    check_marketplaces || rc=1
    check_plugins      || rc=1
    check_ccpm         || rc=1

    if ! summary "Verify:"; then
        rc=1
    fi

    if [ "$rc" -eq 0 ]; then
        printf '\nAgent stack is complete.\n'
    else
        printf '\nAgent stack is incomplete. Run: make setup-agents\n' >&2
        printf 'Details and remediation: docs/AGENTS.md\n' >&2
    fi
    exit "$rc"
}

main "$@"

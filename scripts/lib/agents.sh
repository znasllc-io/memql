#!/usr/bin/env bash
# Library: agents.sh
# Purpose: Shared definitions, state probes, and status output for the
#          agent-stack scripts (setup-agents.sh, verify-agents.sh).
#
#          Both scripts ask the SAME question -- "is this component already
#          installed?" -- so both read it from ONE probe here. Duplicating the
#          probes would let `make setup-agents` and `make verify-agents` drift
#          into disagreeing about what "installed" means, which is exactly the
#          failure `make help` avoids by deriving render and check from a single
#          parser (scripts/make/help.sh).
#
#          This is NOT a capability script (docs/internal/design/
#          capability-script-contract.md): those emit one JSON envelope on
#          stdout for a DSL action executor, whereas these are human-facing and
#          emit colored status lines. It deliberately does not source the
#          capability library, and so is not bound by that contract.
#
#          NOTE: capability_contract_test.go opts a script into the contract by
#          substring-matching the library's path anywhere in the file, comments
#          included -- so spelling that path here, even to say we do NOT use it,
#          would misclassify this library and fail the suite. Hence the
#          roundabout phrasing above.
#
# Usage:   sourced, never executed. The caller owns `set -euo pipefail`.
#
# Portability: bash 3.2 (stock macOS) -- no associative arrays, no `mapfile`,
#              no `${var^^}`. Parallel indexed arrays only.

# ---------------------------------------------------------------------------
# The stack
# ---------------------------------------------------------------------------

# ShellCheck analyses each file in isolation, so it cannot see that these are
# read by the scripts that source this library and flags every one as unused
# (SC2034). The alternative -- `export`-ing them -- would leak the whole stack
# definition into the environment of every child process (including `claude`)
# to buy nothing. Scoped to this definitions-only file; the functions below are
# still checked normally.
# shellcheck disable=SC2034

# GitHub MCP server (remote, HTTP transport).
AGENTS_MCP_NAME="github"
AGENTS_MCP_URL="https://api.githubcopilot.com/mcp/"

# Plugin marketplaces: SOURCE is what `marketplace add` takes, NAME is what the
# marketplace declares in its manifest and what `marketplace list` reports back.
# They are NOT interchangeable -- the source is a repo slug, the name comes from
# the manifest's "name" field -- so the probe must match on NAME.
AGENTS_MARKETPLACE_SOURCES="obra/superpowers-marketplace"
AGENTS_MARKETPLACE_NAMES="superpowers-marketplace"

# Plugins, as plugin@marketplace ids (the form `plugin list --json` reports).
AGENTS_PLUGIN_IDS="superpowers@superpowers-marketplace"

# CCPM ships as a plain Agent Skill (agentskills.io), NOT a Claude Code plugin
# marketplace -- automazeio/ccpm has no .claude-plugin/marketplace.json, just a
# skill/ccpm/ directory its README says to symlink. So it installs by clone +
# symlink into ~/.claude/skills/, which this Claude Code version auto-loads.
AGENTS_CCPM_REPO="https://github.com/automazeio/ccpm.git"
AGENTS_CCPM_SRC="$HOME/.claude/agent-stack/ccpm"
AGENTS_CCPM_LINK="$HOME/.claude/skills/ccpm"
AGENTS_CCPM_TARGET="$AGENTS_CCPM_SRC/skill/ccpm"

# Files that must never reach a commit.
AGENTS_GITIGNORE_ENTRIES=".env .mcp.json"

# ---------------------------------------------------------------------------
# Status output
# ---------------------------------------------------------------------------

# Colors only on a TTY, and honour NO_COLOR -- piped or CI output stays clean
# instead of carrying escape sequences into a log.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    AGENTS_C_PASS=$'\033[0;32m'
    AGENTS_C_SKIP=$'\033[0;36m'
    AGENTS_C_FAIL=$'\033[0;31m'
    AGENTS_C_WARN=$'\033[0;33m'
    AGENTS_C_BOLD=$'\033[1m'
    AGENTS_C_OFF=$'\033[0m'
else
    AGENTS_C_PASS=""
    AGENTS_C_SKIP=""
    AGENTS_C_FAIL=""
    AGENTS_C_WARN=""
    AGENTS_C_BOLD=""
    AGENTS_C_OFF=""
fi

AGENTS_N_PASS=0
AGENTS_N_SKIP=0
AGENTS_N_FAIL=0
AGENTS_N_WARN=0

# status prints one aligned status line and tallies it.
#   status PASS|SKIP|FAIL|WARN "<message>"
# FAIL and WARN go to stderr so a caller can filter the noise from the report.
function status() {
    local level="$1" msg="$2" color="" stream=1
    case "$level" in
        PASS) color="$AGENTS_C_PASS"; AGENTS_N_PASS=$((AGENTS_N_PASS + 1)) ;;
        SKIP) color="$AGENTS_C_SKIP"; AGENTS_N_SKIP=$((AGENTS_N_SKIP + 1)) ;;
        FAIL) color="$AGENTS_C_FAIL"; AGENTS_N_FAIL=$((AGENTS_N_FAIL + 1)); stream=2 ;;
        WARN) color="$AGENTS_C_WARN"; AGENTS_N_WARN=$((AGENTS_N_WARN + 1)); stream=2 ;;
        *)    color="" ;;
    esac
    if [ "$stream" -eq 2 ]; then
        printf '  %s%-4s%s %s\n' "$color" "$level" "$AGENTS_C_OFF" "$msg" >&2
    else
        printf '  %s%-4s%s %s\n' "$color" "$level" "$AGENTS_C_OFF" "$msg"
    fi
}

# section prints a heading above a group of steps.
function section() {
    printf '\n%s%s%s\n' "$AGENTS_C_BOLD" "$1" "$AGENTS_C_OFF"
}

# note prints an unlabelled, indented continuation line (remediation hints).
function note() {
    printf '       %s\n' "$1" >&2
}

# summary prints the tally. Returns non-zero when anything failed, so a caller
# can just `summary || exit 1`.
function summary() {
    printf '\n%s%s%s  ' "$AGENTS_C_BOLD" "${1:-Summary}" "$AGENTS_C_OFF"
    printf '%spass %d%s  %sskip %d%s  %swarn %d%s  %sfail %d%s\n' \
        "$AGENTS_C_PASS" "$AGENTS_N_PASS" "$AGENTS_C_OFF" \
        "$AGENTS_C_SKIP" "$AGENTS_N_SKIP" "$AGENTS_C_OFF" \
        "$AGENTS_C_WARN" "$AGENTS_N_WARN" "$AGENTS_C_OFF" \
        "$AGENTS_C_FAIL" "$AGENTS_N_FAIL" "$AGENTS_C_OFF"
    [ "$AGENTS_N_FAIL" -eq 0 ]
}

# ---------------------------------------------------------------------------
# Secret hygiene
# ---------------------------------------------------------------------------

# scrub filters the PAT out of a stream. Any command whose output we surface
# gets piped through this, so a token echoed back in an error message (a 401
# body, a usage string) can never reach the terminal or a CI log.
function scrub() {
    if [ -n "${GITHUB_PAT:-}" ]; then
        sed "s|${GITHUB_PAT}|***REDACTED***|g"
    else
        cat
    fi
}

# describe_secret reports a secret's shape without revealing any of it. Even a
# prefix is withheld: `github_pat_` vs `ghp_` is the only thing a prefix would
# tell you, and it is not worth the leak surface.
function describe_secret() {
    local value="${1:-}"
    if [ -z "$value" ]; then
        printf 'unset'
    else
        printf '%d chars' "${#value}"
    fi
}

# ---------------------------------------------------------------------------
# JSON helpers
# ---------------------------------------------------------------------------

# json_has_value answers "does any array element have <field> == <want>?".
# Uses jq when available and falls back to a whitespace-stripped grep, so a
# clean macOS box without jq still gets correct probes.
function json_has_value() {
    local json="$1" field="$2" want="$3"
    if command -v jq >/dev/null 2>&1; then
        printf '%s' "$json" \
            | jq -e --arg f "$field" --arg w "$want" \
                'map(select(.[$f]? == $w)) | length > 0' >/dev/null 2>&1
    else
        printf '%s' "$json" | tr -d ' \n\t' | grep -qF "\"$field\":\"$want\""
    fi
}

# ---------------------------------------------------------------------------
# State probes -- READ-ONLY. Never mutate. 0 = present, 1 = absent.
# ---------------------------------------------------------------------------

# probe_mcp tests a single MCP server by name.
#
# Deliberately `mcp get` and not `mcp list`: list health-checks EVERY configured
# server over the network, so it is slow and its exit status reflects unrelated
# servers. `mcp get <name>` exits 0 when the server is configured and 1 when it
# is not, which is precisely the question.
function probe_mcp() {
    claude mcp get "$1" >/dev/null 2>&1
}

function probe_marketplace() {
    local name="$1" json
    json="$(claude plugin marketplace list --json 2>/dev/null)" || return 1
    json_has_value "$json" "name" "$name"
}

function probe_plugin() {
    local id="$1" json
    json="$(claude plugin list --json 2>/dev/null)" || return 1
    json_has_value "$json" "id" "$id"
}

# probe_ccpm is satisfied only when the symlink resolves to the expected target
# AND that target exists -- a dangling link means the skill will not load, so
# reporting it as installed would be a lie.
function probe_ccpm() {
    [ -L "$AGENTS_CCPM_LINK" ] || return 1
    [ "$(readlink "$AGENTS_CCPM_LINK")" = "$AGENTS_CCPM_TARGET" ] || return 1
    [ -d "$AGENTS_CCPM_TARGET" ]
}

function probe_gitignore_entry() {
    local root="$1" entry="$2"
    [ -f "$root/.gitignore" ] || return 1
    grep -qxF "$entry" "$root/.gitignore"
}

# ---------------------------------------------------------------------------
# Shared preflight
# ---------------------------------------------------------------------------

# require_tools checks that each named tool is on PATH, printing install
# guidance for any that are missing. Returns non-zero if any is absent.
function require_tools() {
    local tool missing=0
    for tool in "$@"; do
        if command -v "$tool" >/dev/null 2>&1; then
            status PASS "$tool found ($(tool_version "$tool"))"
        else
            status FAIL "$tool not found on PATH"
            note "$(install_hint "$tool")"
            missing=1
        fi
    done
    return "$missing"
}

# tool_version returns a one-line version string, or "version unknown" when the
# tool answers in an unexpected shape. Never fails the caller.
function tool_version() {
    local tool="$1" out=""
    case "$tool" in
        claude) out="$(claude --version 2>/dev/null | head -1)" ;;
        gh)     out="$(gh --version 2>/dev/null | head -1)" ;;
        git)    out="$(git --version 2>/dev/null | head -1)" ;;
        *)      out="$("$tool" --version 2>/dev/null | head -1)" ;;
    esac
    if [ -n "$out" ]; then printf '%s' "$out"; else printf 'version unknown'; fi
}

function install_hint() {
    case "$1" in
        claude) printf 'Install: https://claude.com/claude-code  (npm i -g @anthropic-ai/claude-code)' ;;
        gh)     printf 'Install: https://cli.github.com  (brew install gh | apt install gh)' ;;
        git)    printf 'Install: https://git-scm.com  (brew install git | apt install git)' ;;
        *)      printf 'Install %s and re-run.' "$1" ;;
    esac
}

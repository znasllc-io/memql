#!/usr/bin/env bash
#
# anthropic-bench.sh
#
# Measure how fast the Anthropic API responds with the configured key
# under conditions that approximate what the agent node sees during a
# takeover turn. Reads MEMQL_SI_ANTHROPIC_API_KEY from .env.local
# (no other side effects -- the key is never echoed).
#
# Three scenarios:
#   1. tiny      -- 11 tokens in / ~5 tokens out, no tools         (baseline)
#   2. medium    -- realistic system prompt + RAG-sized context    (typical 1st turn)
#   3. heavy     -- big system prompt + tools + thinking           (operator turn)
#
# Each scenario runs N iterations and reports per-call latency
# (curl-side total + first-byte if streaming) so we can see whether the
# API is genuinely slow vs. a particular prompt shape is the bottleneck.
#
# Usage:
#   ./scripts/anthropic-bench.sh             # 3 iterations of each
#   ./scripts/anthropic-bench.sh --iterations 5
#   ./scripts/anthropic-bench.sh --scenario heavy
#   ./scripts/anthropic-bench.sh --model claude-haiku-4-5-20251001

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_MODEL="claude-sonnet-4-5-20250929"
DEFAULT_ITERATIONS=3
ENV_FILE="$(cd "$(dirname "$0")/.." && pwd)/.env.local"
SCENARIO="all"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Measure Anthropic API response speed for the key in .env.local under
agent-realistic prompt shapes.

Options:
    --iterations N    Iterations per scenario (default: $DEFAULT_ITERATIONS)
    --model NAME      Anthropic model (default: $DEFAULT_MODEL)
    --scenario NAME   tiny | medium | heavy | all (default: all)
    --thinking        Enable thinking mode on heavy scenario
    --help            Show this help

Output: per-call total seconds + bytes. End-of-run summary with
min/median/max latency per scenario.
EOF
}

function parse_arguments() {
    MODEL="$DEFAULT_MODEL"
    ITERATIONS="$DEFAULT_ITERATIONS"
    THINKING="false"

    while [[ $# -gt 0 ]]; do
        case $1 in
            --iterations) ITERATIONS="$2"; shift 2 ;;
            --model)      MODEL="$2"; shift 2 ;;
            --scenario)   SCENARIO="$2"; shift 2 ;;
            --thinking)   THINKING="true"; shift ;;
            --help)       show_help; exit 0 ;;
            *)            echo "ERROR: unknown option $1"; show_help; exit 1 ;;
        esac
    done
}

function load_key() {
    if [[ ! -f "$ENV_FILE" ]]; then
        echo "ERROR: $ENV_FILE not found"
        exit 1
    fi
    KEY=$(grep "^MEMQL_SI_ANTHROPIC_API_KEY=" "$ENV_FILE" | head -1 | cut -d"'" -f2)
    if [[ -z "$KEY" ]]; then
        # tolerate unquoted form
        KEY=$(grep "^MEMQL_SI_ANTHROPIC_API_KEY=" "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '"' | tr -d "'")
    fi
    if [[ -z "$KEY" ]]; then
        echo "ERROR: MEMQL_SI_ANTHROPIC_API_KEY not found in $ENV_FILE"
        exit 1
    fi
    echo "INFO: key loaded (suffix ...${KEY: -10}, len ${#KEY})"
}

function check_prerequisites() {
    if ! command -v curl &> /dev/null; then
        echo "ERROR: curl is required"
        exit 1
    fi
    if ! command -v jq &> /dev/null; then
        echo "WARNING: jq not installed -- token-count parsing will be coarse"
    fi
}

# Build a request body for a given scenario.
# Stdout: JSON request body.
function build_body() {
    local scenario="$1"
    local thinking_enabled="$2"

    local sys_prompt=""
    local user_msg=""
    local tools_field=""
    local thinking_field=""
    local max_tokens=200

    case "$scenario" in
        tiny)
            user_msg="reply with just OK"
            sys_prompt="You are a terse assistant."
            ;;
        medium)
            user_msg="What is the safest way to add a new column to a 50M-row Postgres table without locking writes?"
            sys_prompt="You are a senior database engineer. Be concise but complete: explain the concurrent-friendly migration pattern (ADD COLUMN with default null, backfill in batches, then SET NOT NULL via NOT VALID + VALIDATE CONSTRAINT). Mention pg_repack only if relevant. Cite PostgreSQL version-specific behaviour where it matters."
            max_tokens=400
            ;;
        heavy)
            # Approximates the agent node's prompt shape: long system
            # prompt + injected RAG context + tools + a UI walkthrough goal.
            sys_prompt=$(cat <<'SP'
You are an AI assistant driving the CoPresent app on the users behalf
via a takeover ("Control Session"). You have UI primitives:
uiRequestControl, uiClick, uiType, uiSelect, uiHighlight, uiNarrate,
uiAskUser, uiPointerTo, uiReadState, uiWaitFor, uiRetry, uiReleaseControl.

Walkthrough mode hard rules (violating these = failure):
1. NARRATE every meaningful step via uiNarrate. One sentence, present
   tense, before each cursor move.
2. PAUSE the cursor with uiPointerTo before uiClick on each meaningful target.
3. ASK before filling any required field. Use uiAskUser({ question,
   options, allowFreeForm: true }) for: Name, Role, Gender, Personality
   style. Do NOT derive values from the users original request text.
4. NAME is not ROLE. The NAME field (createAgent.name) is a single-word
   display name. The ROLE field (createAgent.role) is what "IT Support"
   refers to.
5. ROLE uses uiSelect, not uiClick.
6. BUDGET your iterations. You have 40 iterations per turn.
7. ALWAYS end with uiReleaseControl.
8. NEVER END YOUR TURN MID-FORM.
9. A USER ANSWER TO uiAskUser RESUMES THE WALKTHROUGH; IT DOES NOT END IT.
10. WHEN IN DOUBT, ADVANCE.
11. BRING THE SECTION INTO VIEW BEFORE NARRATING ABOUT IT.
12. CHECK BEFORE YOU ACT. Every toggle chip carries aria-pressed.

OPERATOR MODE: You have the CoPresent Control capability. You drive
the UI DIRECTLY -- start a Control Session yourself via uiRequestControl
and drive the flow with the operator primitives.

Multi-item asks are ONE takeover, not several. "Show me X and Y and Z"
is one uiRequestControl + a chain of uiClick/uiHighlight/uiNarrate
through each target + one uiReleaseControl.

If the users request is unambiguous, drive immediately. If genuinely
ambiguous, open the session with interactivity: "conversational" and
use uiAskUser inside the session.

PRE-FLIGHT REQUIRED-FIELDS CHECK -- MANDATORY FOR CREATE / EDIT /
INVITE / CONFIGURE FLOWS. Before you call uiRequestControl for ANY of
these flows, you MUST identify the target flow, enumerate the form
required fields via uiDescribe, match against the users words, and
resolve every gap before you start driving the UI.

KNOWLEDGE: The Create Agent modal (route modal:createAgent) is a
two-phase form. Phase 1 (Describe) lets the user describe their agent
in free text or voice; the AI suggests a structured configuration.
Phase 2 (Configure) exposes the fields: Gender, Name, Role, AI
Provider, Personality styles, Knowledge Domains, Tools & Integrations.
A "Configure manually" shortcut skips Phase 1.

KNOWLEDGE: When walking a user through Create Agent, pace the flow
field-by-field. After opening the modal and clicking Configure
manually, the standard cadence is: (1) Gender, (2) Name, (3) Role,
(4) AI Provider, (5) Personality, (6) Knowledge domains are role-
filtered, (7) Tools are also role-scoped, (8) When every required
field is populated, COMMIT CONFIRMATION via uiAskUser then click
createAgent.submit yourself.
SP
)
            user_msg="can you guide me on how to create another agent that can help me with operations"
            max_tokens=1024
            tools_field=$(cat <<'TOOLS'
,"tools":[
  {"name":"uiRequestControl","description":"Begin a Control Session","input_schema":{"type":"object","properties":{"reason":{"type":"string"},"interactivity":{"type":"string","enum":["minimal","conversational"]}},"required":["reason"]}},
  {"name":"uiNarrate","description":"Stream a short narration sentence","input_schema":{"type":"object","properties":{"message":{"type":"string"},"target":{"type":"string"}},"required":["message"]}},
  {"name":"uiClick","description":"Click an element by data-op-id","input_schema":{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}},
  {"name":"uiType","description":"Type into an input by data-op-id","input_schema":{"type":"object","properties":{"target":{"type":"string"},"text":{"type":"string"}},"required":["target","text"]}},
  {"name":"uiSelect","description":"Select an option in a dropdown","input_schema":{"type":"object","properties":{"target":{"type":"string"},"value":{"type":"string"}},"required":["target","value"]}},
  {"name":"uiHighlight","description":"Highlight an element","input_schema":{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}},
  {"name":"uiAskUser","description":"Ask the user a question","input_schema":{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"allowFreeForm":{"type":"boolean"}},"required":["question"]}},
  {"name":"uiPointerTo","description":"Move pointer to an element","input_schema":{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}},
  {"name":"uiReadState","description":"Read the current UI state","input_schema":{"type":"object","properties":{}}},
  {"name":"uiReleaseControl","description":"End the Control Session","input_schema":{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}}
]
TOOLS
)
            ;;
        *)
            echo "ERROR: unknown scenario $scenario" >&2
            exit 1
            ;;
    esac

    if [[ "$thinking_enabled" == "true" ]]; then
        thinking_field=',"thinking":{"type":"enabled","budget_tokens":2048}'
        max_tokens=$((max_tokens + 2048))
    fi

    # Escape the system prompt + user message for JSON
    local sys_escaped
    sys_escaped=$(printf '%s' "$sys_prompt" | jq -Rs .)
    local user_escaped
    user_escaped=$(printf '%s' "$user_msg" | jq -Rs .)

    cat <<JSON
{
  "model": "$MODEL",
  "max_tokens": $max_tokens,
  "system": $sys_escaped,
  "messages": [{"role":"user","content":$user_escaped}]$tools_field$thinking_field
}
JSON
}

# Run one curl iteration. Reports timing + bytes + status code.
# Stdout: "<scenario> <iter> <total_s> <bytes> <http_code>"
function run_one() {
    local scenario="$1"
    local iter="$2"
    local body="$3"
    local body_file
    body_file=$(mktemp)
    printf '%s' "$body" > "$body_file"

    local out_file
    out_file=$(mktemp)

    local result
    # Custom curl format: time_total bytes_downloaded http_code
    result=$(curl -s -o "$out_file" -w "%{time_total} %{size_download} %{http_code}" \
        https://api.anthropic.com/v1/messages \
        -H "x-api-key: $KEY" \
        -H "anthropic-version: 2023-06-01" \
        -H "content-type: application/json" \
        --data @"$body_file")

    local time_total bytes status
    time_total=$(echo "$result" | awk '{print $1}')
    bytes=$(echo "$result" | awk '{print $2}')
    status=$(echo "$result" | awk '{print $3}')

    if [[ "$status" != "200" ]]; then
        echo ""
        echo "  ERROR: HTTP $status"
        head -c 300 "$out_file"
        echo ""
    fi

    # Token usage breakdown if jq available
    local usage_summary=""
    if command -v jq &> /dev/null && [[ "$status" == "200" ]]; then
        local in_tok out_tok stop
        in_tok=$(jq -r '.usage.input_tokens // "?"' < "$out_file")
        out_tok=$(jq -r '.usage.output_tokens // "?"' < "$out_file")
        stop=$(jq -r '.stop_reason // "?"' < "$out_file")
        usage_summary="(in=$in_tok out=$out_tok stop=$stop)"
    fi

    printf '  iter %d: %.2fs  %s bytes  http=%s  %s\n' "$iter" "$time_total" "$bytes" "$status" "$usage_summary"

    rm -f "$body_file" "$out_file"

    # Return the time as a side-effect via an array the parent reads
    LATENCIES+=("$time_total")
}

# Compute median + min + max from the latencies array.
function summarize() {
    local scenario="$1"
    if [[ ${#LATENCIES[@]} -eq 0 ]]; then return; fi

    local sorted
    local tmp
    tmp=$(mktemp)
    for v in "${LATENCIES[@]}"; do echo "$v"; done | sort -n > "$tmp"
    local count=${#LATENCIES[@]}
    local min max median mean
    min=$(head -1 "$tmp")
    max=$(tail -1 "$tmp")
    local mid_index
    mid_index=$(( (count + 1) / 2 ))
    median=$(sed -n "${mid_index}p" "$tmp")
    mean=$(awk "{s+=\$1} END {if (NR>0) printf \"%.2f\", s/NR}" "$tmp")
    rm -f "$tmp"
    echo "  -> $scenario: min=${min}s  median=${median}s  max=${max}s  mean=${mean}s  (n=$count)"
}

function run_scenario() {
    local scenario="$1"
    local label="$2"
    echo ""
    echo "=== $label ==="
    LATENCIES=()
    local body
    body=$(build_body "$scenario" "$THINKING")
    local req_bytes=${#body}
    echo "  request body: $req_bytes bytes"

    for ((i=1; i<=ITERATIONS; i++)); do
        run_one "$scenario" "$i" "$body"
    done
    summarize "$scenario"
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    load_key
    echo "INFO: model=$MODEL  iterations=$ITERATIONS  thinking=$THINKING"

    case "$SCENARIO" in
        tiny)   run_scenario tiny   "TINY (baseline -- ~11 in / ~5 out)" ;;
        medium) run_scenario medium "MEDIUM (1k system prompt, no tools)" ;;
        heavy)  run_scenario heavy  "HEAVY (operator-style prompt + 10 tools)" ;;
        all)
            run_scenario tiny   "TINY (baseline -- ~11 in / ~5 out)"
            run_scenario medium "MEDIUM (1k system prompt, no tools)"
            run_scenario heavy  "HEAVY (operator-style prompt + 10 tools)"
            ;;
        *) echo "ERROR: unknown scenario $SCENARIO"; show_help; exit 1 ;;
    esac

    echo ""
    echo "INFO: done"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"

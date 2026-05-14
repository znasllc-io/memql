#!/usr/bin/env bash
# refactor-prompt-flatten.sh -- drops the input-wrapper block from
# every prompt .memql file so prompt bodies become bare input-schema
# field lists (matching tool / builtin syntax).
#
# Idempotent: files already on the flat form are unchanged.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROMPTS="$ROOT/dsl/v1/prompts"

if [[ ! -d "$PROMPTS" ]]; then
  echo "missing $PROMPTS -- wrong working tree?"
  exit 1
fi

export ROOT
python3 "$(dirname "${BASH_SOURCE[0]}")/refactor_prompt_flatten.py"

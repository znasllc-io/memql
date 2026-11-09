#!/usr/bin/env bash
#
# scripts/lib/bool_flag.sh
# ========================
#
# The boolean reader for scripts that parse their own flags instead of
# sourcing capability.sh: scripts/release/release.sh and
# scripts/release/tag-submodules.sh.
#
# WHY IT EXISTS. A Makefile recipe forwarded each boolean as a BARE flag
# whenever its variable was non-empty, `$${ALLOW_OVERWRITE:+--allow-overwrite}`,
# and these scripts accepted only the bare form. So the script could not tell
# ALLOW_OVERWRITE=0 from ALLOW_OVERWRITE=1, and `make release
# ALLOW_OVERWRITE=0` allowed an overwrite of a write-once release tag. The
# recipe forwards the value now, `--allow-overwrite="$${ALLOW_OVERWRITE}"`,
# and the script reads it here.
#
# ONE VOCABULARY. This is the vocabulary of capability.sh's cap_bool, so a make
# variable means the same thing whichever kind of script it reaches:
# true|1|yes|y|on is true, ""|false|0|no|n|off is false, case-insensitively,
# and anything else is REFUSED rather than guessed. A reader cannot know a
# flag's polarity -- --dry-run is safe when true, --allow-overwrite when false
# -- so there is no default for a typo that fails closed both ways.
# TestBoolFlagAgreesWithCapBool (scripts/lib/bool_flag_test.go) holds the two
# to the same answers. It cannot simply call cap_bool: that reads parsed
# capability params and refuses with a JSON envelope on stdout, which these
# scripts, writing for a human, do not print.
#
# Usage, in a parser:
#
#     --push)   PUSH=true ;;
#     --push=*) PUSH="$(bool_flag --push "${1#*=}")" || exit 1 ;;
#
# bool_flag <flag> <value> prints "true" or "false". For anything else it
# names the flag and the value on stderr and returns 2; the caller exits with
# its own bad-argument code. It is a plain assignment's whole right-hand side,
# so the refusal reaches the `||`.

function bool_flag() {
    local flag="$1" raw="${2-}" lower
    # tr, not ${raw,,}: bash 3.2 has no case conversion and dies on it.
    lower="$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')"
    case "$lower" in
        true|1|yes|y|on)     printf 'true' ;;
        ""|false|0|no|n|off) printf 'false' ;;
        *)
            printf "ERROR: %s takes a boolean (true/false); got '%s'\n" "$flag" "$raw" >&2
            return 2
            ;;
    esac
}

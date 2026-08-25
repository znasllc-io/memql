package lib

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// trailing_conditional_test.go -- the silent-success-reported-as-failure bug
// (epic memql#4490).
//
// THE BUG. In bash, a function whose LAST statement is a conditional AND-list
//
//	[[ -n "$X" ]] && cap_result_set "x" "$X"
//
// returns 1 when the test is FALSE, because that is the status of the list.
// Under `set -euo pipefail` -- which every capability script sets -- the caller
// then aborts on the call, so `cap_ok` is never reached and the EXIT trap emits
//
//	{"ok":false, ..., "error":{"code":1,
//	 "message":"capability '...' aborted (exit 1) without an explicit result"}}
//
// on a run that did everything correctly, with `changed:true` beside it.
//
// IT WAS LIVE IN TWO SCRIPTS. `azure-scale.sh --nodePool=mesh --nodeCount=3`
// with no --replicas -- a documented, legal invocation -- scaled the pool and
// then reported failure. `azure-provision.sh` did the same on any real run
// where `az identity show` for the database identity did not resolve, which the
// script already tolerates with `|| true`. Both were measured, not reasoned
// about.
//
// It belongs in this package because the failure is a property of the
// CONTRACT: it is the interaction between `set -e` and the cap_ok-or-nothing
// result guarantee, and it produces the exact class of failure this epic is
// about -- a system reporting the opposite of what happened, in a way nothing
// in the output explains.
//
// WHY IT IS ONLY THE LAST STATEMENT. A mid-function `[[ ]] && cmd` is fine:
// `set -e` does not act on a command whose status is consumed by a following
// statement. Only the function's own return value is at stake, so the check is
// deliberately narrow rather than a ban on the idiom.
//
// THE REPAIR is an unconditional final statement -- `return 0` -- or writing
// the last conditional as an `if` block, whose status when the condition is
// false is 0.

var trailingConditional = regexp.MustCompile(`^\[\[.*\]\][ \t]*&&[ \t]*\S`)
var bashFuncHeader = regexp.MustCompile(`^function[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(\)[ \t]*\{`)

func TestNoCapabilityFunctionEndsOnAConditional(t *testing.T) {
	scripts := capabilityScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no capability scripts found -- the discovery glob is broken")
	}

	checked := 0
	for _, path := range scripts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel := filepath.Base(path)

		lines := strings.Split(string(body), "\n")
		var fn string
		depth := 0
		lastCode := ""
		lastCodeLine := 0

		for i, raw := range lines {
			trimmed := strings.TrimSpace(raw)

			if depth == 0 {
				if m := bashFuncHeader.FindStringSubmatch(trimmed); m != nil {
					fn, depth, lastCode, lastCodeLine = m[1], 1, "", 0
				}
				continue
			}

			depth += strings.Count(raw, "{") - strings.Count(raw, "}")
			if depth <= 0 {
				checked++
				if trailingConditional.MatchString(lastCode) {
					t.Errorf("%s:%d: function %s() ends on a conditional AND-list:\n"+
						"    %s\n"+
						"When the test is false the list returns 1, so the FUNCTION returns 1, and "+
						"under `set -e` the caller aborts on the call. In a capability script that "+
						"means cap_ok is never reached and the envelope reports "+
						"\"aborted without an explicit result\" on a run that succeeded.\n"+
						"Repair: end the function with an unconditional `return 0`, or write the "+
						"last conditional as an `if` block.",
						rel, lastCodeLine, fn, lastCode)
				}
				fn = ""
				continue
			}

			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				lastCode, lastCodeLine = trimmed, i+1
			}
		}
	}

	// Not a silent pass: a walk that matched no function at all would report a
	// clean bill of health about nothing.
	if checked == 0 {
		t.Fatal("walked every capability script and found no `function name() {` at all -- " +
			"either the house style changed or this scan stopped matching, and either way " +
			"this gate is watching nothing")
	}
	t.Logf("checked %d function(s) across %d capability script(s)", checked, len(scripts))
}

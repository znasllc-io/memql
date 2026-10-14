package lib

import (
	"regexp"
	"strings"
	"testing"
)

var bashFuncHeader = regexp.MustCompile(`^function[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(\)[ \t]*\{`)

type bashCodeLine struct {
	text   string
	number int
}

type bashFunction struct {
	name string
	code []bashCodeLine
}

// bashFunctions collects complete multiline functions in the repository's
// function name() { style. It counts literal braces, as the contract gates
// historically have; it is not a Bash parser. Blank lines and full-line
// comments are excluded from code, whose line numbers are one-based.
func bashFunctions(src string) []bashFunction {
	var functions []bashFunction
	var fn bashFunction
	depth := 0
	for i, raw := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(raw)
		if depth == 0 {
			if m := bashFuncHeader.FindStringSubmatch(trimmed); m != nil {
				fn = bashFunction{name: m[1]}
				// A one-liner closes on its header and must not consume the
				// following functions as its body.
				depth = strings.Count(raw, "{") - strings.Count(raw, "}")
				if depth <= 0 {
					depth = 0
				}
			}
			continue
		}
		depth += strings.Count(raw, "{") - strings.Count(raw, "}")
		if depth <= 0 {
			functions = append(functions, fn)
			depth = 0
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			fn.code = append(fn.code, bashCodeLine{text: trimmed, number: i + 1})
		}
	}
	return functions
}

func TestBashFunctionsFindsTrailingConditionalAfterOneLiner(t *testing.T) {
	const src = `function kc() { kubectl -n "$SRC_NS" "$@"; }
function broken() {
    [[ -n "$X" ]] && x
    # A comment must not hide the final statement.

}
function safe() {
    [[ -n "$X" ]] && x
    return 0
}
`
	functions := bashFunctions(src)
	if len(functions) != 2 {
		t.Fatalf("got %d multiline functions, want 2: %+v", len(functions), functions)
	}
	broken := functions[0]
	if broken.name != "broken" || len(broken.code) != 1 {
		t.Fatalf("unexpected broken function: %+v", broken)
	}
	last := broken.code[len(broken.code)-1]
	if last.number != 3 || !trailingConditional.MatchString(last.text) {
		t.Fatalf("missed trailing conditional at line 3: %+v", last)
	}
	safe := functions[1]
	if safe.name != "safe" || len(safe.code) != 2 || trailingConditional.MatchString(safe.code[len(safe.code)-1].text) {
		t.Fatalf("unexpected safe function: %+v", safe)
	}
}

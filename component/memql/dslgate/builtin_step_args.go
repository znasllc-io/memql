package dslgate

import (
	"fmt"
	"regexp"
	"strings"
)

// GateBuiltinStepArgs is the rule id. Declared here rather than in dslgate.go,
// beside the rule it names -- the convention GateCrossNamespaceImport set.
const GateBuiltinStepArgs Gate = "builtin-step-args"

// A builtin step whose call the builtin's own argument profile refuses
// (memql#4927).
//
// # The defect
//
// `customDomainReconcile` and `packageSweepAbandoned` were declared
// `@args(profile="object")` with EMPTY bodies, and their scheduled automations
// called them `builtin customDomainReconcile ()`. The `object` profile refuses
// an empty argument list, so both steps died at parse -- every two minutes, on
// the cron leader, for as long as they were deployed. Neither the custom-domain
// state machine nor the abandoned-run sweep had ever run on the cluster where
// this was found; the only evidence was a WARN line, 76 of them in three hours.
//
// # Why no test caught it
//
// The declaration is in `builtins.memql` and the call is in
// `automations.memql`, and each is well-formed on its own. Nothing loads the
// pair. The engine resolves the call only when the step EXECUTES, so the shape
// ships green through every suite in the repo and fails on a schedule nobody
// is watching -- which is the same argument `tool_handler_resolution.go` makes
// for resolving a tool's handler target at boot instead of mid-turn.
//
// # Why the verdict is not computed here
//
// Whether a profile accepts a call is `parseMetaCommandArgs`' answer, and a
// second implementation of it in this package would be a second implementation
// free to disagree -- exactly the drift that made the original bug invisible.
// So Options.BuiltinStepRefusal is a PREDICATE the engine supplies, backed by
// the real parser over the real registry entry. This file finds call sites; it
// decides nothing.
//
// nil means the gate does not run. That is the fail-OPEN direction and it is
// deliberate: with no verdict available the only alternative is to re-derive
// the profile from source text, including the inference rule that an
// annotation-less builtin with fields is `object` -- and a gate guessing at
// that would refuse boots over builtins it had merely misread.

// THE STEP CALL FORM. `builtin NAME (` -- the `builtin` kind prefix is what
// makes a step call unambiguous in source. A logic body calls a builtin by
// bare name, which is indistinguishable from any other call at this level, so
// it is deliberately out of scope rather than guessed at.
//
// Whitespace before `(` is accepted because the rewriter accepts it
// (kindPrefix then finishCall), so `builtin foo ()` is the live spelling --
// and is in fact how every scheduled step in the tree is written.
var builtinStepCall = regexp.MustCompile(`(?m)^[ \t]*builtin[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(`)

// scanBuiltinStepArgs is the corpus-level gate: the declaration whose profile
// decides the verdict lives in a different file from the call.
func scanBuiltinStepArgs(files []SourceFile, opts Options) []Violation {
	if opts.BuiltinStepRefusal == nil {
		return nil
	}

	var out []Violation
	for _, f := range files {
		if skipForAutomationScan(f.Path) {
			continue
		}
		// Strings first, then comments -- the same order the sibling gates
		// use, and for the same reason: blanking comments first lets a `//`
		// inside a string literal eat the rest of a real line.
		src := codeOnly(f.Content)
		for _, m := range builtinStepCall.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[2]:m[3]]
			args, ok := callArgs(src, m[1]-1)
			if !ok {
				// An unbalanced call is a parse problem the parser reports
				// better than this gate could.
				continue
			}
			detail, known := opts.BuiltinStepRefusal(name, args)
			if !known || detail == "" {
				continue
			}
			out = append(out, Violation{
				Gate:      GateBuiltinStepArgs,
				File:      f.Path,
				Line:      strings.Count(src[:m[0]], "\n") + 1,
				Kind:      "automation",
				Construct: enclosingAutomation(src, m[0]),
				Detail: fmt.Sprintf(
					"step calls `builtin %s (%s)`, and %s "+
						"Left as it is this loads clean and fails every time the step RUNS, "+
						"which for a scheduled automation is on a timer nobody is watching (memql#4927)",
					name, summarise(args), detail),
			})
		}
	}
	return out
}

// callArgs returns the text between the `(` at open and its matching `)`,
// skipping over string literals so a bracket inside one cannot close the call.
func callArgs(src string, open int) (string, bool) {
	depth := 0
	inStr := false
	var quote byte
	for i := open; i < len(src); i++ {
		c := src[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(src[open+1 : i]), true
			}
		}
	}
	return "", false
}

// summarise keeps a violation one line long when the call spans several.
func summarise(args string) string {
	flat := strings.Join(strings.Fields(args), " ")
	if len(flat) <= 60 {
		return flat
	}
	return flat[:57] + "..."
}

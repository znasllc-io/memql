package bodymigrate

// inline.go -- the parts of the bodies rewrite that cross constructs or
// files (epic memql#5370, task memql#5373; D2, D13, D14, D15).
//
//   - The terse header `automation N @trigger(...) => logic L` expands to the
//     one-statement automation it always compiled to.
//   - A logic may not publish (D14). A publishing logic whose one caller is a
//     terse automation is INLINED into it: the automation's body becomes the
//     logic's statements, the logic's `args.event.payload.f` reads become
//     `args.f` with `f` declared (G5 keeps `event.payload.f` out of automation
//     bodies), and the logic is deleted. Any other publishing logic is left
//     for a hand edit and reported.
//   - `mutate` declarations become `mutation` (D13); `partition=` leaves
//     @trigger and `@schedule(cron=)` becomes `@trigger(schedule=)` (D15).

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// expandTerse writes the one-statement automation a terse header meant.
func expandTerse(c construct, src string) string {
	lineStartOff := lineStart(src, c.start)
	ind := src[lineStartOff:c.start]
	return c.terseTrigger + "\n" + ind + "automation " + c.name + " {\n" +
		ind + "  logic " + c.terseLogic + "(event: event)\n" + ind + "}"
}

// publishes reports whether a body holds a publish statement at any depth.
func publishes(stmts []*lstmt) bool {
	for _, st := range stmts {
		if st.form == formPublish {
			return true
		}
		for _, br := range st.branches {
			if publishes(br.body) {
				return true
			}
		}
		if publishes(st.body) {
			return true
		}
		for _, c := range st.cases {
			if publishes(c.body) {
				return true
			}
		}
		for _, br := range st.par {
			if publishes(br.body) {
				return true
			}
		}
	}
	return false
}

// publishNames returns the names statements gave to a publish.
func publishNames(stmts []*lstmt, into map[string]bool) {
	for _, st := range stmts {
		if st.form == formPublish && st.name != "" {
			into[st.name] = true
		}
		for _, br := range st.branches {
			publishNames(br.body, into)
		}
		publishNames(st.body, into)
		for _, c := range st.cases {
			publishNames(c.body, into)
		}
	}
}

// inlineLogic returns the statements a publishing logic contributes to the
// automation it is inlined into, with its final `return <publish name>`
// dropped (a publish has no value) and its reads of a publish name refused.
func inlineLogic(lb *legacyBody) ([]*lstmt, error) {
	stmts := lb.stmts
	names := map[string]bool{}
	publishNames(stmts, names)
	if n := len(stmts); n > 0 && stmts[n-1].form == formReturn && names[strings.TrimSpace(stmts[n-1].expr)] {
		stmts = stmts[:n-1]
	}
	for _, st := range stmts {
		for _, text := range statementTexts(st) {
			for _, t := range tokenizeExpr(text) {
				if t.kind == 'i' && names[t.text] {
					return nil, fmt.Errorf("logic %s reads %s, the value of a publish, which has none", lb.name, t.text)
				}
			}
		}
	}
	return stmts, nil
}

// logicCallers returns where a logic is reached from in a tree, other than
// the terse automation named skip: `logic L(` / `logic L {` calls, tool
// handlers naming it, and other terse automations.
func logicCallers(files map[string]string, name, skipTerse string) []string {
	call := regexp.MustCompile(`\blogic[ \t]+` + regexp.QuoteMeta(name) + `[ \t]*[({]`)
	handler := regexp.MustCompile(`name[ \t]*=[ \t]*"` + regexp.QuoteMeta(name) + `"`)
	terse := regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+@trigger\([^)]*\)[ \t]*=>[ \t]*logic[ \t]+` + regexp.QuoteMeta(name) + `[ \t]*$`)
	var out []string
	paths := sortedKeys(files)
	for _, p := range paths {
		view := codeView(files[p])
		for _, m := range call.FindAllStringIndex(view, -1) {
			// `logic NAME {` alone on its line is the declaration, not a call.
			if strings.TrimSpace(view[lineStart(view, m[0]):m[0]]) == "" && view[m[1]-1] == '{' {
				continue
			}
			out = append(out, p+": a `logic "+name+"(...)` call")
			break
		}
		if handler.MatchString(files[p]) {
			out = append(out, p+": a handler naming "+name)
		}
		for _, m := range terse.FindAllStringSubmatch(view, -1) {
			if m[1] != skipTerse {
				out = append(out, p+": the terse automation "+m[1])
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// declarationRegion returns the span of a logic declaration together with
// the contiguous doc-comment, comment and annotation lines directly above it,
// and the newline that ends it.
func declarationRegion(src string, c construct) (int, int) {
	start := lineStart(src, c.start)
	for start > 0 {
		prevStart := lineStart(src, start-1)
		line := strings.TrimSpace(src[prevStart : start-1])
		if line == "" || !(strings.HasPrefix(line, "//") || strings.HasPrefix(line, "@")) {
			break
		}
		start = prevStart
	}
	end := c.close + 1
	if end < len(src) && src[end] == '\n' {
		end++
	}
	// Swallow one blank line that separated it from what follows, so the
	// deletion does not leave two.
	if end < len(src) && src[end] == '\n' && start > 0 && src[start-1] == '\n' && start > 1 && src[start-2] == '\n' {
		end++
	}
	return start, end
}

// docLines returns a declaration's `///` doc-comment lines, as `//` lines.
func docLines(src string, c construct) []string {
	var out []string
	start := lineStart(src, c.start)
	for start > 0 {
		prevStart := lineStart(src, start-1)
		line := strings.TrimSpace(src[prevStart : start-1])
		if strings.HasPrefix(line, "@") {
			start = prevStart
			continue
		}
		if !strings.HasPrefix(line, "///") {
			break
		}
		out = append([]string{"// " + strings.TrimSpace(strings.TrimPrefix(line, "///"))}, out...)
		start = prevStart
	}
	return out
}

// ---- use imports -------------------------------------------------------------

var useLine = regexp.MustCompile(`(?m)^use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)\.\{([^}]*)\}[ \t]*$`)

// useImports maps an imported construct name to the path it comes from.
func useImports(src string) map[string]string {
	out := map[string]string{}
	for _, m := range useLine.FindAllStringSubmatch(src, -1) {
		for _, n := range strings.Split(m[2], ",") {
			if n = strings.TrimSpace(n); n != "" {
				out[n] = m[1]
			}
		}
	}
	return out
}

// addImports ensures src imports every name in need, from the paths given,
// extending an existing `use <path>.{ ... }` line or adding one after the
// last use line (or at the top).
func addImports(src string, need map[string]string) string {
	if len(need) == 0 {
		return src
	}
	have := useImports(src)
	byPath := map[string][]string{}
	for name, p := range need {
		if _, ok := have[name]; ok {
			continue
		}
		byPath[p] = append(byPath[p], name)
	}
	if len(byPath) == 0 {
		return src
	}
	for _, p := range sortedKeys(byPath) {
		names := byPath[p]
		sort.Strings(names)
		existing := regexp.MustCompile(`(?m)^use[ \t]+` + regexp.QuoteMeta(p) + `\.\{([^}]*)\}[ \t]*$`)
		if m := existing.FindStringSubmatchIndex(src); m != nil {
			list := strings.Split(src[m[2]:m[3]], ",")
			for i := range list {
				list[i] = strings.TrimSpace(list[i])
			}
			list = append(list, names...)
			sort.Strings(list)
			src = src[:m[2]] + " " + strings.Join(list, ", ") + " " + src[m[3]:]
			continue
		}
		line := "use " + p + ".{ " + strings.Join(names, ", ") + " }\n"
		locs := useLine.FindAllStringIndex(src, -1)
		if len(locs) > 0 {
			at := locs[len(locs)-1][1]
			if at < len(src) && src[at] == '\n' {
				at++
			}
			src = src[:at] + line + src[at:]
		} else {
			at := afterFileHeader(src)
			src = src[:at] + line + "\n" + src[at:]
		}
	}
	return src
}

// pruneImports drops from src's use lines every name that removed -- the text
// of the logic moved out of src -- referenced and nothing left in src does,
// and a use line left with no name. Those are the imports only a moved logic
// needed; kept, the import gate refuses each as never referenced.
func pruneImports(src, removed string) string {
	if removed == "" {
		return src
	}
	body := useLine.ReplaceAllString(codeView(src), "")
	gone := map[string]bool{}
	removedView := codeView(removed)
	for name := range useImports(src) {
		if containsWord(removedView, name) && !containsWord(body, name) {
			gone[name] = true
		}
	}
	if len(gone) == 0 {
		return src
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(src, "\n") {
		m := useLine.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil {
			b.WriteString(line)
			continue
		}
		var keep []string
		dropped := false
		for _, n := range strings.Split(m[2], ",") {
			switch n = strings.TrimSpace(n); {
			case gone[n]:
				dropped = true
			case n != "":
				keep = append(keep, n)
			}
		}
		if !dropped {
			b.WriteString(line)
			continue
		}
		if len(keep) == 0 {
			continue
		}
		b.WriteString("use " + m[1] + ".{ " + strings.Join(keep, ", ") + " }")
		if strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// afterFileHeader returns the offset just past a file's opening block of `//`
// comment lines and the blank lines after it -- where an import belongs. A
// `///` doc comment belongs to the construct below it and ends the header.
func afterFileHeader(src string) int {
	pos := 0
	sawComment := false
	for pos < len(src) {
		eol := strings.IndexByte(src[pos:], '\n')
		end := len(src)
		if eol >= 0 {
			end = pos + eol
		}
		t := strings.TrimSpace(src[pos:end])
		switch {
		case strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "///"):
			sawComment = true
		case t == "" && sawComment:
		default:
			return pos
		}
		if eol < 0 {
			return len(src)
		}
		pos = end + 1
	}
	return pos
}

// ---- declarations and triggers ----------------------------------------------

var (
	mutateDecl  = regexp.MustCompile(`(?m)^([ \t]*)mutate([ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{)`)
	triggerOpen = regexp.MustCompile(`@trigger[ \t]*\(`)
	scheduleSyn = regexp.MustCompile(`@schedule[ \t]*\([ \t]*cron[ \t]*=[ \t]*("(?:[^"\\]|\\.)*")[ \t]*\)`)
)

// rewriteDeclarationsAndTriggers renames `mutate` declarations, drops
// `partition=` from every @trigger and spells `@schedule(cron=)` as
// `@trigger(schedule=)`. Located on the code view; commented text is left alone.
func rewriteDeclarationsAndTriggers(src string) string {
	view := codeView(src)
	// mutate -> mutation, from the end so offsets hold.
	locs := mutateDecl.FindAllStringSubmatchIndex(view, -1)
	for k := len(locs) - 1; k >= 0; k-- {
		m := locs[k]
		kw := m[3] // end of the indentation group: `mutate` starts here
		src = src[:kw] + "mutation" + src[kw+len("mutate"):]
	}
	view = codeView(src)
	// @schedule(cron=X) -> @trigger(schedule=X).
	for _, m := range reverse(scheduleSyn.FindAllStringSubmatchIndex(src, -1)) {
		if strings.TrimSpace(view[m[0]:m[0]+len("@schedule")]) == "" {
			continue // inside a comment or a string
		}
		src = src[:m[0]] + "@trigger(schedule=" + src[m[2]:m[3]] + ")" + src[m[1]:]
	}
	view = codeView(src)
	// partition= out of @trigger.
	for _, m := range reverse(triggerOpen.FindAllStringIndex(view, -1)) {
		open := m[1] - 1
		close := matchClose(view, open)
		if close < 0 {
			continue
		}
		inner := src[open+1 : close]
		fixed := dropPartition(inner)
		if fixed != inner {
			src = src[:open+1] + fixed + src[close:]
		}
	}
	return src
}

// dropPartition removes the partition keyword argument from a @trigger's
// argument list, keeping the separators of the rest.
func dropPartition(args string) string {
	parts := splitTopLevel(args, codeView(args), ',')
	var keep []string
	for _, p := range parts {
		k, _ := leadingIdent(strings.TrimSpace(p))
		if k == "partition" {
			continue
		}
		keep = append(keep, strings.TrimSpace(p))
	}
	if len(keep) == len(parts) {
		return args
	}
	return strings.Join(keep, ", ")
}

func reverse[T any](in []T) []T {
	out := make([]T, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

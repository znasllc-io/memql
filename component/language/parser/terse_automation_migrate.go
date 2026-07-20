package parser

import (
	"regexp"
	"sort"
	"strings"
)

// RewriteLonghandSingleStepAutomation collapses the longhand
// single-step pass-through automation into the terse arrow form
// (#2619; the arrow form itself shipped with memql#2215 and had zero
// corpus adoption):
//
//	@trigger(event="system.startup")        automation registerNode
//	automation registerNode {           =>    @trigger(event="system.startup")
//	  step run {                              => logic registerNode
//	    logic registerNode { event: event }
//	  }
//	}
//
// Eligibility is deliberately narrow -- the body must be EXACTLY one
// `step run { logic Y { event: event } }` with no args block, no other
// statements, and no comments inside the construct (comments are
// preserved by not rewriting); exactly one @trigger annotation must
// precede the declaration (it hoists inline -- the reactive surface
// stays greppable on the declaration line), and every other preceding
// annotation line is kept verbatim above the terse declaration. Each
// rewrite is verified through the engine's own two-stage lowering:
// the terse output must normalise to the IDENTICAL procedural text
// the longhand produced, or the construct passes through untouched.
func RewriteLonghandSingleStepAutomation(src []byte) ([]byte, error) {
	text := string(src)
	matches := longhandSingleStepRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return src, nil
	}
	var b strings.Builder
	prev := 0
	changed := false
	for _, m := range matches {
		whole := text[m[0]:m[1]]
		annBlock := text[m[2]:m[3]]
		ind := text[m[4]:m[5]]
		name := text[m[6]:m[7]]
		logicName := text[m[8]:m[9]]

		trigger, rest, ok := splitTriggerAnnotation(annBlock)
		if !ok || strings.Contains(whole, "//") || strings.Contains(whole, "/*") {
			continue // zero or multiple @trigger lines, or comments: keep longhand
		}

		terse := rest + ind + "automation " + name + " " + trigger + " => logic " + logicName

		// The proof: the terse spelling must lower through the engine's
		// own pipeline to the exact procedural text the longhand yields.
		if !terseEquivalent(whole, terse) {
			continue
		}

		b.WriteString(text[prev:m[0]])
		b.WriteString(terse)
		prev = m[1]
		changed = true
	}
	if !changed {
		return src, nil
	}
	b.WriteString(text[prev:])
	return []byte(b.String()), nil
}

// longhandSingleStepRe matches the full longhand single-step
// pass-through construct: a run of annotation lines (group 1), then
// `automation NAME {` (indent group 2, name group 3) whose body is
// exactly one `step run { logic Y { event: event } }` (logic name
// group 4).
var longhandSingleStepRe = regexp.MustCompile(
	`(?m)^((?:[ \t]*@[A-Za-z][^\n]*\n)+)([ \t]*)automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{[ \t]*\n[ \t]*step[ \t]+run[ \t]*\{[ \t]*\n[ \t]*logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*(?:\{[ \t]*event[ \t]*:[ \t]*event[ \t]*\}|\([ \t]*event[ \t]*:[ \t]*event[ \t]*\))[ \t]*\n[ \t]*\}[ \t]*\n[ \t]*\}`)

// splitTriggerAnnotation separates the single @trigger line from an
// annotation block, returning (trigger, remaining block, ok). ok is
// false when the block carries zero or more than one @trigger.
func splitTriggerAnnotation(block string) (string, string, bool) {
	var trigger string
	var rest strings.Builder
	count := 0
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "@trigger(") {
			count++
			trigger = strings.TrimSpace(line)
			continue
		}
		rest.WriteString(line)
		rest.WriteString("\n")
	}
	if count != 1 {
		return "", "", false
	}
	return trigger, rest.String(), true
}

// terseEquivalent lowers both spellings through the engine's own
// normalisation pipeline and compares the procedural output at the
// TOKEN level: the corpus's paren step-call spelling
// (`logic X ( event: event )`) and the brace form the terse lowering
// emits differ only in insignificant whitespace inside the call args,
// so a byte comparison would spuriously reject every paren-form
// construct while a token comparison proves the real property -- the
// downstream parser sees identical input.
func terseEquivalent(longhand, terse string) bool {
	terseLowered, err := NormaliseTerseAutomationSource(terse)
	if err != nil {
		return false
	}
	newOut, err := NormaliseAutomationSource(terseLowered)
	if err != nil {
		return false
	}
	oldOut, err := NormaliseAutomationSource(longhand)
	if err != nil {
		return false
	}
	// The hoist re-emits @trigger adjacent to the declaration, so a
	// corpus construct written trigger-first/description-second lowers
	// with its two annotation lines swapped. Annotation order is
	// name-keyed downstream (processFunctionAttributes switches on
	// Name), so the leading annotation run is compared as a set.
	return sameTokenStream(sortLeadingAnnotations(newOut), sortLeadingAnnotations(oldOut))
}

// sortLeadingAnnotations sorts the run of @-annotation lines at the
// top of a lowered construct, leaving everything after the run
// untouched.
func sortLeadingAnnotations(out string) string {
	lines := strings.Split(out, "\n")
	end := 0
	for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "@") {
		end++
	}
	if end < 2 {
		return out
	}
	ann := append([]string(nil), lines[:end]...)
	sort.Strings(ann)
	return strings.Join(append(ann, lines[end:]...), "\n")
}

// sameTokenStream reports whether two sources tokenize identically
// (type + literal, positions ignored). The call-arg brace-vs-paren
// spelling difference survives to the token level, so this still
// distinguishes genuinely different programs.
func sameTokenStream(a, b string) bool {
	ta, err := NewLexer(a).Tokenize()
	if err != nil {
		return false
	}
	tb, err := NewLexer(b).Tokenize()
	if err != nil {
		return false
	}
	if len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i].Type != tb[i].Type || ta[i].Literal != tb[i].Literal {
			return false
		}
	}
	return true
}

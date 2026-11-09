package routingrules

// generate.go -- rendering a routing rule's FORM as `rule` source text, for the
// nightly evidence review (epic memql#5146, design D5).
//
// # What this package is, and what it stopped being
//
// It used to ARM a runtime-authored rule too: routingRuleActivate rendered a
// form, ran it through the authoring pipeline and registered the construct in
// the owner-scoped authored runtime (epic memql#5127, design D7). That path was
// retired on 2026-11-08, because nothing that routes a call reads the authored
// runtime -- routing reads the shipped rules plus the routing configuration
// document (component/memql/routing_configuration_rules.go) -- so an armed rule
// never decided a single call. A custom rule is cluster routing configuration,
// saved with routingRuleSave, and Gate 1 refuses a `rule` construct in any
// authored bundle (component/memql/authoring_sandbox.go).
//
// What is left is the RENDERER. The evidence fold proposes a demotion as rule
// TEXT, hashes that exact text, and a person approves the hash, so an approval
// can never carry to a different rule. That guarantee needs exactly one
// renderer of the grammar -- a second would drift, and with a hash over the
// output the drift is silent -- and this is it. app/engine_authored.go installs
// it on the router integration, whose module cannot import this one.
//
// # Why no model is involved
//
// A rule proposed from evidence must be exactly the rule the evidence supports.
// A renderer that is a pure function of the form can only produce the rule the
// form says, and re-rendering an unchanged form produces byte-identical text,
// which is what makes hashing the output safe.
//
// # Why the output is short
//
// A rule construct is annotations and an empty body. There is no room here for
// a generator that is a program writing a program: every line below is one
// annotation, rendered from one field, and the whole output is short enough
// that the person approving it can read it and see what it does.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Form is the rule a proposal fills in, and deliberately NOT anything derived
// from a past rendering -- a generator that read its own past output would
// produce text whose content depended on it.
type Form struct {
	// Name is the rule's construct name.
	Name string
	// Description becomes the /// doc comment, which is what a reader of the
	// rule sees first.
	Description string

	// When is the condition set, keyed by the closed @when keys. A key present
	// with an empty value is a condition matching only an empty value; a key
	// ABSENT is no condition at all. The map preserves that distinction, which
	// a struct of strings could not.
	When map[string]string

	// Policy is required.
	Policy string
	// Level optionally overrides the call's declared level.
	Level string
	// Precedence orders the rule. A tie with another unlocked rule is refused,
	// so the form carries the number rather than inventing one.
	Precedence int
	// OnUnavailable is "degrade" or "park". Empty renders nothing and reads as
	// degrade, matching the DSL's own default.
	OnUnavailable string
	// Excludes removes concrete models from this rule's resolution.
	Excludes []string
}

// ConstructNameFor is the name the generated construct carries. It is the
// form's own name: a rule is addressed by name in the registry, in the
// decision record and on the operator's screen, and a derived name would make
// those three disagree.
func ConstructNameFor(f Form) string { return strings.TrimSpace(f.Name) }

// GenerateRule renders the construct. Its producer validates the form; the one
// thing it re-checks is the name, because rendering an empty one produces text
// the parser refuses with a message about a missing identifier.
func GenerateRule(f Form) (string, error) {
	name := ConstructNameFor(f)
	if name == "" {
		return "", fmt.Errorf("a rule needs a name")
	}
	var b strings.Builder
	if d := strings.TrimSpace(f.Description); d != "" {
		for _, line := range wrapDoc(d) {
			b.WriteString("/// " + line + "\n")
		}
	}
	b.WriteString("@when(" + renderWhen(f.When) + ")\n")
	if lvl := strings.TrimSpace(f.Level); lvl != "" {
		b.WriteString("@level(" + strconv.Quote(lvl) + ")\n")
	}
	b.WriteString("@policy(" + strconv.Quote(strings.TrimSpace(f.Policy)) + ")\n")
	b.WriteString("@precedence(" + strconv.Itoa(f.Precedence) + ")\n")
	if u := strings.TrimSpace(f.OnUnavailable); u != "" {
		b.WriteString("@onUnavailable(" + strconv.Quote(u) + ")\n")
	}
	for _, entry := range f.Excludes {
		if e := strings.TrimSpace(entry); e != "" {
			b.WriteString("@exclude(" + strconv.Quote(e) + ")\n")
		}
	}
	// NO @locked, ever. A rule carrying it evaluates before every shipped
	// rule, and that authority belongs to the embedded tree alone; nothing a
	// form describes is entitled to it. The absence here is enforcement, not
	// omission.
	b.WriteString("rule " + name + " { }\n")
	return b.String(), nil
}

// renderWhen emits the condition list in a STABLE order, so regenerating an
// unchanged form produces byte-identical source. Map iteration order would
// make every regeneration look like an edit to whatever hashes the source.
func renderWhen(when map[string]string) string {
	keys := make([]string, 0, len(when))
	for k := range when {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(when[k]))
	}
	return strings.Join(parts, ", ")
}

// wrapDoc breaks a description into doc-comment lines. It wraps on spaces at a
// conservative width rather than reflowing, because a description is the
// author's and the only thing being changed is where the lines end.
func wrapDoc(s string) []string {
	const width = 76
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(out, line)
}

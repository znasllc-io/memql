package annotations

import (
	"sort"
	"strings"
)

// Use is one written annotation, as the parser saw it: the name, the ONE form
// its arguments were written in, and -- for keyword arguments -- the keys, in
// the order written, each with its shape. The conversion from the parser's
// attribute lives in component/language/parser (AnnotationUse), because this
// package imports nothing.
type Use struct {
	Name string
	Form Form
	Keys []WrittenKey
}

// WrittenKey is one keyword key as written: its name, and whether it was
// written bare (`clusterOwner`) rather than with a value (`owner="x"`). A
// placement's key has a shape too -- a "flag" key is written bare, any other
// with a value -- and Check refuses a key written in the other one.
type WrittenKey struct {
	Name string
	Bare bool
}

// The refusal codes. They are the stable part of a refusal: the message may
// be reworded, the code may not, and the conformance corpus keys on it.
const (
	CodeUnknown   = "annotation_unknown"   // no receiver accepts the name
	CodeRetired   = "annotation_retired"   // the name is retired here
	CodeMisplaced = "annotation_misplaced" // other receivers accept the name
	CodeForm      = "annotation_form"      // the placement does not take this argument form
	CodeKey       = "annotation_key"       // a keyword argument the placement does not have
	CodeRepeated  = "annotation_repeated"  // a non-repeatable annotation written twice
)

// Refusal is the check's answer when it refuses: a stable code and a message
// that names the receiver, the annotation, and what to write instead.
type Refusal struct {
	Code    string
	Message string
}

// Error renders the refusal with its code last, in brackets, so a caller that
// prefixes context (a construct name, a position) leaves the code findable.
func (r *Refusal) Error() string {
	return r.Message + " [" + r.Code + "]"
}

// RuleCode is the refusal's stable rule id. A load report reads a refusal's
// code through this method (baseloader.CodedRefusal), without importing this
// package.
func (r *Refusal) RuleCode() string { return r.Code }

// Check decides one written annotation on one receiver. It returns nil when
// the receiver accepts the annotation in the form it was written, and the
// refusal otherwise.
//
// A name the receiver does not accept is, in this order: retired (on this
// receiver, or everywhere) and refused with its migration hint; live on other
// receivers and refused as misplaced, naming them; or unknown, refused with a
// did-you-mean over the receiver's own names and the whole list.
//
// A zero Form reads as a bare flag, the form of `@name`.
func Check(r Receiver, u Use) *Refusal {
	p, ok := Lookup(r, u.Name)
	if !ok {
		return refuseName(r, u.Name)
	}
	form := u.Form
	if form == 0 {
		form = FormFlag
	}
	if form&p.Forms == 0 {
		return &Refusal{Code: CodeForm, Message: "@" + p.Name + " on " + r.Phrase() + " takes " +
			p.Forms.String() + ", as in " + p.Example + " -- it was written with " + form.String()}
	}
	if form == FormKeywords {
		for _, key := range u.Keys {
			spec, ok := p.key(key.Name)
			if !ok {
				return keyRefusal(p, key.Name)
			}
			if flag := spec.Type == "flag"; flag != key.Bare {
				return keyShapeRefusal(p, spec)
			}
		}
	}
	return nil
}

// CheckAll decides every annotation written on one declaration, in order, and
// returns the first refusal. On top of Check it refuses a non-repeatable
// annotation written twice: the parsers fold annotations one at a time, so a
// second one would silently win or silently lose, and a reader scanning
// top-down could not tell which.
func CheckAll(r Receiver, uses []Use) *Refusal {
	seen := make(map[string]bool, len(uses))
	for _, u := range uses {
		if ref := Check(r, u); ref != nil {
			return ref
		}
		if seen[u.Name] {
			if p, _ := Lookup(r, u.Name); !p.Repeatable {
				return &Refusal{Code: CodeRepeated, Message: "@" + u.Name + " is written more than once on " +
					r.Phrase() + " -- it is not repeatable, so a reader cannot tell which one takes effect; keep one"}
			}
		}
		seen[u.Name] = true
	}
	return nil
}

// refuseName explains a name the receiver does not accept.
func refuseName(r Receiver, name string) *Refusal {
	if hint, retired := retiredHint(r, name); retired {
		return &Refusal{Code: CodeRetired, Message: "@" + name + " on " + r.Phrase() + " is retired -- " + hint}
	}
	if elsewhere := placementsNamed(name); len(elsewhere) > 0 {
		phrases := make([]string, 0, len(elsewhere))
		for _, p := range elsewhere {
			phrases = append(phrases, p.Receiver.Phrase())
		}
		// The example comes from the same kind of place the name was written
		// in when one accepts it: a field is shown a field's `@default("...")`,
		// not the provider's bare `@default`, which every field refuses.
		example := elsewhere[0].Example
		for _, p := range elsewhere {
			if p.Receiver.isField() == r.isField() {
				example = p.Example
				break
			}
		}
		msg := "@" + name + " is not valid on " + r.Phrase() + " -- it is accepted on " +
			joinWords(phrases) + ", as in " + example
		if hint, ok := misplacedHints[name]; ok {
			msg += ". " + hint
		}
		return &Refusal{Code: CodeMisplaced, Message: msg}
	}
	names := ByReceiver[string(r)]
	msg := "unknown annotation @" + name + " on " + r.Phrase()
	accepts := r.Phrase() + " accepts " + atList(names)
	if len(names) == 0 {
		accepts = r.Phrase() + " accepts no annotations"
	}
	if suggestion, ok := nearest(name, names); ok {
		msg += " -- did you mean @" + suggestion + "? " + capitalize(accepts)
	} else {
		msg += " -- " + accepts
	}
	return &Refusal{Code: CodeUnknown, Message: msg}
}

// keyRefusal explains a keyword argument the placement does not have.
func keyRefusal(p Placement, key string) *Refusal {
	names := make([]string, 0, len(p.Keys))
	rendered := make([]string, 0, len(p.Keys))
	for _, k := range p.Keys {
		names = append(names, k.Name)
		if k.Type == "flag" {
			rendered = append(rendered, k.Name)
		} else {
			rendered = append(rendered, k.Name+"=")
		}
	}
	msg := "@" + p.Name + " on " + p.Receiver.Phrase() + " has no key " + key
	keys := "its keys are " + joinWords(rendered) + ", as in " + p.Example
	if suggestion, ok := nearest(key, names); ok {
		msg += " -- did you mean " + suggestion + "? " + capitalize(keys)
	} else {
		msg += " -- " + keys
	}
	return &Refusal{Code: CodeKey, Message: msg}
}

// keyShapeRefusal explains a key the placement has, written in the other
// shape: a flag given a value, or a valued key written bare. The second is
// the dangerous one -- the parser stores a bare key as `true`, and a reader
// wanting the key's value reads "" and carries on.
func keyShapeRefusal(p Placement, spec ArgSpec) *Refusal {
	head := "@" + p.Name + " on " + p.Receiver.Phrase() + ": " + spec.Name
	if spec.Type == "flag" {
		return &Refusal{Code: CodeKey, Message: head + " is a flag and takes no value -- write it bare (" +
			spec.Name + "), as in " + p.Example}
	}
	return &Refusal{Code: CodeKey, Message: head + " takes a value -- write " + valuedSpelling(spec) +
		", as in " + p.Example}
}

// valuedSpelling renders a valued key the way it is written:
// `maxCalls=<number>`, `ttl="..."`.
func valuedSpelling(k ArgSpec) string {
	if k.Type == "int" || k.Type == "number" {
		return k.Name + "=<number>"
	}
	return k.Name + `="..."`
}

// key returns the placement's keyword key named name.
func (p Placement) key(name string) (ArgSpec, bool) {
	for _, k := range p.Keys {
		if k.Name == name {
			return k, true
		}
	}
	return ArgSpec{}, false
}

// atList renders names as `@a, @b and @c`, sorted.
func atList(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]string, len(sorted))
	for i, n := range sorted {
		out[i] = "@" + n
	}
	return joinWords(out)
}

// joinWords joins with commas and a final "and".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// nearest returns the candidate closest to name by edit distance, when it is
// close enough to be the name the author meant: a difference in case alone,
// or at most two edits (three for a name of eight letters or more). Ties
// break on the lexically smaller candidate, so the answer does not depend on
// the order of the list.
func nearest(name string, candidates []string) (string, bool) {
	best, bestDist := "", -1
	lower := strings.ToLower(name)
	for _, c := range candidates {
		if c == name {
			continue
		}
		d := levenshtein(lower, strings.ToLower(c))
		if bestDist == -1 || d < bestDist || (d == bestDist && c < best) {
			best, bestDist = c, d
		}
	}
	limit := 2
	if len(name) >= 8 {
		limit = 3
	}
	if bestDist >= 0 && bestDist <= limit && bestDist < len(name) {
		return best, true
	}
	return "", false
}

// levenshtein is the edit distance between a and b.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

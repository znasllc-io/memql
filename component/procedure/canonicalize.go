package procedure

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// pureReadTypes are the step types whose whole contribution is a RESULT. When
// nothing later consumed that result, the step is noise and Canonicalize drops
// it.
//
// exec is deliberately absent, and that absence is the design. A command's
// EFFECT is not knowable from whether its output was read: `rm -rf build`
// consumes nothing and is the most important action in its trace. Dropping on
// Consumed alone would delete most commands in a corpus and lose exactly the
// side effects the corpus exists to learn.
var pureReadTypes = map[string]bool{
	"fs_read": true,
	"fetch":   true,
	"mcp":     true,
}

// Canonicalize turns recorded steps into actions: a tool identity, an argument
// TREE, and the two digests.
//
// The trees are the point. argv, JSON and paths arrive as opaque strings, and
// two recordings of the same command with a different filename differ in ONE
// string -- which anti-unification can only generalize into a hole covering
// the whole command. Parsed, they differ in one leaf, and the template that
// comes out has one hole in the right place.
func Canonicalize(steps []Step) []Action {
	out := make([]Action, 0, len(steps))
	for _, s := range steps {
		if pureReadTypes[s.StepType] && !s.Consumed {
			continue
		}
		out = append(out, Action{
			Tool:         toolOf(s),
			Args:         canonicalizeArgs(s.Input),
			ResultDigest: s.ResultDigest,
			EffectDigest: s.EffectDigest,
			ResultValue:  s.ResultValue,
			Key:          s.Key,
			Seq:          s.Seq,
		})
	}
	return out
}

// toolOf is the canonical identity, and it is ONE vocabulary for both of
// D24's corpus levels on purpose: a reader asking what a step did should not
// first have to ask which writer wrote the row.
func toolOf(s Step) string {
	if s.StepType == "automation" && s.Call.Name != "" {
		return "automation:" + s.Call.Name
	}
	return s.StepType
}

// canonicalizeArgs lowers a recorded input map into a tree.
func canonicalizeArgs(in map[string]any) *Node {
	if len(in) == 0 {
		return Obj(nil)
	}
	m := make(map[string]*Node, len(in))
	for k, v := range in {
		m[k] = canonicalizeValue(k, v)
	}
	return Obj(m)
}

// canonicalizeValue lowers one recorded value. A string is the interesting
// case: it may be a command line, a JSON document, a path, or just a string,
// and the three structured readings are tried in that order.
func canonicalizeValue(key string, v any) *Node {
	if lit, litType, ok := scalarLiteral(v); ok {
		return LitOf(lit, litType)
	}
	switch t := v.(type) {
	case nil:
		// The empty spelling keeps a null Equal to "" -- scalars fold to one
		// spelling -- and the type keeps a replay from sending "" for it.
		return LitOf("", "null")
	case string:
		return canonicalizeString(key, t)
	case []any:
		kids := make([]*Node, len(t))
		for i, e := range t {
			if s, ok := e.(string); ok && commandKeys[key] {
				// An ARGUMENT VECTOR -- Codex's exec events record argv this
				// way -- is already split, and each element is one argument
				// taken verbatim. Reading one again as a command line would
				// treat the apostrophe in `don't` as an opening quote and
				// lose it, and a replay would send a message nobody wrote.
				// Kept whole, the vector is also Equal to the same command
				// recorded as a string, so the two recordings generalize
				// together.
				kids[i] = Lit(s)
				continue
			}
			kids[i] = canonicalizeValue(key, e)
		}
		return Arr(kids...)
	case map[string]any:
		m := make(map[string]*Node, len(t))
		for k, e := range t {
			m[k] = canonicalizeValue(k, e)
		}
		return Obj(m)
	default:
		return Lit("")
	}
}

// scalarLiteral is the ONE spelling of a recorded non-string scalar. It is a
// function of its own because two callers must agree on it exactly:
// canonicalization, which spells what a recording sent, and InputLiteral, which
// spells what a goal's input supplies. A number the two spelled differently
// would map a free parameter to its input at learning time and miss it at
// replay time.
func scalarLiteral(v any) (lit, litType string, ok bool) {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t), "bool", true
	case float64:
		return formatNumber(t), "number", true
	case int:
		return strconv.Itoa(t), "number", true
	case int64:
		return strconv.FormatInt(t, 10), "number", true
	case json.Number:
		return t.String(), "number", true
	}
	return "", "", false
}

// commandKeys are the argument names whose value is a command line. Naming
// them is what lets argv splitting apply to a command and NOT to a message
// that happens to contain spaces.
var commandKeys = map[string]bool{
	"command": true,
	"cmd":     true,
	"argv":    true,
}

// pathKeys are the argument names whose value is a filesystem path.
var pathKeys = map[string]bool{
	"path":       true,
	"file":       true,
	"filePath":   true,
	"cwd":        true,
	"directory":  true,
	"targetPath": true,
}

// canonicalizeString tries the three structured readings in order and MARKS
// the one it took (Node.Form). The mark is what Materialize turns back into
// the string: without it an argv, a path and a JSON array are three arrays
// nobody can tell apart.
func canonicalizeString(key, s string) *Node {
	if commandKeys[key] {
		n := Arr(splitArgv(s)...)
		n.Form = FormArgv
		return n
	}
	if n, ok := parseJSONTree(s); ok {
		n.Form = FormJSON
		return n
	}
	if pathKeys[key] || looksLikePath(s) {
		n := Arr(splitPath(s)...)
		n.Form = FormPath
		if strings.HasPrefix(s, "/") {
			n.Form = FormRootedPath
		}
		return n
	}
	return Lit(s)
}

// parseJSONTree decodes a string that is a JSON object or array. A bare scalar
// is deliberately NOT treated as JSON: "true" and "1" are far more often a
// string argument than a document, and reading them as one would make two
// different arguments compare equal.
func parseJSONTree(s string) (*Node, bool) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || (t[0] != '{' && t[0] != '[') {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return canonicalizeValue("", v), true
}

func looksLikePath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// splitPath splits a path into its segments, dropping the empty leading one a
// rooted path produces. The leading slash is not information the template
// needs: two recordings differing only in a root would otherwise differ in a
// segment that is always empty. It is not LOST either -- the caller records it
// as FormRootedPath.
//
// Every OTHER empty segment is kept, and that is what makes the split
// reversible. "a//b" is a protocol-relative URL as often as a sloppy path,
// "dir/" and "dir" differ to rsync, and a string that merely STARTS like a
// path -- a source file whose first line is a // comment -- is split here
// too. Dropping empties would make Materialize write "/ comment" for "//
// comment" and corrupt every such file a replay writes.
func splitPath(s string) []*Node {
	rest := strings.TrimPrefix(s, "/")
	if rest == "" {
		return nil
	}
	parts := strings.Split(rest, "/")
	out := make([]*Node, len(parts))
	for i, p := range parts {
		out[i] = Lit(p)
	}
	return out
}

// splitArgv splits a command line on whitespace, honouring single and double
// quotes and backslash escapes. It is not a shell: it does not expand,
// substitute or interpret operators, because the recording is evidence of what
// ran and re-interpreting it would be a second opinion about it.
func splitArgv(s string) []*Node {
	var (
		out   []*Node
		cur   strings.Builder
		open  bool
		quote rune
		esc   bool
	)
	flush := func() {
		if open {
			out = append(out, Lit(cur.String()))
			cur.Reset()
			open = false
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			open = true
			esc = false
		case r == '\\' && quote != '\'':
			esc = true
			open = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			open = true
		case r == '\'' || r == '"':
			quote = r
			open = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			open = true
		}
	}
	flush()
	return out
}

// formatNumber writes a float back in the shortest spelling that round-trips,
// and without an exponent for an integral value. 1 and 1.0 must produce the
// same literal or two recordings of one call compare unequal.
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// SortActions orders actions by their recorded sequence. Canonicalize
// preserves input order; a caller assembling a sequence from rows that arrived
// out of order calls this first, because every stage below reads position as
// meaning.
func SortActions(a []Action) {
	sort.SliceStable(a, func(i, j int) bool { return a[i].Seq < a[j].Seq })
}

package procedure

import (
	"encoding/json"
	"fmt"
	"math"
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
		toks, seps := scanArgv(s)
		n := Arr(toks...)
		n.Form = FormArgv
		n.Seps = seps
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

// splitArgv is a command line's arguments, each carrying its spelling
// (scanArgv without the separators).
func splitArgv(s string) []*Node {
	toks, _ := scanArgv(s)
	return toks
}

// scanArgv splits a command line on whitespace the way a POSIX shell removes
// quotes, and keeps how it was WRITTEN: every argument's exact source text
// (Node.Raw) and the exact text around the arguments -- the separators, one
// more than the arguments. Put back together the pieces are the line, byte
// for byte, which is what lets Materialize send a recorded command as it ran.
//
// The values follow POSIX quote removal exactly, because a value is what the
// PROGRAM received and two recordings are compared by it:
//
//   - inside single quotes nothing is special;
//   - inside double quotes a backslash escapes only $, `, ", \ and a newline,
//     and before any other character it is a literal backslash -- so
//     `grep -E "\d+"` searched for \d+, and `printf "a\tb"` printed a\tb;
//   - outside quotes a backslash escapes the next character, and one that
//     ends the line is kept, as sh keeps it;
//   - a backslash-newline outside single quotes is a line continuation and
//     vanishes. It belongs to an argument only when the argument goes on
//     after it; otherwise it is separator text.
//
// It is still not a shell: it does not expand, substitute or interpret
// operators, because the recording is evidence of what ran and
// re-interpreting it would be a second opinion about it. A value holding
// `$(date +%F)` is the text the shell was handed, not what it expanded to --
// which is exactly why the spelling is kept beside it.
//
// It reads bytes rather than runes: every character it treats specially is
// ASCII, and no byte of a multi-byte UTF-8 sequence is, so a spelling sliced
// at these boundaries is always whole runes.
func scanArgv(s string) ([]*Node, []string) {
	var (
		toks    []*Node
		seps    []string
		val     strings.Builder
		open    bool // an argument is being read
		start   int  // where the open argument's spelling begins
		end     int  // just past the last byte that belongs to it
		sepFrom int  // where the separator before the next argument begins
		quote   byte // 0, '\'' or '"'
	)
	begin := func(at int) {
		if !open {
			open, start = true, at
		}
	}
	flush := func() {
		seps = append(seps, s[sepFrom:start])
		toks = append(toks, &Node{Kind: KindLit, Lit: val.String(), Raw: s[start:end]})
		val.Reset()
		open, sepFrom = false, end
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				val.WriteByte(c)
			}
			i++
			end = i
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
				i++
			case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
				i += 2
			case c == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\", s[i+1]) >= 0:
				val.WriteByte(s[i+1])
				i += 2
			default:
				val.WriteByte(c)
				i++
			}
			end = i
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i += 2
		case c == '\\':
			begin(i)
			if i+1 < len(s) {
				val.WriteByte(s[i+1])
				i += 2
			} else {
				val.WriteByte(c)
				i++
			}
			end = i
		case c == '\'' || c == '"':
			begin(i)
			quote = c
			i++
			end = i
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if open {
				flush()
			}
			i++
		default:
			begin(i)
			val.WriteByte(c)
			i++
			end = i
		}
	}
	if open {
		flush()
	}
	seps = append(seps, s[sepFrom:])
	return toks, seps
}

// spelledAs reports whether raw is ONE argument whose value is lit, with
// nothing around it -- what an argument's recorded spelling must be, so the
// command a replay sends says what the tree it was approved as says.
func spelledAs(raw, lit string) bool {
	toks, seps := scanArgv(raw)
	return len(toks) == 1 && toks[0].Lit == lit && seps[0] == "" && seps[1] == ""
}

// checkSeps is what a command line's separators must be: one more than its
// arguments, each nothing but the text scanArgv puts between arguments --
// blanks and line continuations -- and every one BETWEEN two arguments holding
// a blank, or the two would be sent as one argument. Separators that carried
// anything else would send text the tree does not describe.
func checkSeps(seps []string, args int) error {
	if len(seps) != args+1 {
		return fmt.Errorf("procedure: a command line of %d argument(s) carries %d separator(s), not %d", args, len(seps), args+1)
	}
	for i, s := range seps {
		blank := false
		for j := 0; j < len(s); j++ {
			switch s[j] {
			case ' ', '\t', '\r', '\n':
				blank = true
			case '\\':
				if j+1 < len(s) && s[j+1] == '\n' {
					j++
					continue
				}
				return fmt.Errorf("procedure: separator %d of a command line is %q, which is not whitespace", i, s)
			default:
				return fmt.Errorf("procedure: separator %d of a command line is %q, which is not whitespace", i, s)
			}
		}
		if !blank && i > 0 && i < args {
			return fmt.Errorf("procedure: separator %d of a command line is %q, which does not separate two arguments", i, s)
		}
	}
	return nil
}

// formatNumber writes a float back in the shortest spelling that round-trips,
// and without an exponent for an integral value. 1 and 1.0 must produce the
// same literal or two recordings of one call compare unequal.
//
// The int64 range is checked BEFORE any conversion. Converting a float
// outside it is implementation-defined: amd64 answers the integer indefinite
// value, which never equals the float, but arm64 saturates, and 2^63 used to
// come back as 9223372036854775807 -- another number, spelled one way on a
// Mac and another on a Linux replica. This module may not import core/num,
// which owns that narrowing for the rest of the tree; the guard is its local
// equivalent.
func formatNumber(f float64) string {
	const bound = 1 << 63 // exactly representable: -bound is int64's minimum, bound is one past its maximum
	if f >= -bound && f < bound && f == math.Trunc(f) {
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

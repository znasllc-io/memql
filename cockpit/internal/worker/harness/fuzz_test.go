package harness

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Fuzz targets for the recording's two translations that take input from
// an app. Each asserts the PROPERTY that would actually go wrong, never
// merely "does not panic": nothing here panics, and the failure worth
// finding is a plausible answer that is wrong.

// splitShellWords reads a command line the way a POSIX shell reads the
// only two forms shellJoin writes: bare words, and single-quoted runs in
// which a quote of the word's own is written as close-quote, backslash,
// quote, open-quote.
func splitShellWords(line string) ([]string, bool) {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
				continue
			}
			word.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\':
			if i+1 >= len(line) {
				return nil, false
			}
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		return nil, false
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, true
}

// FuzzShellJoinRoundTrips. The fallback's exec events carry an argv, and
// the recording carries it as one command line. The property: a shell
// reading that line back gets EXACTLY the argv the app ran -- the same
// words, the same boundaries, nothing expanded, and a first word that is
// the command rather than an assignment or a keyword. A rendering that
// did not round-trip would record a command the app never ran, and a
// replay would run it.
func FuzzShellJoinRoundTrips(f *testing.F) {
	for _, seed := range [][2]string{
		{"bash", "-lc"}, {"sh -c 'echo oops >&2; exit 3'", ""}, {"it's", "a\"b"},
		{"=cmd", "$HOME"}, {"a b", "`id`"}, {"*.go", "~"}, {"\n", "\t"}, {"", "'"},
		{"FOO=bar", "ls"}, {"if", "true"}, {"time", "make"},
	} {
		f.Add(seed[0], seed[1])
	}
	// Checked against lists of the test's own, not against the ones
	// shellJoin trusts, so a word dropped from those by mistake is caught.
	keywords := map[string]bool{
		"case": true, "do": true, "done": true, "elif": true, "else": true, "esac": true,
		"fi": true, "for": true, "if": true, "in": true, "then": true, "until": true,
		"while": true, "time": true, "function": true, "select": true, "coproc": true,
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		argv := []string{a, b, "x"}
		line := shellJoin(argv)
		got, ok := splitShellWords(line)
		if !ok || len(got) != len(argv) {
			t.Fatalf("shellJoin(%q) = %s, which reads back as %q", argv, line, got)
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Fatalf("shellJoin(%q) = %s: word %d reads back as %q", argv, line, i, got[i])
			}
		}
		// The command's name left bare is never one a shell reads as
		// something else.
		if first, _, _ := strings.Cut(line, " "); first == argv[0] {
			if strings.Contains(first, "=") || keywords[first] {
				t.Fatalf("shellJoin(%q) = %s: the command %q is left bare", argv, line, first)
			}
		}
		// A word left bare is only ever made of characters no shell reads
		// specially.
		for _, w := range argv {
			if shellWord(w) != w {
				continue
			}
			if w == "" || w[0] == '=' || strings.ContainsAny(w, " \t\n'\"\\$`*?[]{}~!;&|<>()#^") {
				t.Fatalf("shellWord left %q bare", w)
			}
		}
	})
}

var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// FuzzResultShapeIsClosed. Whatever an app returns, the recording types
// it from the six JSON types the engine reads and digests it in the one
// shape it compares -- and a type other than "string" is only ever given
// to text that really is that JSON. A result typed "object" that does not
// parse would make a replay compare the wrong way.
func FuzzResultShapeIsClosed(f *testing.F) {
	for _, seed := range []string{
		"", "notes.txt", `{"a":1}`, `[1,2]`, "42", "true", "null", `"quoted"`,
		`{"a":1} trailing`, "\xff\xfe", " 7 ", "1e400", `{"a":`,
	} {
		f.Add(seed)
	}
	closed := map[string]bool{"object": true, "array": true, "string": true, "number": true, "boolean": true, "null": true}
	f.Fuzz(func(t *testing.T, text string) {
		typ, digest := resultShape(text)
		if !closed[typ] || !digestShape.MatchString(digest) {
			t.Fatalf("resultShape(%q) = %q %q", text, typ, digest)
		}
		if digest != Digest([]byte(text)) {
			t.Fatalf("text digests as something other than its own bytes")
		}
		if typ != "string" && !json.Valid([]byte(text)) {
			t.Fatalf("resultShape(%q) typed text that is not JSON as %q", text, typ)
		}
		if v, ok := decodeValue(json.RawMessage(text)); ok {
			if again, _ := resultShape(v); !closed[again] {
				t.Fatalf("decoded %q typed as %q", text, again)
			}
		}
	})
}

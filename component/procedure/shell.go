package procedure

import (
	"strconv"
	"strings"
)

// shell.go reads a command line the way a POSIX shell would read it, for the
// questions a replay has to answer about one -- never to run it. What ran is
// what was recorded; these functions only say what a spelling MEANS.

// spelledWithExpansion reports whether one argument's recorded spelling
// carries a shell expansion, so that the value the program received is not
// the value the recording kept (Node.Lit is the text after quote removal and
// before expansion):
//
//   - an unescaped $ or backtick outside single quotes -- parameter, command
//     and arithmetic expansion, inside double quotes as well;
//   - an unquoted ~ at the start -- tilde expansion;
//   - an unquoted, unescaped *, ? or [ -- pathname expansion.
//
// These are POSIX's word expansions that can change a word (sh 2.6); brace
// expansion is bash's, not sh's, and field splitting only ever applies to what
// an expansion produced. Skipping the character after a backslash is exact:
// outside quotes a backslash escapes every character, and inside double
// quotes the two an expansion can start with, $ and `, are among the ones it
// escapes.
func spelledWithExpansion(spelling string) bool {
	var quote byte
	for i := 0; i < len(spelling); i++ {
		c := spelling[i]
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
			}
		case '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				i++
			case '$', '`':
				return true
			}
		default:
			switch c {
			case '\'', '"':
				quote = c
			case '\\':
				i++
			case '$', '`', '*', '?', '[':
				return true
			case '~':
				if i == 0 {
					return true
				}
			}
		}
	}
	return false
}

// shellItem is one piece of a command line as a shell reads its simple
// commands: a word, or a control operator that ends one.
type shellItem struct {
	// op is a control operator -- &&, ||, ;, |, &, |& or a newline -- and
	// empty for a word.
	op string
	// word is a word's text after quote removal.
	word string
	// slots are the placeholders the word holds (slotText): the elements of a
	// template's command line with no text of their own -- a parameter, or a
	// structure. A word holding one is not a word anybody recorded.
	slots []int
	// here: the word holds an unquoted << -- a here-document or here-string
	// operator.
	here bool
}

// shellReading is a command line read into its words and operators, and the
// placeholders found inside a here-document's BODY, which is the command's
// input rather than any word of it.
type shellReading struct {
	items     []shellItem
	bodySlots []int
}

// hereDoc is a here-document waiting for its body: the delimiter, and whether
// `<<-` strips the body's leading tabs.
type hereDoc struct {
	delim string
	strip bool
}

// slotText is the placeholder written into a command line for element k of a
// template's argv. NUL delimits it because no command line a shell runs can
// hold one -- a parameter carrying one is refused -- so a placeholder never
// meets real text.
func slotText(k int) string { return "\x00" + strconv.Itoa(k) + "\x00" }

// readSlot reads a placeholder at the start of s.
func readSlot(s string) (k, width int, ok bool) {
	if len(s) < 3 || s[0] != 0 {
		return 0, 0, false
	}
	j := 1
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		k = k*10 + int(s[j]-'0')
		j++
	}
	if j == 1 || j >= len(s) || s[j] != 0 {
		return 0, 0, false
	}
	return k, j + 1, true
}

// slotsIn is every placeholder in a stretch of text.
func slotsIn(s string) []int {
	var out []int
	for i := 0; i < len(s); i++ {
		if k, w, ok := readSlot(s[i:]); ok {
			out = append(out, k)
			i += w - 1
		}
	}
	return out
}

// readShell reads a command line into words and control operators the way a
// POSIX shell tokenizes one: quotes removed as scanArgv removes them, an
// unquoted control operator ending a word wherever it stands (`app&&npm` is
// two words and an operator; `"a|b"` is one word), a redirection that holds
// & or | (`2>&1`, `<&0`, `&>f`, `>|`) left inside its word, a newline ending
// a command, and a here-document's body read up to its delimiter line and set
// aside rather than read as commands.
//
// Like scanArgv it expands and runs nothing, and it is deliberately not a
// whole shell: a command substitution inside double quotes is one word, and a
// reserved word (if, while, {) is a word like any other. What it is for is
// finding COMMAND WORDS and SCRIPTS -- which program a word will run as, and
// which word a shell will run as code -- in a line a replay is about to send.
func readShell(src string) shellReading {
	var (
		r       shellReading
		val     strings.Builder
		open    bool
		slots   []int
		here    bool
		prev    byte // the last byte read unquoted into the open word
		quote   byte
		delimAt = -1 // where this word's here-document delimiter begins in val
		strip   bool
		// wantDelim: a word that was exactly << or <<- -- the next word is
		// its delimiter.
		wantDelim *hereDoc
		pending   []hereDoc
	)
	endWord := func() {
		if !open {
			return
		}
		w := val.String()
		switch {
		case wantDelim != nil && !here:
			pending = append(pending, hereDoc{delim: w, strip: wantDelim.strip})
			wantDelim = nil
		case delimAt >= 0 && delimAt < len(w):
			pending = append(pending, hereDoc{delim: w[delimAt:], strip: strip})
		case delimAt >= 0:
			wantDelim = &hereDoc{strip: strip}
		}
		r.items = append(r.items, shellItem{word: w, slots: slots, here: here})
		val.Reset()
		open, slots, here, prev, delimAt, strip = false, nil, false, 0, -1, false
	}
	op := func(o string) {
		endWord()
		r.items = append(r.items, shellItem{op: o})
	}
	// bodies reads the pending here-documents' bodies from from, and answers
	// where the command line resumes.
	bodies := func(from int) int {
		pos := from
		for _, h := range pending {
			for pos < len(src) {
				line, next := src[pos:], len(src)
				if j := strings.IndexByte(src[pos:], '\n'); j >= 0 {
					line, next = src[pos:pos+j], pos+j+1
				}
				pos = next
				cmp := line
				if h.strip {
					cmp = strings.TrimLeft(line, "\t")
				}
				if cmp == h.delim {
					break
				}
				r.bodySlots = append(r.bodySlots, slotsIn(line)...)
			}
		}
		pending = nil
		return pos
	}
	for i := 0; i < len(src); {
		c := src[i]
		if c == 0 {
			if k, w, ok := readSlot(src[i:]); ok {
				open = true
				slots = append(slots, k)
				prev = 0
				i += w
				continue
			}
		}
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				val.WriteByte(c)
			}
			i++
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
				i++
			case c == '\\' && i+1 < len(src) && src[i+1] == '\n':
				i += 2
			case c == '\\' && i+1 < len(src) && strings.IndexByte("$`\"\\", src[i+1]) >= 0:
				val.WriteByte(src[i+1])
				i += 2
			default:
				val.WriteByte(c)
				i++
			}
		case c == '\\' && i+1 < len(src) && src[i+1] == '\n':
			i += 2
		case c == '\\':
			open, prev = true, 0
			if i+1 < len(src) {
				val.WriteByte(src[i+1])
				i += 2
			} else {
				val.WriteByte(c)
				i++
			}
		case c == '\'' || c == '"':
			open, prev, quote = true, 0, c
			i++
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			i++
		case c == '\n':
			op("\n")
			i++
			if len(pending) > 0 {
				i = bodies(i)
			}
		case c == ';':
			op(";")
			i++
		case c == '&':
			switch {
			case i+1 < len(src) && src[i+1] == '&':
				op("&&")
				i += 2
			case (open && (prev == '>' || prev == '<')) || (i+1 < len(src) && src[i+1] == '>'):
				open, prev = true, c
				val.WriteByte(c)
				i++
			default:
				op("&")
				i++
			}
		case c == '|':
			switch {
			case i+1 < len(src) && src[i+1] == '|':
				op("||")
				i += 2
			case i+1 < len(src) && src[i+1] == '&':
				op("|&")
				i += 2
			case open && prev == '>':
				prev = c
				val.WriteByte(c)
				i++
			default:
				op("|")
				i++
			}
		case c == '<' && i+1 < len(src) && src[i+1] == '<':
			open, here = true, true
			if i+2 < len(src) && src[i+2] == '<' {
				// A here-string: its word is fed to the command, and it
				// has no body.
				val.WriteString("<<<")
				prev = '<'
				i += 3
				break
			}
			val.WriteString("<<")
			i += 2
			strip = i < len(src) && src[i] == '-'
			if strip {
				val.WriteByte('-')
				i++
			}
			delimAt, prev = val.Len(), 0
		default:
			open, prev = true, c
			val.WriteByte(c)
			i++
		}
	}
	endWord()
	return r
}

// simpleCommands splits a reading's items at its control operators: each is
// one simple command's words, in order.
func simpleCommands(items []shellItem) [][]shellItem {
	var (
		out [][]shellItem
		cur []shellItem
	)
	for _, it := range items {
		if it.op != "" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, it)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// argvSource is the command line a FormArgv node materializes as, with every
// element that has no text of its own -- a parameter, or a structure --
// written as a placeholder (slotText k for elems[k]). The separators and the
// spellings are the ones Materialize writes, so the line read here is the line
// a replay sends, whatever a parameter's value.
func argvSource(n *Node) (string, []*Node) {
	var (
		b     strings.Builder
		elems []*Node
	)
	seps := n.Seps
	if seps != nil && checkSeps(seps, len(n.Kids)) != nil {
		seps = nil
	}
	for i, k := range n.Kids {
		switch {
		case seps != nil:
			b.WriteString(seps[i])
		case i > 0:
			b.WriteByte(' ')
		}
		switch {
		case k != nil && k.Kind == KindLit && k.Raw != "":
			b.WriteString(k.Raw)
		case k != nil && k.Kind == KindLit:
			b.WriteString(shellQuote(k.Lit))
		default:
			b.WriteString(slotText(len(elems)))
			elems = append(elems, k)
		}
	}
	if seps != nil {
		b.WriteString(seps[len(n.Kids)])
	}
	return b.String(), elems
}

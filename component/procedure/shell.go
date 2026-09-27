package procedure

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

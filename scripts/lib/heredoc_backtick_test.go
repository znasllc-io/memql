package lib

// Backticks in an UNQUOTED heredoc are command substitutions. Help text is
// the usual place for one by accident: `make release` written as markdown
// quoting inside `cat <<EOF` runs make release every time --help is read.
// That shipped in scripts/release/release.sh, whose help is unquoted because
// it interpolates the default ACR, and under scripts/release make's built-in
// `%: %.sh` rule turned each --help into a copy of release.sh named release.
//
// The rule: a backtick in an unquoted heredoc is escaped (\`), the form every
// other script here already uses. Quote the delimiter (<<'EOF') when nothing
// needs interpolating, and write $(...) for a substitution that is meant.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

var reHeredocOpener = regexp.MustCompile(`<<(-?)[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// unescapedHeredocBackticks returns "line: text" for every line of an
// unquoted heredoc's body that holds an unescaped backtick. Full-line
// comments open no heredoc.
func unescapedHeredocBackticks(src string) []string {
	var found []string
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		m := reHeredocOpener.FindStringSubmatch(lines[i])
		if m == nil || strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			continue
		}
		stripTabs, quoted, word := m[1] == "-", m[2] != "", m[3]
		for i++; i < len(lines); i++ {
			body := lines[i]
			if stripTabs {
				body = strings.TrimLeft(body, "\t")
			}
			if body == word {
				break
			}
			if !quoted && hasUnescapedBacktick(lines[i]) {
				found = append(found, strconv.Itoa(i+1)+": "+strings.TrimSpace(lines[i]))
			}
		}
	}
	return found
}

func hasUnescapedBacktick(s string) bool {
	for j := 0; j < len(s); j++ {
		if s[j] == '`' && (j == 0 || s[j-1] != '\\') {
			return true
		}
	}
	return false
}

func TestUnescapedHeredocBackticksCatchesTheShape(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"an unquoted heredoc runs a backticked command", "cat <<EOF\nhow `make release` forwards\nEOF\n", 1},
		{"an escaped backtick is literal", "cat <<EOF\nrun \\`make help\\`\nEOF\n", 0},
		{"a quoted delimiter makes the body literal", "cat <<'EOF'\nhow `make release` forwards\nEOF\n", 0},
		{"a <<- heredoc ends at a tab-indented delimiter", "\tcat <<-EOF\n\tfine\n\tEOF\necho `date`\n", 0},
		{"a comment that mentions << opens nothing", "# cat <<EOF\n`date`\n", 0},
	}
	for _, tc := range cases {
		if got := unescapedHeredocBackticks(tc.src); len(got) != tc.want {
			t.Errorf("%s: got %d finding(s) %q, want %d", tc.name, len(got), got, tc.want)
		}
	}
}

func TestNoUnescapedBacktickInAnUnquotedHeredoc(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	err := filepath.Walk(filepath.Join(root, "scripts"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && repowalk.SkipDir(info.Name()) {
			return filepath.SkipDir
		}
		if info.IsDir() || !strings.HasSuffix(path, ".sh") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, f := range unescapedHeredocBackticks(string(b)) {
			t.Errorf("%s:%s\nA backtick in an unquoted heredoc is a command substitution: it runs when "+
				"the text is printed. Escape it (\\`), quote the delimiter (<<'EOF'), or write $(...) "+
				"for a substitution that is meant.", rel(t, path), f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned no scripts -- the walk is broken")
	}
}

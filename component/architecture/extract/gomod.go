package extract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readModulePath returns the `module ...` declaration from
// <dir>/go.mod. We parse it ourselves rather than depending on
// golang.org/x/mod -- the line we care about is unambiguous and a
// new transitive dep here would have to be added everywhere the
// extractor ships (CLI, tests, future cockpit integration).
func readModulePath(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		mod := strings.TrimSpace(strings.TrimPrefix(line, "module "))
		if i := strings.Index(mod, "//"); i >= 0 {
			mod = strings.TrimSpace(mod[:i])
		}
		mod = strings.Trim(mod, "\"")
		return mod, nil
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no module directive in %s/go.mod", dir)
}

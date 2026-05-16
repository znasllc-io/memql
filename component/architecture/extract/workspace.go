package extract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace describes the set of Go modules the extractor will analyze
// in one run. Discovered either from a Go workspace file (go.work) or
// from a single module root.
type Workspace struct {
	// Root is the absolute path the run was invoked from. Used to
	// compute workspace-relative paths in the model.
	Root string

	// Modules lists module directories (absolute paths) in
	// declaration order from go.work, or [Root] for a single-module
	// run.
	Modules []string
}

// DiscoverWorkspace looks for go.work at root and falls back to
// treating root itself as a single module. The path is stat'd; an
// invalid root is an error rather than a silent fallback so the user
// gets a clear diagnostic.
func DiscoverWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", abs)
	}
	ws := &Workspace{Root: abs}
	workFile := filepath.Join(abs, "go.work")
	if _, err := os.Stat(workFile); err == nil {
		mods, err := parseGoWork(workFile)
		if err != nil {
			return nil, fmt.Errorf("parse go.work: %w", err)
		}
		for _, rel := range mods {
			ws.Modules = append(ws.Modules, filepath.Clean(filepath.Join(abs, rel)))
		}
		return ws, nil
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
		ws.Modules = []string{abs}
		return ws, nil
	}
	return nil, fmt.Errorf("no go.work or go.mod at %s", abs)
}

// parseGoWork extracts module directories from the use(...) block of
// a go.work file. We don't take a dependency on golang.org/x/mod just
// for this -- the file format is small, line-oriented, and a regex
// would obscure intent. Returns paths as written (relative to go.work).
func parseGoWork(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mods []string
	inBlock := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if !inBlock {
			switch {
			case strings.HasPrefix(line, "use ("):
				inBlock = true
			case strings.HasPrefix(line, "use "):
				// single-line: use ./foo
				p := strings.TrimSpace(strings.TrimPrefix(line, "use "))
				if p != "" {
					mods = append(mods, p)
				}
			}
			continue
		}
		if strings.HasPrefix(line, ")") {
			inBlock = false
			continue
		}
		mods = append(mods, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mods, nil
}

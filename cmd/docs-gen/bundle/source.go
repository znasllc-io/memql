package bundle

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// PublicDir is the one tree the bundle reads, relative to the repository root.
const PublicDir = "docs/public"

// ConceptCatalogPath is where the concept catalog sits in the bundle, relative
// to PublicDir. It is rendered in process and never written into docs/public.
const ConceptCatalogPath = "reference/concepts.md"

// SourceFile is one tracked file under docs/public.
type SourceFile struct {
	// Path is relative to docs/public, slash-separated.
	Path string
	// Content is the file's bytes for a markdown file, nil for any other: a
	// non-markdown file matters only as a link target that exists.
	Content []byte
}

// GeneratedPage is a page rendered in process at bundle time, such as the
// concept catalog. It is selected and link-checked like an authored page.
type GeneratedPage struct {
	Path    string // relative to docs/public
	Content string // the whole page, front matter included
}

// LoadTree reads the files git tracks under docs/public in the repository at
// root. Tracked, never walked: a filesystem walk ships whatever untracked
// scratch file happens to sit in the checkout.
func LoadTree(root string) ([]SourceFile, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", PublicDir)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files %s: %w", PublicDir, err)
	}
	var files []SourceFile
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		inPublic := strings.TrimPrefix(rel, PublicDir+"/")
		f := SourceFile{Path: inPublic}
		if path.Ext(rel) == ".md" {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				if os.IsNotExist(err) {
					// Tracked but deleted in the working tree: not in this build.
					continue
				}
				return nil, err
			}
			f.Content = data
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("git tracks no files under %s in %s", PublicDir, root)
	}
	return files, nil
}

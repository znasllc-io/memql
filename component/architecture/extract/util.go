package extract

import "path/filepath"

// baseName returns the trailing element of path. Wrapper kept so the
// extractor's helpers all live in one file; using filepath.Base
// directly inline would scatter import lines across the package.
func baseName(path string) string { return filepath.Base(path) }

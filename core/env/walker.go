package env

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultVersionPath = "v1"
	sharedDirName      = "shared"
)

var (
	versionPattern = regexp.MustCompile(`^v[0-9]+$`)
	dirPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// WalkerOption configures Walker behavior.
type WalkerOption func(*walkerConfig)

type walkerConfig struct {
	forbidShared bool
}

// ForbidShared returns an option that makes the walker error if a shared directory exists.
func ForbidShared() WalkerOption {
	return func(c *walkerConfig) {
		c.forbidShared = true
	}
}

// Walker resolves directories under a versioned root.
// All subdirectories under the version path are loaded uniformly.
type Walker struct {
	fs          fs.FS
	versionPath string
	dirPaths    map[string]string
	sharedPath  string
}

// NewWalker constructs a walker rooted at versionPath (e.g., v1).
// All subdirectories are discovered and can be loaded via AllPaths().
func NewWalker(root fs.FS, versionPath string, opts ...WalkerOption) (*Walker, error) {
	var cfg walkerConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	if root == nil {
		return nil, fmt.Errorf("walker requires an fs root")
	}
	versionPath = normalizePath(versionPath)
	if versionPath == "" {
		versionPath = defaultVersionPath
	}
	if !versionPattern.MatchString(path.Base(versionPath)) {
		return nil, fmt.Errorf("version directory %q must match v<integer>", versionPath)
	}

	entries, err := fs.ReadDir(root, versionPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", versionPath, err)
	}

	dirPaths := make(map[string]string)
	var sharedPath string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		fullPath := path.Join(versionPath, name)
		if !dirHasContent(root, fullPath) {
			continue
		}
		switch name {
		case sharedDirName:
			if cfg.forbidShared {
				return nil, fmt.Errorf("%s/%s directory is not allowed", versionPath, sharedDirName)
			}
			sharedPath = fullPath
		default:
			if !dirPattern.MatchString(name) {
				return nil, fmt.Errorf("directory %q must be alphanumeric/[-_]", name)
			}
			dirPaths[name] = fullPath
		}
	}

	return &Walker{
		fs:          root,
		versionPath: versionPath,
		dirPaths:    dirPaths,
		sharedPath:  sharedPath,
	}, nil
}

// Directories returns sorted directory names (excluding shared).
func (w *Walker) Directories() []string {
	names := make([]string, 0, len(w.dirPaths))
	for name := range w.dirPaths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AllPaths returns all directory paths that should be loaded (shared + all directories).
func (w *Walker) AllPaths() []string {
	if w == nil {
		return nil
	}
	paths := make([]string, 0, len(w.dirPaths)+1)
	if w.sharedPath != "" {
		paths = append(paths, w.sharedPath)
	}
	// Add directories in sorted order for deterministic loading
	for _, name := range w.Directories() {
		paths = append(paths, w.dirPaths[name])
	}
	return paths
}

// VersionPath returns the normalized version root.
func (w *Walker) VersionPath() string {
	if w == nil {
		return ""
	}
	return w.versionPath
}

func normalizePath(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

func dirHasContent(root fs.FS, dir string) bool {
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		fullPath := path.Join(dir, name)
		if entry.IsDir() {
			if dirHasContent(root, fullPath) {
				return true
			}
			continue
		}
		return true
	}
	return false
}

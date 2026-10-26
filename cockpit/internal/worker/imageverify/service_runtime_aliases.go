package imageverify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type serviceAliasFiles interface{ Readlink(string) (string, error) }

func (installedServiceFiles) Readlink(p string) (string, error) { return os.Readlink(p) }

// Loader/cache paths can contain SONAME or directory aliases. Pin the exact
// operator-observed link chain as well as its final regular files: retaining an
// old library's hash alone does not detect a link retargeted to different code.
// This compares declared identities; it never runs an ELF dependency resolver.
func validateServiceRuntimeAliases(c ServiceConfig) error {
	if c.RuntimeAliases == nil || len(c.RuntimeAliases) > 256 {
		return errors.New("image service requires its bounded runtime alias identities")
	}
	for p, target := range c.RuntimeAliases {
		if !fixedServicePath(p) || target == "" || len(target) > 1024 || filepath.Clean(target) != target || !servicePath.MatchString("/"+strings.TrimPrefix(target, "/")) || serviceWithin(p, c.AttemptRoot) || serviceWithin(p, c.WorkspaceRoot) || strings.HasPrefix(p, "/home/") || strings.HasPrefix(p, "/run/") || strings.HasPrefix(p, "/tmp/") || strings.HasPrefix(p, "/var/tmp/") {
			return errors.New("invalid immutable runtime alias")
		}
		if _, exists := c.RuntimeFiles[p]; exists {
			return errors.New("runtime alias conflicts with a regular dependency")
		}
		resolved, err := resolveServiceRuntimePath(p, c.RuntimeAliases)
		if err != nil {
			return err
		}
		covered := false
		for file := range c.RuntimeFiles {
			if serviceWithin(file, resolved) {
				covered = true
				break
			}
		}
		if !covered {
			return errors.New("runtime alias does not reach a declared dependency")
		}
	}
	for p := range c.RuntimeFiles {
		resolved, err := resolveServiceRuntimePath(p, c.RuntimeAliases)
		if err != nil || resolved != p {
			return errors.New("runtime dependency must name its canonical regular target")
		}
	}
	return nil
}

func resolveServiceRuntimePath(p string, aliases map[string]string) (string, error) {
	for hop := 0; hop < 32; hop++ {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		prefix := ""
		changed := false
		for i, part := range parts {
			prefix += "/" + part
			target, ok := aliases[prefix]
			if !ok {
				continue
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(prefix), target)
			}
			p = filepath.Clean(filepath.Join(append([]string{target}, parts[i+1:]...)...))
			changed = true
			break
		}
		if !changed {
			return p, nil
		}
	}
	return "", errors.New("runtime alias chain is cyclic or exceeds its bound")
}

// Canonical parents can themselves be aliases (for example /lib64). Every
// such link must be explicitly captured, root-owned and unchanged. Other
// parents are immutable directories. The final real targets are hashed by the
// ordinary immutable-file inspector after these link identities pass.
func inspectServiceRuntimeAliases(fs serviceFiles, c ServiceConfig) error {
	reader, ok := fs.(serviceAliasFiles)
	if len(c.RuntimeAliases) > 0 && !ok {
		return errors.New("runtime alias inspection is unavailable")
	}
	for p := range c.RuntimeAliases {
		for current := p; current != "/"; current = filepath.Dir(current) {
			info, err := fs.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				expected, declared := c.RuntimeAliases[current]
				uid, owned := serviceFileUID(info)
				if !declared || !owned || uid != 0 {
					return errors.New("runtime alias is foreign or undeclared")
				}
				actual, err := reader.Readlink(current)
				if err != nil || actual != expected {
					return errors.New("runtime alias identity changed")
				}
			} else if current == p || !info.IsDir() || !serviceRootOwned(info) {
				return errors.New("runtime alias parent is not immutable")
			}
		}
		// Resolve from the captured graph only; a loader alias cannot escape to a
		// different final file merely because its original target still exists.
		resolved, err := resolveServiceRuntimePath(p, c.RuntimeAliases)
		if err != nil {
			return err
		}
		info, err := fs.Lstat(resolved)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !serviceRootOwned(info) {
			return errors.New("runtime alias target is not immutable")
		}
	}
	return nil
}

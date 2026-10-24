package dsl

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/znasllc-io/memql/core/dslfs"
)

// CandidateTree snapshots the registered domains and replaces product domains
// only in the returned view. Unlike MountOverlayDomains, an already installed
// domain must be read from the candidate: that is the ordinary package-update
// case. No registration is changed, even while a reader walks the candidate.
// Embedded core domains remain reserved and are returned separately.
func CandidateTree(root fs.FS) (tree fs.FS, domains, reserved []string, err error) {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("candidate DSL root: %w", err)
	}
	pluginTreesMu.RLock()
	overlays := make(map[string]fs.FS, len(pluginTrees))
	compiled := map[string]bool{}
	for domain, registered := range pluginTrees {
		overlays[domain] = registered
		compiled[domain] = !runtimeTrees[domain]
	}
	pluginTreesMu.RUnlock()
	for _, entry := range entries {
		domain := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(domain, "_") || strings.HasPrefix(domain, ".") {
			continue
		}
		sub, subErr := fs.Sub(root, domain)
		if subErr != nil {
			return nil, nil, nil, fmt.Errorf("candidate domain %s: %w", domain, subErr)
		}
		files, walkErr := dslfs.WalkMemqlFiles(sub)
		if walkErr != nil {
			return nil, nil, nil, fmt.Errorf("candidate domain %s: %w", domain, walkErr)
		}
		if len(files) == 0 {
			continue
		}
		if IsCoreDomain(domain) {
			reserved = append(reserved, domain)
			continue
		}
		if compiled[domain] {
			return nil, nil, nil, fmt.Errorf("candidate domain %q is owned by a compiled-in pack and cannot be replaced by runtime DSL", domain)
		}
		if err := ValidatePackDomain(domain, nil, nil); err != nil {
			return nil, nil, nil, err
		}
		overlays[domain] = sub
		domains = append(domains, domain)
	}
	return &layeredFS{base: embedFS, overlays: overlays}, domains, reserved, nil
}

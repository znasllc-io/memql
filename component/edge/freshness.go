package edge

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// A name that only LOOKS hashed is not enough: dates, release numbers and
// fixed build IDs can all match. Verify a SHA-256 filename component against
// the actual bytes. Other bundler hash formats safely use revalidation.
var digestComponent = regexp.MustCompile(`(?:^|[.-])([a-fA-F0-9]{12,64})(?:[.-]|$)`)

func contentAddressedAsset(fsys fs.FS, name string) bool {
	base := strings.ToLower(path.Base(name))
	// Entry documents must always be revisited, even if a generator put a
	// digest in the filename. This also covers extensionless HTML resolutions.
	if strings.HasSuffix(base, ".html") || strings.HasSuffix(base, ".htm") {
		return false
	}
	candidates := digestComponent.FindAllStringSubmatch(base, -1)
	if len(candidates) == 0 {
		return false
	}
	tag, ok := contentETag(fsys, name)
	if !ok {
		return false
	}
	digest := strings.Trim(tag, `"`)
	for _, candidate := range candidates {
		if strings.HasPrefix(digest, candidate[1]) {
			return true
		}
	}
	return false
}

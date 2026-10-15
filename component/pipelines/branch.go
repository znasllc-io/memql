package pipelines

import "strings"

// ValidBranchName reports whether name is a plain branch name GitHub accepts,
// checked before any request leaves the cluster: the part of git
// check-ref-format a person can type, plus GitHub's length limit. It never
// accepts a full ref ("refs/heads/main"), a revision expression ("main~1",
// "main@{1}") or surrounding whitespace.
func ValidBranchName(name string) bool {
	if name == "" || len(name) > 255 || name == "@" || strings.HasPrefix(name, "refs/") {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == ' ' || strings.ContainsRune(`~^:?*[\`, r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

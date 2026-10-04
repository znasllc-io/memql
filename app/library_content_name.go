package app

import "strings"

// sanitizeContentName bounds what reaches the Library's name field and the
// object path. Path separators and control characters are removed for the
// upload route's reason: the name is the last segment of the blob path, and a
// content whose "name" carried a slash would write outside its own prefix.
//
// SHARED by the two writers that compose their own createLibraryFile call: an
// app session's recording (appsession_content_store.go, agent only) and a
// pipeline step's log and artifacts (pipelines_library_store.go, on the
// workbench as well as the agent). Untagged for the second, so one rule names
// a file whichever node writes it.
func sanitizeContentName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if cleaned == "" {
		cleaned = "content"
	}
	if len([]rune(cleaned)) > 200 {
		cleaned = string([]rune(cleaned)[:200])
	}
	return cleaned
}

package appsession

import (
	"os"
	"path/filepath"
	"strings"
)

// Browser profiles are runtime state, even when an App chooses an arbitrary
// directory beside its screenshots. Recognize Chromium's on-disk markers
// without reading preferences, cookies or caches. A single coincidental file
// name is insufficient; ordinary evidence beside the profile remains output.
func isBrowserRuntimeDir(path string) bool {
	regular := func(name string) bool {
		info, err := os.Lstat(filepath.Join(path, name))
		return err == nil && info.Mode().IsRegular()
	}
	if !regular("Local State") {
		return false
	}
	if regular("DevToolsActivePort") {
		return true
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && (entry.Name() == "Default" || strings.HasPrefix(entry.Name(), "Profile ")) && regular(filepath.Join(entry.Name(), "Preferences")) {
			return true
		}
	}
	return false
}

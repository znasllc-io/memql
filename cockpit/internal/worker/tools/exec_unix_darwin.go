//go:build darwin

package tools

// macOS has no reliable per-child address-space limit across supported hosts.
// Resource limits from the worker's launch service remain inherited normally.
func shellMemoryLimitKiB(_ int) (int, error) { return 0, nil }

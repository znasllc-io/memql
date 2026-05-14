package metadata

import "sort"

const (
	// MaxKeys is the maximum number of metadata keys per node.
	MaxKeys = 100
	// MaxValueLen is the maximum length of a single metadata value.
	MaxValueLen = 1000
)

// Validate enforces metadata size limits.
// Truncates values exceeding MaxValueLen and drops keys beyond MaxKeys
// (keeps the first MaxKeys keys sorted alphabetically).
func Validate(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}

	// Truncate long values
	for k, v := range m {
		if len(v) > MaxValueLen {
			m[k] = v[:MaxValueLen]
		}
	}

	// Enforce key count limit
	if len(m) > MaxKeys {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		trimmed := make(map[string]string, MaxKeys)
		for i := 0; i < MaxKeys; i++ {
			trimmed[keys[i]] = m[keys[i]]
		}
		return trimmed
	}

	return m
}

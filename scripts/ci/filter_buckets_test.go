// Every native path bucket must name an existing subtree. A misspelled or
// renamed directory otherwise leaves the selected step silently absent.
package ci

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func bucketsRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// bucketLiteralPrefix reduces a glob to the longest leading path segment run
// that contains no glob metacharacter, which is the part that must exist on
// disk. `component/memql/**` -> `component/memql`; `**/*.md` -> "" (skipped).
func bucketLiteralPrefix(pattern string) string {
	pattern = strings.TrimPrefix(pattern, "!")
	var kept []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.ContainsAny(seg, "*?[]{}") {
			break
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/")
}

// TestEveryFilterBucketPathExists is the guard.
func TestEveryFilterBucketPathExists(t *testing.T) {
	root := bucketsRepoRoot(t)

	buckets := nativePipeline(t).Select.Buckets
	if len(buckets) == 0 {
		t.Fatal("native pipeline declares no path buckets")
	}

	for name, patterns := range buckets {
		for _, pattern := range patterns {
			prefix := bucketLiteralPrefix(pattern)
			if prefix == "" {
				continue // a pure glob like `**/*.md` names no fixed path
			}
			// An entirely literal pattern names a FILE, and a file is allowed
			// not to exist yet: the `go` bucket legitimately watches `go.work`
			// and `go.work.sum` before the module split creates them, and that
			// is the correct way to write "react if this appears". Only a
			// pattern with a glob after its prefix (`component/mcp/**`) is
			// asserting that a directory exists to be globbed.
			if prefix == strings.TrimPrefix(pattern, "!") {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, prefix)); err != nil {
				t.Errorf("filter bucket %q names %q, whose literal prefix %q does not exist.\n"+
					"a bucket matching nothing silently omits its selected steps: it "+
					"stays false forever, so its steps stop "+
					"running while the gate stays green (the memql#2972 / memql#3019 shape).",
					name, pattern, prefix)
			}
		}
	}
}

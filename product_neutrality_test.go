package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEngineIsProductNeutral is the enforcement gate for the engine/product
// decoupling (#2428): the engine repo must not mention any downstream
// product -- not in code, not in comments, not in docs. Product-specific
// concepts, slugs, corpora, deploy config, and prose live in the product
// pack repos and reach the engine through the generic registration seams
// (docs/public/operate/downstream-stacks.md + docs/public/build/plugin-sdk.md).
//
// The banned terms are the product names this repo shed; extend the list if
// a future product's name ever leaks in. The only exception is an exact
// historical service label used to remove/migrate an installed worker; no
// whole file is exempt and current configuration must use MemQL labels.
func TestEngineIsProductNeutral(t *testing.T) {
	banned := []string{"copresent", "visionarys"}

	skipDirs := map[string]bool{
		".git":         true,
		"bin":          true,
		"node_modules": true,
	}

	// Police GIT-TRACKED files only: local untracked files (env overrides,
	// scratch notes) are not repo content.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var hits []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || rel == "product_neutrality_test.go" {
			continue // this file names the banned terms by necessity
		}
		// File PATHS are repo content too (a copresent-named doc is a leak
		// even if its body is clean).
		lowerPath := strings.ToLower(rel)
		for _, term := range banned {
			if strings.Contains(lowerPath, term) {
				hits = append(hits, rel+": (path contains banned term)")
			}
		}
		if skipDirs[strings.SplitN(rel, string(filepath.Separator), 2)[0]] {
			continue
		}
		f, err := os.Open(rel)
		if err != nil {
			continue // deleted-in-worktree etc.
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		line := 0
		for scanner.Scan() && len(hits) <= 40 {
			line++
			lower := productNeutralityContent(rel, scanner.Text())
			for _, term := range banned {
				if strings.Contains(lower, term) {
					hits = append(hits, rel+":"+itoa(line)+": "+strings.TrimSpace(scanner.Text()))
				}
			}
		}
		f.Close()
		if len(hits) > 40 {
			break
		}
	}

	if len(hits) > 0 {
		t.Errorf("the engine repo must not mention a product (%v). %d hit(s):\n  %s",
			banned, len(hits), strings.Join(hits, "\n  "))
	}
}

func productNeutralityContent(path, line string) string {
	lower := strings.ToLower(line)
	switch path {
	case "cockpit/scripts/install/uninstall-mac.sh",
		"cockpit/scripts/macos/activate-app.sh",
		"cockpit/scripts/macos/app_package_test.py":
		// Existing installations used this label. Cleanup must identify the
		// exact old job, and the package tests must verify it is stopped.
		// Removing this identifier would leave a second worker running.
		lower = strings.ReplaceAll(lower, "com.visionarys.memql-cockpit-worker", "")
	}
	return lower
}

func TestProductNeutralityLegacyExceptionIsExact(t *testing.T) {
	const oldLabel = "com.visionarys.memql-cockpit-worker"
	const cleanup = "cockpit/scripts/install/uninstall-mac.sh"
	if got := productNeutralityContent(cleanup, oldLabel); got != "" {
		t.Fatalf("historical cleanup identifier must remain usable: %q", got)
	}
	for _, tc := range []struct{ path, text, want string }{
		{"cockpit/deploy/launchd/com.znasllc.memql-cockpit-worker.plist", oldLabel, "visionarys"},
		{cleanup, oldLabel + " CoPresent", "copresent"},
		{cleanup, "com.visionarys.another-service", "visionarys"},
	} {
		if got := productNeutralityContent(tc.path, tc.text); !strings.Contains(got, tc.want) {
			t.Errorf("exception hid unrelated product content: %s: %q", tc.path, tc.text)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

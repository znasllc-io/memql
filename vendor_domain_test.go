package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// znas.io is one company's domain, and a hostname is product (memql#3593).
//
// The engine is meant to carry none, and this name was in eight files across
// deploy/, scripts/ and editors/ -- an operator cloning memQL to learn it got a
// cluster named under a stranger's domain. The local default is
// memql.localhost, an RFC 6761 loopback name that belongs to nobody; anything
// else is the operator's own choice, arriving as --domain.
//
// SAME CAVEAT AS TestEngineIsProductNeutral, which this sits beside: it is a
// banned-names list. It catches this name, not the next one. Do not read a
// passing run as proof that no vendor hostname is baked in anywhere.
//
// SCOPE: the artifacts that BUILD AND ADDRESS a cluster -- the manifests ArgoCD
// renders, the scripts that seed and verify it, and the installer that drives
// them. Those are the places where a hostname is configuration, and where one
// baked in is the bug this issue was opened about.
//
// TWO ROOTS ARE DELIBERATELY OUT.
//
//   - docs/ records what the tree used to do. Prior specs and design documents
//     describe the old default by name, and rewriting history to satisfy a
//     linter would make them lie.
//
//   - component/ carries ~40 mentions, and every one is an EXAMPLE rather than
//     configuration: test fixtures picking a plausible hostname, and doc
//     comments naming the `*.local.<domain>` dev-wildcard shape they are about.
//     None of them decides what any cluster serves. Sweeping them would be a
//     large unrelated edit to tests this change does not otherwise touch, and
//     it would not make one more cluster correct. If they are worth renaming,
//     that is its own task.
func TestNoVendorDomainLiterals(t *testing.T) {
	const banned = "znas.io"

	roots := []string{"deploy/", "scripts/", "editors/"}

	skipDirs := map[string]bool{
		"node_modules": true,
		"dist":         true,
		"dist-test":    true,
	}

	// Police GIT-TRACKED files only, exactly as the neutrality sweep does:
	// untracked local files are not repo content.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var hits []string
	for _, rel := range strings.Split(string(out), "\x00") {
		// Two files name it by necessity: this one, and the overlay render test
		// that asserts the rendered output does NOT contain it.
		if rel == "" || rel == "vendor_domain_test.go" ||
			rel == "deploy/k8s/overlays/local/render_domain_test.go" {
			continue
		}
		var inScope bool
		for _, root := range roots {
			if strings.HasPrefix(rel, root) {
				inScope = true
				break
			}
		}
		if !inScope {
			continue
		}
		var skipped bool
		for dir := range skipDirs {
			if strings.Contains(rel, "/"+dir+"/") {
				skipped = true
				break
			}
		}
		if skipped {
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
			if strings.Contains(strings.ToLower(scanner.Text()), banned) {
				hits = append(hits, rel+":"+itoa(line)+": "+strings.TrimSpace(scanner.Text()))
			}
		}
		f.Close()
		if len(hits) > 40 {
			break
		}
	}

	if len(hits) > 0 {
		t.Errorf("the engine must not name %q -- the domain is a value now, supplied as --domain. %d hit(s):\n  %s",
			banned, len(hits), strings.Join(hits, "\n  "))
	}
}

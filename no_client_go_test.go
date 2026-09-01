package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// TestNoKubernetesClientInTheModuleGraph is epic memql#4805's D2 criterion,
// made checkable: "no client-go anywhere in the engine's module graph".
//
// # Why it needs a gate rather than a promise
//
// Custom domains are the first feature whose engine half creates Kubernetes
// objects, and the obvious way to write it is `k8s.io/client-go`. The design
// refuses that -- an in-engine client-go reconciler would be a second way to
// touch cluster objects beside the GitOps/script path, which is what
// environment-parity review rejects -- and the same refusal was reached
// independently by memql#4257 for deploy-control, on cost grounds: hundreds of
// packages and a permanent upgrade obligation, for typed access to CRDs whose
// types are not in it anyway.
//
// Both features therefore speak plain HTTPS to the API server with the pod's
// own ServiceAccount (component/deploycontrol.ClusterAPI). That is a decision
// somebody will reasonably want to revisit when a third feature needs the API,
// and this test is what makes revisiting it a decision rather than an import.
//
// # What it measures, and what it does not
//
// It reads the MODULE requirements of every module in the workspace, which is
// where a client-go dependency would have to appear -- including as an
// indirect one, since `go mod tidy` records those. It does not scan imports:
// an import of a module nothing requires does not build, so the module graph
// is the earlier and stricter place to look.
func TestNoKubernetesClientInTheModuleGraph(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// The banned prefixes. `k8s.io/client-go` is the one D2 names; the other
	// two are how it arrives -- nothing pulls client-go without them, and a
	// dependency on either is the same decision under a different name.
	banned := []string{
		"k8s.io/client-go",
		"k8s.io/apimachinery",
		"sigs.k8s.io/controller-runtime",
	}

	var modFiles []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// ONE SKIP LIST for every repo walker in this package
		// (memql#3678). A hand-rolled one here would be a second answer to
		// "which directories are not the repository", and the failure mode of
		// the first divergence is a walker that reads a worktree under
		// .claude/ and double-counts everything it finds.
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			modFiles = append(modFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A REACHABLE POSITIVE. An empty offender list is only evidence about the
	// tree if the instrument could have found something, and a walk that
	// matched no go.mod at all would report exactly the same clean result as a
	// workspace with no client-go in it.
	if len(modFiles) < 10 {
		t.Fatalf("found only %d go.mod file(s) under %s -- this workspace has dozens, so the walk "+
			"is not measuring what it claims to and its clean result means nothing", len(modFiles), root)
	}

	offenders := map[string][]string{}
	for _, mf := range modFiles {
		b, rerr := os.ReadFile(mf)
		if rerr != nil {
			continue
		}
		rel, _ := filepath.Rel(root, mf)
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, ban := range banned {
				if strings.HasPrefix(trimmed, ban+" ") || strings.HasPrefix(trimmed, ban+"\t") {
					offenders[rel] = append(offenders[rel], trimmed)
				}
			}
		}
	}

	for mod, lines := range offenders {
		t.Errorf("%s requires a Kubernetes client library:\n    %s\n\n"+
			"Epic memql#4805 (D2) and memql#4257 both decided against one, for two reasons that "+
			"still hold: an in-engine client-go reconciler is a SECOND way to touch cluster "+
			"objects beside the GitOps/script path, and the dependency itself is hundreds of "+
			"packages and a permanent upgrade obligation for typed access to CRDs whose types it "+
			"does not contain. The substrate both features use instead is "+
			"component/deploycontrol.ClusterAPI: plain HTTPS to the API server with the pod's own "+
			"ServiceAccount. If this dependency is genuinely the right answer now, that is a "+
			"design decision -- make it deliberately and delete this test with the reasoning.",
			mod, strings.Join(lines, "\n    "))
	}

	// And the sanity check that the workspace still builds the way this test
	// assumes: `go list -m all` would be the stronger instrument, but it needs
	// the network on a cold module cache. The file walk above is the offline
	// one, and this confirms the toolchain agrees the workspace is coherent.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	t.Logf("scanned %d go.mod file(s) for %s", len(modFiles), strings.Join(banned, ", "))
}

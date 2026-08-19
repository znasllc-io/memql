// livekit_cloud_guard_test.go -- the no-cloud-leak guard (memql#4113).
//
// WHAT IT PROTECTS. The two LiveKit planes are deliberately different: local
// dev runs against a LiveKit *Cloud* project (epic memql#2184 -- the local
// overlay removes the self-hosted livekit-server), while the cloud install
// stays strictly self-hosted (deploy/k8s/base/livekit.yaml + its
// ExternalSecret). That asymmetry is safe only while nothing carries a
// *.livekit.cloud URL, key, or secret across it. The failure mode this stops
// is mundane and quiet: an operator copies a working local patch into the
// cloud overlay, the manifests still render, ArgoCD still reconciles, and a
// production cluster is now pointed at somebody's dev LiveKit project.
//
// WHY IT IS BACK. An equivalent guard shipped as
// scripts/deploy/livekit_cloud_guard_test.go (commit ffc6558b) and was
// deleted wholesale in 992deb41 along with the rest of the product
// deploy/release estate. Two docs pages -- docs/public/operate/telephony.md
// and telephony-local-dev.md -- kept telling readers CI enforced this. It
// did not: `grep -rln "livekit.cloud" --include="*_test.go" .` matched
// nothing anywhere in the repo.
//
// WHY A TEST AND NOT A REVIEW. Same reason as render_cloud_test.go's: a
// leaked hostname breaks no build, fails no render, and reconciles cleanly.
// Nothing surfaces it until someone reads a running cluster's env.
package overlays

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// livekitCloudHost matches a LiveKit Cloud project host in any of the shapes
// one is actually pasted in: a bare host, a wss:// URL, inside quotes, or as
// part of a longer env value. The project label is whatever precedes the
// literal ".livekit.cloud".
var livekitCloudHost = regexp.MustCompile(`[A-Za-z0-9_-]+\.livekit\.cloud`)

// cloudPlaneTrees are the manifest trees a cloud install renders from. The
// LOCAL overlay is deliberately absent: pointing it at LiveKit Cloud is the
// supported local topology, not a leak.
var cloudPlaneTrees = []string{
	"cloud", // deploy/k8s/overlays/cloud -- the one cloud overlay (epic memql#3943)
	"../base",
}

// TestNoLiveKitCloudInCloudPlane fails when a *.livekit.cloud reference lands
// in a manifest the cloud install renders from.
//
// It scans FILE TEXT rather than rendered output on purpose: rendering needs
// kustomize or kubectl on the runner and skips without them (see render()),
// and a guard that silently skips is the thing that let this gap persist. Text
// scanning has no such dependency, and a leak arrives as a literal string in a
// patch either way.
func TestNoLiveKitCloudInCloudPlane(t *testing.T) {
	var (
		hits    []string
		scanned int
		files   int
	)

	for _, tree := range cloudPlaneTrees {
		root := filepath.Clean(tree)
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("cloud-plane tree %q is not readable: %v -- the guard "+
				"cannot be trusted while it cannot see what it claims to check", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The shared skip list (memql#3678). A worktree under
				// .claude/ is a full repo copy, so a walk that does not
				// skip it double-counts and reports another checkout's
				// manifests as this one's.
				if repowalk.SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".env", ".json", ".conf":
			default:
				return nil
			}
			files++
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(body), "\n") {
				scanned++
				trimmed := strings.TrimSpace(line)
				// A comment explaining the split is not a leak. Only a real
				// value can wire a cluster to the wrong plane.
				if strings.HasPrefix(trimmed, "#") {
					continue
				}
				if m := livekitCloudHost.FindString(line); m != "" {
					hits = append(hits, path+":"+itoaLine(i+1)+": "+trimmed)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %q: %v", root, err)
		}
	}

	// Report coverage whether or not anything was found: a pass that does not
	// say what it examined is a claim about the tool, not about the tree.
	t.Logf("scanned %d lines across %d manifest files in %s", scanned, files, strings.Join(cloudPlaneTrees, ", "))
	if files == 0 {
		t.Fatalf("guard examined 0 files -- the trees moved and this test is now inert")
	}

	if len(hits) > 0 {
		t.Errorf("LiveKit Cloud reference(s) in the cloud plane -- local dev uses a "+
			"LiveKit Cloud project, the cloud install is self-hosted "+
			"(deploy/k8s/base/livekit.yaml), and the two must never cross:\n  %s",
			strings.Join(hits, "\n  "))
	}
}

// TestLiveKitCloudDetectorFires is the reachable positive for the guard above.
// A scanner that finds nothing proves nothing until it is shown it could have
// found something -- and the shapes below are the ones a leak actually arrives
// in.
func TestLiveKitCloudDetectorFires(t *testing.T) {
	mustMatch := []string{
		`  LIVEKIT_URL: wss://memql-dev-a1b2c3.livekit.cloud`,
		`      value: "wss://someproject.livekit.cloud"`,
		`    url: my-project.livekit.cloud`,
		`LIVEKIT_URL=wss://proj-9.livekit.cloud`,
	}
	for _, in := range mustMatch {
		if !livekitCloudHost.MatchString(in) {
			t.Errorf("detector missed a LiveKit Cloud reference: %q", in)
		}
	}

	mustNotMatch := []string{
		`  LIVEKIT_URL: ws://livekit:7880`,                  // the self-hosted in-cluster address
		`        - name: livekit`,                           // the self-hosted Deployment
		`  host: livekit.example.com`,                       // an operator's own domain
		`  image: livekit/livekit-server`,                   // the upstream image
		`  note: cloud install is self-hosted, not livekit`, // prose
	}
	for _, in := range mustNotMatch {
		if livekitCloudHost.MatchString(in) {
			t.Errorf("detector false-positived on: %q", in)
		}
	}
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

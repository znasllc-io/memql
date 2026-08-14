// promote_overlay_test.go -- the engine promotion, exercised end to end against
// the REAL overlays in this tree (memql#3769, epic memql#3748 §4.1/§4.3).
//
// WHY THE REAL FILES AND NOT A FIXTURE. The claim under test is "pin the
// production overlay's image digests to the ones staging is running", and the
// two overlays are hand-maintained, heavily commented YAML that a fixture would
// simplify in exactly the places a line-oriented rewriter can go wrong -- a
// comment inside the images: block, an entry carrying `newName:` between its
// name and its digest, an all-zeros digest production pins deliberately. A
// fixture would pass while the shipped promote reformatted the file.
//
// The copies are made into t.TempDir() first: nothing here writes to the tree.
package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// overlayNames are the two environments engine promotion moves a version
// between. Named rather than discovered for the reason the render gate gives:
// a third environment arriving and nobody deciding what it promotes from is
// the failure worth catching.
const (
	sourceOverlay = "staging"
	targetOverlay = "prod"
)

// capabilityEnvelope is the single JSON object a capability script writes to
// stdout (#2221). Only the fields this test reads.
type capabilityEnvelope struct {
	OK      bool `json:"ok"`
	Changed bool `json:"changed"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result struct {
		Repinned int `json:"repinned"`
		Images   int `json:"images"`
	} `json:"result"`
}

// promote runs the script with the two streams kept apart, which is what the
// contract requires of any caller: the envelope is on stdout, the human log is
// on stderr, and merging them is how a caller ends up parsing a log line.
func promote(t *testing.T, args ...string) (env capabilityEnvelope, stderr string, exitCode int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{aksScript(t, "promote-overlay.sh")}, args...)...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running promote-overlay.sh: %v (stderr: %s)", err, errBuf.String())
	}
	line := strings.TrimSpace(outBuf.String())
	if line == "" {
		t.Fatalf("no result envelope on stdout (exit %d, stderr: %s)", exitCode, errBuf.String())
	}
	if uerr := json.Unmarshal([]byte(line), &env); uerr != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", uerr, line)
	}
	return env, errBuf.String(), exitCode
}

// stageOverlays copies the two real overlays into a scratch directory and
// returns its path. The directory layout mirrors deploy/k8s/overlays/ so the
// script's --from/--to take the same shape they take in production use.
func stageOverlays(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{sourceOverlay, targetOverlay} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		src := filepath.Join(repoOverlayDir(t, name), "kustomization.yaml")
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), raw, 0o644); err != nil {
			t.Fatalf("write overlay copy: %v", err)
		}
	}
	return root
}

// repoOverlayDir resolves an overlay directory in this checkout.
func repoOverlayDir(t *testing.T, name string) string {
	t.Helper()
	// aksScript anchors at scripts/deploy; the repo root is two levels up.
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(aksScript(t, "promote-overlay.sh")))),
		"deploy", "k8s", "overlays", name)
}

// digestLine matches one `name: <image>` / `digest: <sha>` pair as the overlay
// writes them. Used to read a file's pin set back out for comparison, from the
// OUTSIDE -- deliberately not by calling the script's own parser, so a bug in
// that parser cannot make this test agree with it.
var (
	reName   = regexp.MustCompile(`(?m)^\s*-?\s*name:\s*(\S+)\s*$`)
	reDigest = regexp.MustCompile(`(?m)^\s*digest:\s*(\S+)\s*$`)
)

// pinsOf reads the images: block's name -> digest map out of a kustomization.
func pinsOf(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)
	idx := strings.Index(body, "\nimages:\n")
	if idx < 0 {
		t.Fatalf("%s has no images: block", path)
	}
	block := body[idx:]
	// Stop at the next column-0 key that is not a comment.
	for _, line := range strings.Split(block, "\n") {
		if line == "" || line == "images:" || strings.HasPrefix(line, " ") ||
			strings.HasPrefix(line, "#") || strings.HasPrefix(line, "\t") {
			continue
		}
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "images:") {
			block = block[:strings.Index(block, "\n"+line)]
			break
		}
	}

	out := map[string]string{}
	var current string
	for _, line := range strings.Split(block, "\n") {
		if m := reName.FindStringSubmatch(line); m != nil {
			// `newName:` also matches `name:` loosely; anchor on the full key.
			if strings.Contains(line, "newName:") {
				continue
			}
			current = m[1]
			continue
		}
		if m := reDigest.FindStringSubmatch(line); m != nil && current != "" {
			out[current] = m[1]
			current = ""
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: read no pins out of the images: block", path)
	}
	return out
}

// TestPromotePinsProductionToWhatStagingRuns is the acceptance criterion:
// "Promote pins production's digests from staging's."
//
// It also holds the SHAPE of the edit, which is the part a rewriter gets wrong
// silently: every digest moves, and nothing else in the file does.
func TestPromotePinsProductionToWhatStagingRuns(t *testing.T) {
	root := stageOverlays(t)
	from := filepath.Join(root, sourceOverlay)
	to := filepath.Join(root, targetOverlay)
	toFile := filepath.Join(to, "kustomization.yaml")

	before, err := os.ReadFile(toFile)
	if err != nil {
		t.Fatalf("read target before: %v", err)
	}
	sourcePins := pinsOf(t, filepath.Join(from, "kustomization.yaml"))

	env, stderr, code := promote(t, "--from="+from, "--to="+to, "--version=9.9.9", "--dryRun=false")
	if code != 0 || !env.OK {
		t.Fatalf("promote failed: exit %d, envelope %+v\n%s", code, env, stderr)
	}
	if !env.Changed {
		t.Fatalf("promote reported changed=false against two overlays whose digests differ")
	}

	targetPins := pinsOf(t, toFile)
	for name, want := range sourcePins {
		if got := targetPins[name]; got != want {
			t.Errorf("after promote, %s is pinned to %s; staging runs %s", name, got, want)
		}
	}
	if len(targetPins) != len(sourcePins) {
		t.Errorf("promote changed the SIZE of the image set: %d pins before, %d after -- a promote moves digests, never the set",
			len(sourcePins), len(targetPins))
	}

	// The other half of the claim, and the one that makes "trained constructs do
	// not cross" mechanically true rather than merely stated: the promote's whole
	// effect on this tree is digest lines. Namespace, replica counts, the
	// environment ConfigMap reference, every comment -- untouched. A promote that
	// could carry state would have to write something else, somewhere.
	after, err := os.ReadFile(toFile)
	if err != nil {
		t.Fatalf("read target after: %v", err)
	}
	assertOnlyDigestLinesChanged(t, string(before), string(after))
}

// assertOnlyDigestLinesChanged fails when the two revisions differ anywhere
// except in the value of a `digest:` line, or in their line count.
func assertOnlyDigestLinesChanged(t *testing.T, before, after string) {
	t.Helper()
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	if len(a) != len(b) {
		t.Fatalf("promote changed the target's line count (%d -> %d) -- it must rewrite digest values in place, not reformat the file",
			len(b), len(a))
	}
	for i := range b {
		if b[i] == a[i] {
			continue
		}
		if reDigest.MatchString(b[i]) && reDigest.MatchString(a[i]) {
			continue
		}
		t.Errorf("promote changed a line that is not a digest, at line %d:\n  before: %s\n  after:  %s\n"+
			"A promote moves image digests and nothing else -- the namespace, the replica counts and the "+
			"environment ConfigMap are the VALUES that make the two overlays two environments.",
			i+1, b[i], a[i])
	}
}

// TestPromoteIsIdempotent -- promoting the same digests twice is a no-op that
// reports changed=false, which is what stops the caller manufacturing an empty
// commit (executor.go's RunPromote branches on exactly this).
func TestPromoteIsIdempotent(t *testing.T) {
	root := stageOverlays(t)
	from := filepath.Join(root, sourceOverlay)
	to := filepath.Join(root, targetOverlay)

	if env, stderr, code := promote(t, "--from="+from, "--to="+to, "--dryRun=false"); code != 0 || !env.Changed {
		t.Fatalf("first promote should change the target: exit %d, %+v\n%s", code, env, stderr)
	}
	env, stderr, code := promote(t, "--from="+from, "--to="+to, "--dryRun=false")
	if code != 0 || !env.OK {
		t.Fatalf("second promote failed: exit %d, %+v\n%s", code, env, stderr)
	}
	if env.Changed {
		t.Errorf("promoting the same digests twice reported changed=true; the second run has nothing to do")
	}
	if env.Result.Repinned != 0 {
		t.Errorf("second promote reported repinned=%d, want 0", env.Result.Repinned)
	}
}

// TestPromoteDryRunTouchesNothing -- the default. A capability script's dry-run
// is the no-op surface a caller replays locally, so it must not be the sort of
// dry-run that writes and then reports it did not.
func TestPromoteDryRunTouchesNothing(t *testing.T) {
	root := stageOverlays(t)
	to := filepath.Join(root, targetOverlay)
	toFile := filepath.Join(to, "kustomization.yaml")

	before, err := os.ReadFile(toFile)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	// No --dryRun at all: the default must be the safe one.
	if env, stderr, code := promote(t, "--from="+filepath.Join(root, sourceOverlay), "--to="+to); code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v\n%s", code, env, stderr)
	}
	after, err := os.ReadFile(toFile)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the default (dry) run edited the target overlay")
	}
}

// TestPromoteRefusesRatherThanGuesses covers the three shapes where a promote
// has no mechanical answer. Each is exit 3 (refused), not a silent skip: an
// asymmetric image set is a review decision -- production pins memql-mcp closed
// with an all-zeros digest on purpose -- and a floating tag in the source would
// pin production to a moving reference.
func TestPromoteRefusesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		wantMsg string
	}{
		{
			name: "source pins an image the target does not",
			mutate: func(t *testing.T, root string) {
				appendImage(t, filepath.Join(root, sourceOverlay, "kustomization.yaml"),
					"acrmemql.azurecr.io/memql-newnode", strings.Repeat("a", 64))
			},
			wantMsg: "memql-newnode",
		},
		{
			name: "target pins an image the source does not",
			mutate: func(t *testing.T, root string) {
				appendImage(t, filepath.Join(root, targetOverlay, "kustomization.yaml"),
					"acrmemql.azurecr.io/memql-oldnode", strings.Repeat("b", 64))
			},
			wantMsg: "memql-oldnode",
		},
		{
			name: "source carries a floating tag instead of a digest",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, sourceOverlay, "kustomization.yaml")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				body := reDigest.ReplaceAllString(string(raw), "    digest: latest")
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantMsg: "not a sha256 digest",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := stageOverlays(t)
			tc.mutate(t, root)
			toFile := filepath.Join(root, targetOverlay, "kustomization.yaml")
			before, err := os.ReadFile(toFile)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}

			env, stderr, code := promote(t,
				"--from="+filepath.Join(root, sourceOverlay),
				"--to="+filepath.Join(root, targetOverlay),
				"--dryRun=false")
			if code != 3 {
				t.Fatalf("want exit 3 (refused), got %d\nenvelope %+v\n%s", code, env, stderr)
			}
			if env.OK || env.Error == nil {
				t.Fatalf("a refusal must still emit a failure envelope, got %+v", env)
			}
			if !strings.Contains(env.Error.Message, tc.wantMsg) {
				t.Errorf("refusal message does not name the problem (%q):\n  %s", tc.wantMsg, env.Error.Message)
			}
			after, err := os.ReadFile(toFile)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if string(before) != string(after) {
				t.Error("a refused promote edited the target overlay anyway")
			}
		})
	}
}

// TestPromoteRefusesToPromoteAnOverlayIntoItself -- a bad param, not a refusal:
// the caller named one environment twice, which no amount of cluster state
// makes meaningful.
func TestPromoteRefusesToPromoteAnOverlayIntoItself(t *testing.T) {
	root := stageOverlays(t)
	dir := filepath.Join(root, targetOverlay)
	env, stderr, code := promote(t, "--from="+dir, "--to="+dir, "--dryRun=false")
	if code != 2 {
		t.Fatalf("want exit 2 (bad param), got %d\nenvelope %+v\n%s", code, env, stderr)
	}
}

// appendImage adds a name/digest pair to the end of an overlay's images: block.
// The block runs to the end of the file in staging and is followed by
// `components:` in prod, so the entry is inserted after the last digest line
// rather than appended blindly.
func appendImage(t *testing.T, path, name, hexDigest string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	last := -1
	for i, line := range lines {
		if reDigest.MatchString(line) {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("%s has no digest lines to insert after", path)
	}
	entry := []string{"  - name: " + name, "    digest: sha256:" + hexDigest}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, entry...)
	out = append(out, lines[last+1:]...)
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

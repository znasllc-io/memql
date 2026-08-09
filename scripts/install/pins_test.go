// Pin integrity gate for the local-cluster installer (znasllc-io/memql#3358).
//
// The installer never runs `curl | bash`. It downloads exactly the artifacts
// named in scripts/install/tool-pins.env and refuses anything whose sha256 does
// not match the committed digest. That guarantee is only worth something if the
// committed pins are COMPLETE: a version without a digest looks deliberate while
// verifying nothing, which is strictly worse than no pin at all.
//
// This test validates the COMMITTED tool-pins.env and never touches the
// network. Regenerating the file is a separate, deliberate act:
//
//	scripts/install/refresh-tool-pins.sh            # latest upstream releases
//	scripts/install/refresh-tool-pins.sh --k3d-version=v5.8.3
package install

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinnedTools is the tool graph the local-cluster installer downloads. Every
// one of them must carry a full pin triple. Linux/amd64 only -- this epic is
// Linux-only by design.
var pinnedTools = []string{"K3D", "KUBECTL", "MKCERT"}

var (
	reSemver = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.\-+]+)?$`)
	reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rePinKey = regexp.MustCompile(`^([A-Z0-9]+)_(VERSION|URL|SHA256)$`)
)

// sha256OfNothing is the digest of the empty byte string. A pin carrying it
// means the refresh script recorded a failed/empty download.
const sha256OfNothing = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func pinsPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "tool-pins.env")
}

// parsePins reads a KEY=VALUE env file, ignoring blank lines and # comments.
func parsePins(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v -- regenerate it with scripts/install/refresh-tool-pins.sh", path, err)
	}
	pins := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s:%d: not a KEY=VALUE line: %q", path, i+1, line)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if _, dup := pins[k]; dup {
			t.Errorf("%s: duplicate key %s -- the pins file must have one entry per key", path, k)
		}
		pins[k] = v
	}
	return pins
}

// TestEveryToolHasVersionAndDigest is THE assertion of #3358: a tool is pinned
// only when it has BOTH a semver version and a 64-hex sha256 (plus the https
// URL those two describe). A partial pin fails here.
func TestEveryToolHasVersionAndDigest(t *testing.T) {
	pins := parsePins(t, pinsPath(t))

	for _, tool := range pinnedTools {
		t.Run(tool, func(t *testing.T) {
			version, ok := pins[tool+"_VERSION"]
			if !ok || version == "" {
				t.Fatalf("%s_VERSION is missing or empty", tool)
			}
			if !reSemver.MatchString(version) {
				t.Errorf("%s_VERSION = %q is not a semver version", tool, version)
			}

			url, ok := pins[tool+"_URL"]
			if !ok || url == "" {
				t.Fatalf("%s_URL is missing or empty", tool)
			}
			if !strings.HasPrefix(url, "https://") {
				t.Errorf("%s_URL = %q must be an https:// URL", tool, url)
			}
			if !strings.Contains(url, strings.TrimPrefix(version, "v")) {
				t.Errorf("%s_URL = %q does not name the pinned version %q -- "+
					"the URL and the version must describe the same artifact", tool, url, version)
			}

			digest, ok := pins[tool+"_SHA256"]
			if !ok || digest == "" {
				t.Fatalf("%s_SHA256 is missing or empty -- a pin without a digest "+
					"looks deliberate while verifying nothing", tool)
			}
			if !reSHA256.MatchString(digest) {
				t.Errorf("%s_SHA256 = %q is not a 64-char lowercase hex sha256", tool, digest)
			}
			if digest == sha256OfNothing {
				t.Errorf("%s_SHA256 is the digest of an EMPTY file -- the refresh "+
					"script recorded a failed download", tool)
			}
			if isPlaceholder(digest) {
				t.Errorf("%s_SHA256 = %q looks like a placeholder, not a real digest", tool, digest)
			}
		})
	}
}

// isPlaceholder catches the "fill this in later" digests: a single repeated
// character (000..., aaa..., fff...) or the obvious 0123456789abcdef ramp.
func isPlaceholder(digest string) bool {
	distinct := map[rune]struct{}{}
	for _, r := range digest {
		distinct[r] = struct{}{}
	}
	return len(distinct) <= 2
}

// TestPinsFileHasNoStrayKeys keeps tool-pins.env a pure, generated pin table:
// every key belongs to a pinned tool, and no pinned tool is half-declared.
func TestPinsFileHasNoStrayKeys(t *testing.T) {
	pins := parsePins(t, pinsPath(t))
	known := map[string]bool{}
	for _, tool := range pinnedTools {
		known[tool] = true
	}
	for k := range pins {
		m := rePinKey.FindStringSubmatch(k)
		if m == nil {
			t.Errorf("stray key %q -- expected <TOOL>_VERSION / <TOOL>_URL / <TOOL>_SHA256", k)
			continue
		}
		if !known[m[1]] {
			t.Errorf("key %q names tool %q which is not in the pinned tool graph %v", k, m[1], pinnedTools)
		}
	}
	if want, got := len(pinnedTools)*3, len(pins); got != want {
		t.Errorf("tool-pins.env has %d keys, want %d (3 per tool x %d tools)", got, want, len(pinnedTools))
	}
}

// TestRefreshScriptNeverPipesToShell is the structural half of "no curl|bash
// anywhere in the installer": the generator that produces the pins must not
// itself execute a downloaded stream.
func TestRefreshScriptNeverPipesToShell(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	rePipeToShell := regexp.MustCompile(`(curl|wget)[^|\n]*\|\s*(sudo\s+)?(ba)?sh\b`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(wd, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if loc := rePipeToShell.FindString(stripShellComments(string(b))); loc != "" {
			t.Errorf("%s pipes a download straight into a shell (%q) -- the installer "+
				"must download to disk, verify the sha256, then execute", e.Name(), loc)
		}
	}
}

// stripShellComments drops whole-line `#` comments so the pipe-to-shell guard
// scans executable code and not the header prose that explains why the pattern
// is banned. Trailing inline comments are rare in these scripts and keeping the
// stripper line-oriented avoids mangling `#` inside quoted strings.
func stripShellComments(src string) string {
	var kept []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

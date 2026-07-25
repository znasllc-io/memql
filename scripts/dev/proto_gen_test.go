// Contract gate for scripts/dev/proto-gen.sh (znasllc-io/memql#2774).
//
// Both rules here encode a failure that actually happened, so each assertion
// is a regression net rather than style policing.
package dev

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const protoGenScript = "proto-gen.sh"

func protoGenSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(protoGenScript)
	if err != nil {
		t.Fatalf("read %s: %v", protoGenScript, err)
	}
	return string(raw)
}

// TestProtoGen_CheckDoesNotRestoreFromGit is the data-loss net.
//
// `--check` regenerates into the working tree, then puts it back. It used to
// put it back with `git checkout -- <gen paths>`, which restores to HEAD --
// not to what was there before. Running `make proto-gen` and then
// `make proto-gen-check` before committing therefore DELETED the regeneration,
// while the script reported "no drift" and exited 0. The next build failed on
// undefined symbols with no obvious link to the command that caused it.
//
// The restore must come from a backup taken before regenerating, so the check
// leaves the tree exactly as it found it -- committed or not.
func TestProtoGen_CheckDoesNotRestoreFromGit(t *testing.T) {
	src := protoGenSource(t)

	restoreFromGit := regexp.MustCompile(`git checkout\s+--\s+"\$\{GEN_PATHS`)
	if restoreFromGit.MatchString(src) {
		t.Error("proto-gen.sh restores the generated tree with `git checkout -- ${GEN_PATHS[@]}`; " +
			"that resets to HEAD and silently discards an uncommitted regeneration (#2774). " +
			"Restore from a backup taken before regenerating instead.")
	}
	if !strings.Contains(src, "backup") {
		t.Error("proto-gen.sh --check must back the generated trees up before regenerating " +
			"so it can restore exactly what it found")
	}
}

// TestProtoGen_ProtocIsPinned is the churn net.
//
// The plugins were pinned but protoc was not, on the reasoning that the
// generated body is plugin-determined. True -- but the `// protoc vX.Y.Z`
// header comment is not, so a differing protoc rewrote that line in all eight
// generated files. A one-message proto change then produced an eight-file
// diff, and stamp noise was indistinguishable at a glance from a real
// regeneration.
func TestProtoGen_ProtocIsPinned(t *testing.T) {
	src := protoGenSource(t)

	pin := regexp.MustCompile(`readonly PROTOC_VERSION="[0-9]+\.[0-9]+"`)
	if !pin.MatchString(src) {
		t.Error("proto-gen.sh must pin PROTOC_VERSION -- an unpinned protoc rewrites the " +
			"version stamp in every generated file (#2774)")
	}
	if !strings.Contains(src, "resolve_protoc") {
		t.Error("proto-gen.sh must provision the pinned protoc itself (bin/tools/, mirroring " +
			"scripts/identity/build-css.sh) rather than trusting whatever is on PATH")
	}
}

// TestProtoGen_FallsBackToSystemProtoc pins the availability tradeoff.
//
// Pinning protoc introduced a network dependency on GitHub releases. An
// outage there must not block a proto change, so the script degrades to a
// system protoc rather than failing outright. That fallback is safe for the
// GATE specifically because --check diffs with the version stamp ignored, so a
// differing protoc still produces a correct drift verdict -- the only cost is
// cosmetic stamp churn on a plain regenerate, which is why the fallback warns
// loudly instead of degrading silently.
func TestProtoGen_FallsBackToSystemProtoc(t *testing.T) {
	src := protoGenSource(t)

	if !strings.Contains(src, "command -v protoc") {
		t.Error("proto-gen.sh must fall back to a system protoc when the pinned download " +
			"fails; a releases outage should not block the proto lane (#2774)")
	}
	if !strings.Contains(src, "WARNING") {
		t.Error("the protoc fallback must warn -- silently generating with an unpinned " +
			"protoc reintroduces the stamp churn the pin exists to prevent")
	}
	// The stamp-ignore is what makes the fallback safe for the gate; losing it
	// would turn a fallback into false drift.
	if !strings.Contains(src, "STAMP_IGNORE") {
		t.Error("the drift diff must keep ignoring the protoc version stamp, or a fallback " +
			"protoc would report false drift")
	}
}

// TestProtoGen_ScriptIsValidBash keeps a syntax error from reaching CI, where
// the failure surfaces as a confusing generation error rather than a parse
// error.
func TestProtoGen_ScriptIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", protoGenScript).CombinedOutput()
	if err != nil {
		t.Errorf("bash -n %s failed: %v\n%s", protoGenScript, err, out)
	}
}

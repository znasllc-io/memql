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
	// The old prerequisite instruction is what told contributors to install an
	// arbitrary system protoc; provisioning replaces it.
	if strings.Contains(src, "apt-get install -y protobuf-compiler") {
		t.Error("proto-gen.sh still instructs installing a system protobuf-compiler; " +
			"the pinned protoc is provisioned by the script")
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

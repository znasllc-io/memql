//go:build unix

package workbench

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// build_user_unix.go drops the build to a non-root uid.
//
// ===========================================================================
// THE OTHER HALF OF "NO CLUSTER CREDENTIALS IN THE BUILD'S ENVIRONMENT"
// ===========================================================================
// buildEnv constructs the environment the command is GIVEN, which is the half
// a reader assumes is the whole thing. It is not. The engine runs as root in
// the workbench pod, and a child process running as root can read
// /proc/1/environ -- the memql process's own environment, which carries the
// database DSN, the master key, the object-storage connection string and every
// vendor credential the pod holds. A build script that read it would have
// everything, and the constructed environment above would have bought nothing.
//
// So the command runs as uid 10001, which the workbench image creates and
// which owns nothing but the build directory it is handed. /proc/1/environ is
// mode 0400 owned by root; the read is denied by the kernel rather than by a
// convention.
//
// ===========================================================================
// WHAT THIS IS NOT
// ===========================================================================
// It is not a sandbox. A build can still reach the network, still spend the
// pod's CPU, and still read anything world-readable in the image. Those are
// bounded by the pod's own limits, by the timeout, and by the fact that the
// directory is destroyed when the call returns. The uid closes the one hole
// that would have made the environment claim false.
//
// ===========================================================================
// A PROCESS THAT IS NOT ROOT SKIPS IT, DELIBERATELY
// ===========================================================================
// A developer running the engine as themselves cannot setuid to anything, and
// refusing the build there would make the local path fail for a reason that
// does not apply to it -- their own uid is already not root's. So the drop is
// applied when it CAN be, and its absence is reported by
// buildUserForDirectory's second return so a caller can say which happened.

// BuildUserEnv names the uid the build runs as. An operator knob because an
// image built elsewhere may not have 10001, and a wrong uid is a build that
// cannot write its own directory.
const BuildUserEnv = "MEMQL_PACKAGES_BUILD_UID"

// DefaultBuildUid is the uid the workbench image creates (see the Dockerfile's
// workbench-runtime stage).
const DefaultBuildUid = 10001

// MaxBuildUid bounds what MEMQL_PACKAGES_BUILD_UID may name.
//
// A BOUND rather than a bare parse, and the reason is what the value becomes:
// syscall.Credential takes a uint32, so an `int` past that range does not
// error -- it WRAPS. On a 64-bit node "4294967296" would silently become uid
// 0, which is to say the build would run as root while the configuration
// claimed it did not. 2^31-1 is comfortably above every real uid and safely
// inside uint32.
const MaxBuildUid = 1<<31 - 1

// buildUid resolves the uid to run a build as, or 0 for "do not drop".
func buildUid() int {
	raw := strings.TrimSpace(os.Getenv(BuildUserEnv))
	if raw == "" {
		return DefaultBuildUid
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > MaxBuildUid {
		return DefaultBuildUid
	}
	// An explicit 0 is an operator saying "run it as me", which is the
	// escape hatch for an image with no build user. It is spelled as a
	// deliberate value rather than as an unset variable, because the
	// dangerous state must never be the one you reach by configuring nothing.
	return n
}

// applyBuildUser points the command at the build uid and gives that uid the
// directory, or reports why it could not.
//
// Returns the uid actually applied and a reason when none was: both are
// logged, so "this build ran as root" is a sentence in the record rather than
// something to infer from its absence.
func applyBuildUser(cmd *exec.Cmd, dir string) (int, string) {
	uid := buildUid()
	if uid == 0 {
		return 0, "MEMQL_PACKAGES_BUILD_UID is 0, so the build runs as this process"
	}
	if os.Geteuid() != 0 {
		// Not root, so setuid is not available -- and not needed: this
		// process is already not the one whose environment is worth taking.
		return 0, "this process is not root, so the build runs as it and cannot change user"
	}
	if err := chownTree(dir, uid); err != nil {
		return 0, "the build directory could not be given to the build user (" + err.Error() + "), so the build runs as this process"
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Converted ONCE, after buildUid has bounded it to [0, MaxBuildUid] --
	// which is what makes the narrowing to uint32 safe rather than wrapping.
	id := uint32(uid) //nolint:gosec // bounded by buildUid above
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: id,
		Gid: id,
		// NO SUPPLEMENTARY GROUPS. Without this the child inherits root's,
		// which on some images includes groups with read access to mounted
		// secrets -- the exact thing the uid is here to put out of reach.
		NoSetGroups: false,
		Groups:      []uint32{id},
	}
	return uid, ""
}

// chownTree gives every path under root to uid.
//
// Walked rather than done at creation because the tree arrives from an
// archive: the extractor writes it as this process, and a build that cannot
// write into its own source directory fails in a way that looks like a broken
// build script.
func chownTree(root string, uid int) error {
	return filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		return os.Lchown(p, uid, uid)
	})
}

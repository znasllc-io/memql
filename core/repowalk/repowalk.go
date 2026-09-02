// Package repowalk holds the one skip list every test that walks the repository
// tree should share.
//
// It exists because the list was copied, and the copies drifted. Several
// walkers skipped `.git` / `vendor` / `node_modules` but not `.claude` --
// which holds git WORKTREES, full copies of this repo. A walker that descends
// into one sees every source file two or three times.
//
// That is not hypothetical. TestDeclaredMetadataKeysAreReadByNothing counts
// occurrences of a metadata key and asserts exactly one; with two worktrees
// checked out it reported three, named the same file three times, and printed a
// confident instruction to go edit three documents that were not wrong. It
// failed only on machines where somebody happened to have a worktree, so it was
// green in CI and red locally -- the least debuggable shape a test failure has.
//
// `filepath.Walk` does not consult `.gitignore`, so being untracked protects
// nothing. The only fix is for the walker to know.
//
// `.superpowers` joined the list for exactly that reason, and it had exactly
// that symptom (memql#4878). It is where the skills write their working prose
// -- design records, audit lanes, brainstorm notes -- and one of them cited a
// `deploy`, a `bootstrap` and a `secrets-seed` make target, none of which
// exist. TestMakeTargetCitationsNameRealTargets read them and failed, with
// twenty findings naming a directory that is gitignored, untracked, and
// absent from every checkout CI has ever made. So `make test` was RED on the
// developer's machine and green on the server, over prose about the
// repository rather than the repository.
package repowalk

// skipped is the set of directory names no repository walk should descend into.
//
// Deliberately small. These five are wrong for EVERY walker: three are not our
// source at all, `.claude` is our source duplicated, and `.superpowers` is
// working prose ABOUT the source that no gate is asking a question of.
// Anything narrower -- `bin`, `gen`, `dist`, `testdata`, `sdk` -- is a
// judgement particular to one test's question, and belongs at that call site
// rather than here, where it would silently narrow every other walker's
// coverage.
var skipped = map[string]bool{
	".git":         true,
	".claude":      true,
	".superpowers": true,
	"vendor":       true,
	"node_modules": true,
}

// SkipDir reports whether a directory with this base name should be skipped.
//
// Use it as the first check in a walk callback, before any test-specific skips:
//
//	if d.IsDir() && repowalk.SkipDir(d.Name()) {
//	    return filepath.SkipDir
//	}
//
// Takes a base name, not a path, so it works identically with filepath.Walk,
// filepath.WalkDir, and fs.WalkDir.
func SkipDir(name string) bool {
	return skipped[name]
}

// SkippedNames returns the skip set, sorted, for tests and error messages that
// need to state what a walk excluded.
func SkippedNames() []string {
	out := make([]string, 0, len(skipped))
	for name := range skipped {
		out = append(out, name)
	}
	// Small fixed set; insertion-sort keeps this dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

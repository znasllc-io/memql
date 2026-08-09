// Static guard: the TS SDK's `UserRoleWire` names every role
// `memql.proto`'s `UserRole` declares (znasllc-io/memql#3331).
//
// # The failure mode
//
// `sdk/ts/src/client/wire.ts` says of itself, in its header, that it is "the
// hand-mirrored TS view of the subset" and that "the proto remains the source
// of truth". Hand-mirroring is a deliberate choice -- bundling a proto runtime
// in the browser is not worth the bytes -- but it has no mechanism. A value
// added to an enum in the proto does not appear in the union, and nothing says
// so.
//
// USER_ROLE_DEVELOPER was added to the proto for the cockpit's deploy-control
// gating (#1886) and never reached the union. The consequence was not a
// compile error or a runtime throw. `roleFromWire` ends in `?? ""`, so a
// developer resolved to the SAME value as an unauthenticated caller, and every
// consumer that branched on the role branched wrong: the VS Code deploy panel
// could not gate cut/deploy, so it offered all five actions behind a hedge --
// three of which the engine was certain to refuse.
//
// A silent downgrade of an authorization input is the worst shape this can
// take, which is why the guard is on the ENUM rather than on the one role that
// went missing.
//
// # Why this is a Go test over two text files
//
// It has to read `component/grpc/memql.proto`, which no TS test in
// `sdk/ts/test/` can reach: the SDK is a publishable package and its tests run
// from its own directory, with the proto outside the package root. `scripts/ci`
// is where this repo already keeps guards that read across the tree
// (proto_bucket_coverage_test.go, dockerfile_module_manifests_test.go), so it
// goes here.
//
// It parses rather than imports for the same reason: the generated Go enum
// (`memqlv1.UserRole`) would answer the proto half, but the TS union is text
// either way, and reading BOTH sides the same way keeps the comparison
// honest -- a mismatch is then about the two files, not about whether
// generation had been run.
//
// # What it does NOT check
//
// That each wire value maps to a sensible `Role`. TypeScript already enforces
// that half: `userRoleFromWire` is typed `Record<UserRoleWire, Role>`, so
// widening the union without adding an entry is a build error. The values
// themselves are pinned by sdk/ts/test/role.test.ts. This guard covers the one
// gap neither can see -- the proto growing a value the union never learned
// about.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRootForUserRoleParity walks up from this file to the module root.
//
// Anchored on runtime.Caller rather than os.Getwd so it resolves the same way
// under `go test ./...` from any directory, and lands inside whichever
// checkout this source belongs to -- a git worktree under .claude/worktrees/
// finds its OWN proto and its OWN wire.ts, never the primary checkout's
// (the memql#3346 lesson, arrived at from the other direction: that guard had
// to stop walking INTO nested checkouts; this one must stay inside its own).
func repoRootForUserRoleParity(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.work")
		}
		dir = parent
	}
}

func readFileForParity(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// protoUserRoleValues extracts the value names from `enum UserRole { ... }`.
//
// Scoped to that block rather than grepping the file for `USER_ROLE_`, so a
// mention in a comment elsewhere in the proto cannot invent a value. Comment
// lines inside the block are stripped for the same reason -- the enum's own
// USER_ROLE_DEVELOPER comment names the symbol three lines before declaring it.
func protoUserRoleValues(t *testing.T, proto string) []string {
	t.Helper()

	const marker = "enum UserRole {"
	start := strings.Index(proto, marker)
	if start < 0 {
		t.Fatalf("`%s` not found in memql.proto -- the enum was renamed or removed. "+
			"This guard is now measuring nothing; retarget it rather than deleting it.", marker)
	}
	rest := proto[start+len(marker):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatal("unterminated `enum UserRole` block in memql.proto")
	}
	body := rest[:end]

	valueRe := regexp.MustCompile(`^\s*(USER_ROLE_[A-Z0-9_]+)\s*=\s*\d+\s*;`)
	var values []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := valueRe.FindStringSubmatch(line); m != nil {
			values = append(values, m[1])
		}
	}
	if len(values) == 0 {
		t.Fatal("parsed zero values out of `enum UserRole` -- the parse broke, and an " +
			"empty expectation would make this guard pass vacuously")
	}
	sort.Strings(values)
	return values
}

// wireUserRoleValues extracts the string members of `export type UserRoleWire`.
func wireUserRoleValues(t *testing.T, wireTS string) []string {
	t.Helper()

	const marker = "export type UserRoleWire ="
	start := strings.Index(wireTS, marker)
	if start < 0 {
		t.Fatalf("`%s` not found in sdk/ts/src/client/wire.ts -- the union was renamed or "+
			"removed. Retarget this guard rather than deleting it.", marker)
	}
	rest := wireTS[start+len(marker):]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatal("unterminated `export type UserRoleWire` declaration in wire.ts")
	}
	body := rest[:end]

	memberRe := regexp.MustCompile(`"(USER_ROLE_[A-Z0-9_]+)"`)
	var values []string
	for _, m := range memberRe.FindAllStringSubmatch(body, -1) {
		values = append(values, m[1])
	}
	if len(values) == 0 {
		t.Fatal("parsed zero members out of `UserRoleWire` -- the parse broke, and an empty " +
			"actual would make every proto value look missing")
	}
	sort.Strings(values)
	return values
}

func TestUserRoleWireCoversEveryProtoRole(t *testing.T) {
	root := repoRootForUserRoleParity(t)

	proto := readFileForParity(t, filepath.Join(root, "component", "grpc", "memql.proto"))
	wireTS := readFileForParity(t, filepath.Join(root, "sdk", "ts", "src", "client", "wire.ts"))

	protoValues := protoUserRoleValues(t, proto)
	wireValues := wireUserRoleValues(t, wireTS)

	inWire := make(map[string]bool, len(wireValues))
	for _, v := range wireValues {
		inWire[v] = true
	}
	inProto := make(map[string]bool, len(protoValues))
	for _, v := range protoValues {
		inProto[v] = true
	}

	var missing []string
	for _, v := range protoValues {
		if !inWire[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		t.Errorf(`memql.proto declares %d role(s) that sdk/ts's UserRoleWire does not name:

  %s

wire.ts is HAND-MIRRORED from the proto, so this does not fix itself. And it
does not fail loudly either: roleFromWire ends in `+"`?? \"\"`"+`, so an unlisted role
resolves to the SAME value as an unauthenticated caller. Every consumer that
branches on the role then branches wrong -- memql#3331 was exactly this, and
it left the VS Code deploy panel unable to tell a developer from an unknown.

To fix, in sdk/ts/src/client/:
  1. add the value to `+"`UserRoleWire`"+` in wire.ts;
  2. map it in `+"`userRoleFromWire`"+` in types.ts -- the compiler REQUIRES this,
     since the record is typed Record<UserRoleWire, Role>;
  3. add the corresponding member to `+"`Role`"+` in types.ts if it is a real role;
  4. adjudicate it anywhere a role gates behaviour. editors/vscode/src/deploy/
     actions.ts (satisfiesTier) is the one that exists today.`,
			len(missing), strings.Join(missing, "\n  "))
	}

	var extra []string
	for _, v := range wireValues {
		if !inProto[v] {
			extra = append(extra, v)
		}
	}
	if len(extra) > 0 {
		t.Errorf(`sdk/ts's UserRoleWire names %d value(s) memql.proto does not declare:

  %s

A value the server can never send. Harmless at runtime, but it is a claim
about the wire that the wire does not support -- and it makes the union stop
being a readable answer to "what can arrive here". Remove it, or add it to the
proto if it was meant to exist.`,
			len(extra), strings.Join(extra, "\n  "))
	}
}

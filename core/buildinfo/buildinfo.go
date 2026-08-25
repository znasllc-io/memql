// Package buildinfo carries what a running binary cannot rediscover about
// itself: which release it was built from, and which commit.
//
// # Why this exists (memql#3998)
//
// Before this package a MemQL node had no honest answer to "which version are
// you?", and the dishonesty had two halves that reinforced each other:
//
//   - ServerHello.version was the literal "v1". That is the WIRE PROTOCOL
//     version -- a real and useful fact, and not a release -- so a client that
//     read it learned nothing about which engine it was talking to.
//   - resolveServiceVersion read the checked-in VERSION file, which has said
//     0.15.0 at every tag since v0.16.1, and the image build then overwrote it
//     with 0.15.0-<epoch>.
//
// So a v0.18.0 cluster introduced itself as 0.15.0-1754…: release-SHAPED and
// wrong. That is the worst of the three answers available. The right release is
// obviously best; something visibly NOT a release is a distant second but still
// usable, because a client comparing versions can see it has nothing to compare
// and say so. A plausible lie defeats the comparison silently, which is exactly
// how an operator ends up staring at "stream closed" with no hint that their
// cluster is simply older than their plugin.
//
// # The contract
//
// release is set at LINK time, by the build that knows which release it is
// cutting:
//
//	go build -ldflags "-X github.com/znasllc-io/memql/core/buildinfo.release=v0.18.1" .
//
// and by nothing else. There is no environment variable, no file on disk, and
// no runtime setter, because each of those is a way for a node to claim a
// release it was not built from -- and a version source that can be overridden
// at runtime is not a version source, it is a rumour. The image build wires the
// flag from the release tag it was dispatched with (Dockerfile's MEMQL_RELEASE
// build arg, fed by .github/workflows/build-engine-images.yml).
//
// commit is stamped by the same build and follows the same rule; its own
// comment below states the one difference, which is that the Go toolchain can
// supply it for a developer build where there is no release to stamp
// (memql#4486). That fallback reads a table the LINKER wrote, so it is not a
// value a running process can be told either.
//
// EVERY build passes it, not only the release one (memql#4574). The toolchain
// fallback cannot fire inside an image build -- .dockerignore excludes .git --
// so scripts/lib/engine_build_args.sh, the shared mapping behind `make dev` and
// the editor's rebuild-from-checkout, sets MEMQL_COMMIT from the checkout it is
// building. It does NOT set MEMQL_RELEASE, because neither of its callers is
// cutting one.
//
// release is EMPTY in a bare `go build .`, in `make dev`, and in any image
// built off a branch. The emptiness is the point: a binary that was not cut
// from a release must not name one. Version answers a DevVersion-prefixed
// string there, which no release-tag parser accepts, so a client gets "cannot
// compare" instead of a confident wrong answer.
//
// # What this does NOT fix
//
// Only clusters cut AFTER this ships can introduce themselves. It cannot teach
// v0.18.0 -- or anything older -- to state its release, because the binary
// doing the lying is the one already installed, and the fix is in the binary.
// A change here reaches the field one release later, by construction.
//
// That is precisely why version awareness in the VS Code plugin does not rest
// on this alone. The recorded version in clusters.yaml (memql#3990) is written
// at install time, when the installer knows the tag it pulled, and refreshed
// opportunistically from every source that can be trusted -- the install
// receipt, the ArgoCD targetRevision, deploy-control status, and the
// memqlVersion() builtin. This package makes the engine the most trustworthy of
// those sources; it does not make it the only one.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// DevVersion is what a binary that was not cut from a release says about
// itself, and the prefix of what it says when it also knows its commit.
//
// It is deliberately not parseable as a release tag. Any client comparing a
// cluster's version against the newest release must land on "cannot compare"
// for this value rather than on "current" -- see the VersionRelation contract
// in editors/vscode/src/version/compare.ts, which yields "notComparable" for
// anything unparseable and never "current".
const DevVersion = "dev"

// release is stamped at link time. See the package comment for the one command
// that is allowed to set it. Never assign to it from Go: it is a build fact,
// and a build fact with a setter is a runtime value wearing a disguise.
var release string

// Release returns the release tag this binary was built from, or "" when it was
// not built from a release. The empty return is meaningful and callers should
// branch on it rather than passing it on as if it were a version.
func Release() string {
	return strings.TrimSpace(release)
}

// IsRelease reports whether this binary was cut from a release.
func IsRelease() bool {
	return Release() != ""
}

// Version returns what this binary states about itself: the release it was cut
// from, or `dev+<short commit>` when it was not cut from one. It never returns
// empty, and it never returns a release tag the binary was not built from.
//
// This is the single answer for the whole process. main.go's
// resolveServiceVersion returns it (which is what reaches app.RunConfig.Version,
// and from there the engine's memqlVersion() builtin), and component/grpc reads
// it for ServerHello.engine_version, so the two surfaces a client can ask
// cannot disagree.
//
// # Why the commit rides the STRING here, and only for a dev build (memql#4575)
//
// A cluster built from a checkout used to answer the bare word "dev", which is
// honest and useless: a developer who rebuilt an hour ago and one who installed
// last week read the same word, and nothing else on the machine could say which
// source was running. The commit is the answer, and this is where it reaches
// every surface at once -- the portal's rail footer, the editor's cluster row,
// the boot log, memqlVersion() -- because they all already render this one
// string.
//
// The alternative was a second field everywhere, and for the editor that
// alternative does not work. Its recorded version lives in the operator's
// clusters.yaml, and the cockpit rewrites that file from its own struct and
// DROPS every key it does not model (editors/vscode/src/clusters/model.ts
// documents this for `version` itself). A new `commit:` key there would work
// until the operator's next cockpit command silently erased it. `version` is a
// key both tools already model, and model.ts already anticipates what lands
// here: "a release tag, but equally a branch name, a commit sha".
//
// `+` is SemVer build metadata, and `dev+a1b2c3d4e5f6` is still unparseable as
// a release -- editors/vscode/src/version/compare.ts requires `v?X.Y.Z` before
// it will look at anything else. So `notComparable` remains the answer, the
// learners' rule that a non-release-shaped value may not overwrite a recorded
// release still holds, and nothing downstream has to learn a new shape.
//
// A RELEASE build is left alone. A release's version is what the whole
// upgrade comparison rests on -- it is compared, sorted, matched against an
// image tag and rendered in a dozen places -- and appending build metadata to
// it puts the same fact in two places and hands every one of those callers a
// string with a part they must remember to ignore. The release build states
// its commit through ServerHello.engine_commit instead, which is the case that
// needs it most: a tag's image pins are written before that tag's own images
// exist, so an instance declaring ENGINE_REF=v0.19.6 legitimately runs 0.19.5
// binaries, and only the commit can say which source is executing.
func Version() string {
	if r := Release(); r != "" {
		return r
	}
	if c := ShortCommit(); c != "" {
		return DevVersion + "+" + c
	}
	// No release and no commit: an unstamped build outside a VCS checkout.
	// The bare word is the honest answer, not a placeholder.
	return DevVersion
}

// IsDevVersion reports whether a version string is what an uncut build states
// about itself, in either of its two forms.
//
// Exported because "is this a dev build" is a question several surfaces ask and
// a `== "dev"` comparison is now WRONG for the common case -- a stamped local
// build answers `dev+<sha>`. A predicate is what keeps that from being
// rediscovered one caller at a time.
func IsDevVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == DevVersion || strings.HasPrefix(v, DevVersion+"+")
}

// commit is the git revision this binary was built from, stamped at link time
// by the same build that stamps release:
//
//	go build -ldflags "-X github.com/znasllc-io/memql/core/buildinfo.commit=<sha>" .
//
// Why a release tag was not enough (memql#4486). Asked "what version is
// running?", the honest answer required mapping a running image DIGEST back to
// a registry tag, because no node stated anything at boot. Adding the release
// alone would not have fixed it, for a reason specific to how this repository
// cuts releases: an instance declares ENGINE_REF=v0.19.6 and composes
// cloud-entry?ref=v0.19.6, but the BINARIES are 0.19.5 -- a tag's image pins
// are written before that tag's own images exist. Manifests and binaries
// legitimately differ by one release, so the release tag alone still cannot
// tell an operator which SOURCE is executing. The commit can, and during an
// incident that is the difference between reading the right diff and the wrong
// one.
var commit string

// Commit returns the git revision this binary was built from, or "" when it
// cannot be established. The empty return is meaningful -- callers must render
// it as unknown rather than passing it on as if it were a revision.
//
// Two sources, in order, and NEITHER is a runtime input:
//
//  1. the link-time stamp above, which is what an image build sets. A Docker
//     build context carries no .git, so this is the ONLY source that works for
//     a released image -- which is precisely why the stamp exists.
//  2. the toolchain's own VCS stamping (debug.ReadBuildInfo's vcs.revision),
//     which covers `go build .` on a developer machine, where there is no
//     release to stamp and the question is still worth answering.
//
// Source 2 is not a hole in the contract the package comment states. That
// contract forbids a version a RUNNING PROCESS CAN BE TOLD -- an env var, a
// file on disk, a setter. debug.ReadBuildInfo reads a table the linker wrote
// into the binary; there is no way to change what it answers without producing
// a different binary, which is the same property the -X stamp has.
//
// A build from a dirty tree reports its revision with a "-dirty" suffix. The
// suffix is load-bearing: a revision that names a commit whose contents were
// not what was built is the same class of confident-and-wrong answer the
// release half of this package exists to prevent.
func Commit() string {
	if c := strings.TrimSpace(commit); c != "" {
		return c
	}
	return vcsCommit()
}

// vcsCommit reads the revision the Go toolchain stamped at build time. It
// returns "" when the binary was not built from a VCS checkout -- which is the
// normal case inside a container image build.
func vcsCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(s.Value)
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}

// ShortCommit returns Commit() abbreviated to the 12 hex characters git itself
// uses for an unambiguous short revision, preserving any "-dirty" suffix. It
// returns "" exactly when Commit() does.
func ShortCommit() string {
	c := Commit()
	if c == "" {
		return ""
	}
	dirty := strings.HasSuffix(c, "-dirty")
	sha := strings.TrimSuffix(c, "-dirty")
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if dirty {
		return sha + "-dirty"
	}
	return sha
}

// LogAttrs returns the build identity as alternating slog key/value pairs, for
// the one line every node writes at boot (app.newApp).
//
// commit is OMITTED rather than logged empty when it cannot be established. A
// structured field carrying "" is worse than an absent one: a log query
// filtering on commit matches it, and an operator reading the line sees a field
// that looks answered.
func LogAttrs() []any {
	attrs := []any{"version", Version(), "release", IsRelease()}
	if c := ShortCommit(); c != "" {
		attrs = append(attrs, "commit", c)
	}
	return attrs
}

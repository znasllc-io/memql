// Package buildinfo carries the one thing a running binary cannot rediscover
// about itself: which release it was built from.
//
// # Why this exists (memql#3998)
//
// Before this package a memQL node had no honest answer to "which version are
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
// release is EMPTY in a bare `go build .`, in `make dev`, and in any image
// built off a branch. The emptiness is the point: a binary that was not cut
// from a release must not name one. Version answers DevVersion there, which no
// release-tag parser accepts, so a client gets "cannot compare" instead of a
// confident wrong answer.
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

import "strings"

// DevVersion is what a binary that was not cut from a release says about
// itself.
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
// from, or DevVersion when it was not cut from one. It never returns empty, and
// it never returns a release tag the binary was not built from.
//
// This is the single answer for the whole process. main.go's
// resolveServiceVersion returns it (which is what reaches app.RunConfig.Version,
// and from there the engine's memqlVersion() builtin), and component/grpc reads
// it for ServerHello.engine_version, so the two surfaces a client can ask
// cannot disagree.
func Version() string {
	if r := Release(); r != "" {
		return r
	}
	return DevVersion
}

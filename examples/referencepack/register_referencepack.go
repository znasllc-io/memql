//go:build referencepack

// This file demonstrates the REAL build-tag-gated auto-register pattern a
// production pack uses. It carries the ONLY unconditional init() in the pack,
// and it is compiled in ONLY when the `referencepack` build tag is set:
//
//	go build -tags referencepack ./...
//
// The tag is never set in any production binary, so this init() never runs in
// prod and the pack never auto-loads there. A production pack swaps the tag for
// its product tag (e.g. //go:build myproduct) and anchors the package via a
// blank import in the app bootstrap, exactly as the carrier repo does.
//
// Because Register(Domain) lives here (not in pack.go), merely linking the
// referencepack package into a binary does NOT load the pack -- that is the
// whole point of the non-prod strategy: the pack is normal Go that `go build
// ./...` compiles, but loading is opt-in behind a tag (prod) or an explicit
// call (tests).
package referencepack

func init() {
	Register(Domain)
}

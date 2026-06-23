//go:build deploypack

// register_deploypack.go carries the ONLY unconditional init() in the deploy
// pack, compiled in ONLY when the `deploypack` build tag is set:
//
//	go build -tags deploypack ./...
//
// The tag is not set in any default node binary, so this init() never runs
// there and the pack never auto-loads. A deployment that wants the deploy pack
// (the identity node that hosts the Deploy Console, the dogfood lane) builds
// with -tags deploypack and anchors the package via a blank import in the app
// bootstrap, exactly like a production product pack swaps in its own tag.
//
// Because Register(Domain) lives here (not in pack.go), merely linking the
// deploypack package into a binary does NOT load the pack -- loading is opt-in
// behind the tag (or an explicit Register call in tests).
package deploypack

func init() {
	Register(Domain)
}

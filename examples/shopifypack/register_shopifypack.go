//go:build shopifypack

// Build-tag-gated auto-register. The shopifypack tag is never set in
// production binaries; tests call Register explicitly.
package shopifypack

func init() {
	Register(Domain)
}

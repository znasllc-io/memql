package installation

import "github.com/znasllc-io/memql/integrations/argocd"

// Accept only the native opaque render values. Checking the concrete pointer
// forms also rejects a typed nil interface before any value-receiver method
// can panic. A missing render is never an empty, successful inventory.
func validRenderInventory(inventory argocd.RenderInventory) bool {
	switch value := inventory.(type) {
	case argocd.RenderedRevision:
		return internalDigest.MatchString(value.Digest())
	case *argocd.RenderedRevision:
		return value != nil && internalDigest.MatchString(value.Digest())
	case argocd.RenderedSet:
		return internalDigest.MatchString(value.Digest())
	case *argocd.RenderedSet:
		return value != nil && internalDigest.MatchString(value.Digest())
	default:
		return false
	}
}

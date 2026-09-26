package frontdoor

import "strings"

// StorefrontTestingPrefix reserves a sibling under the same wildcard route
// and certificate as the production hostname. Testing shares the site's build;
// it does not create another deployable or another deployment.
const StorefrontTestingPrefix = "test--"

func StorefrontTestingHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	label, domain, ok := strings.Cut(host, ".")
	if !ok || label == "" || domain == "" || len(label)+len(StorefrontTestingPrefix) > 63 || strings.HasPrefix(label, StorefrontTestingPrefix) {
		return ""
	}
	return StorefrontTestingPrefix + host
}

// StorefrontProductionHost reverses only the reserved testing alias. The
// resolver must still require a storefront on the exact canonical hostname;
// custom domains and account front doors cannot acquire aliases this way.
func StorefrontProductionHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	production, ok := strings.CutPrefix(host, StorefrontTestingPrefix)
	return production, ok && StorefrontTestingHost(production) == host
}

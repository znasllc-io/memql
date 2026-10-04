package shopify

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// connectDocPath is the operator page that tells an operator which scopes to
// register on the shared app. Read from the repository checkout, as
// integrations/release reads the pin file it checks.
const connectDocPath = "../../docs/public/operate/shopify-connect.md"

// TestTheConnectDocListsTheScopesTheManagedFlowRequests holds the operator
// page to managedConnectScopes (memql#5638, G7). The page said `read_products`
// for a month after the code started asking for `write_products` (53b46ece8),
// and an operator registering the page's list on the app would have had every
// product-content delivery refused. If this fails, the fix is the page's
// sentence beginning "The shared storefront flow requests", never this test:
// the code is what Shopify is asked for.
func TestTheConnectDocListsTheScopesTheManagedFlowRequests(t *testing.T) {
	raw, err := os.ReadFile(connectDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", connectDocPath, err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	const opener, closer = "The shared storefront flow requests", "Register those scopes"
	start := strings.Index(text, opener)
	if start < 0 {
		t.Fatalf("%s no longer says %q; point this test at the sentence that lists the scopes", connectDocPath, opener)
	}
	end := strings.Index(text[start:], closer)
	if end < 0 {
		t.Fatalf("%s: the scope sentence no longer ends with %q", connectDocPath, closer)
	}
	var documented []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(text[start:start+end], -1) {
		documented = append(documented, m[1])
	}
	want := managedConnectScopes()
	slices.Sort(documented)
	slices.Sort(want)
	if !slices.Equal(documented, want) {
		t.Errorf("%s lists %v; the managed flow requests %v", connectDocPath, documented, want)
	}
}

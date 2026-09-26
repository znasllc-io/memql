package identity

import "testing"

func TestShopifyReturnURL(t *testing.T) {
	for _, tc := range []struct {
		name, origin, path, result, site, want string
	}{
		{"no state", "https://os.example.test", "", "installed", "", "https://os.example.test/?connect=deployables&shopify=installed"},
		{"a state's path and site", "https://os.example.test/", "/apps/deployables", "connected", "site-1", "https://os.example.test/apps/deployables?shopify=connected&site=site-1"},
		{"a path with a query", "https://os.example.test", "/?connect=deployables", "permission_lost", "s", "https://os.example.test/?connect=deployables&shopify=permission_lost&site=s"},
		{"values are escaped", "https://os.example.test", "/", "a&b", "x y&z=1", "https://os.example.test/?shopify=a%26b&site=x+y%26z%3D1"},
		{"an off-origin path is dropped", "https://os.example.test", "//evil.example/x", "connected", "", "https://os.example.test/?connect=deployables&shopify=connected"},
		{"an absolute URL is dropped", "https://os.example.test", "https://evil.example/", "connected", "", "https://os.example.test/?connect=deployables&shopify=connected"},
		{"no nameable origin", "", "/x", "connected", "", "/x?shopify=connected"},
	} {
		if got := ShopifyReturnURL(tc.origin, tc.path, tc.result, tc.site); got != tc.want {
			t.Errorf("%s: got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

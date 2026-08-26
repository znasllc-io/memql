package shopify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnlyAShopHostIsAcceptedAsAStoreDomain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{name: "the ordinary case", in: "acme-widgets.myshopify.com", want: "acme-widgets.myshopify.com", valid: true},
		{name: "case and space are normalised", in: "  ACME.MyShopify.Com  ", want: "acme.myshopify.com", valid: true},
		{name: "a trailing slash is tolerated", in: "acme.myshopify.com/", want: "acme.myshopify.com", valid: true},

		// Each of these composed a live request URL before urlsafety.go.
		{name: "another vendor's host", in: "attacker.example", valid: false},
		{name: "a lookalike suffix", in: "acme.myshopify.com.attacker.example", valid: false},
		{name: "a prefix that only contains the suffix", in: "myshopify.com.attacker.example", valid: false},
		{name: "the bare apex", in: "myshopify.com", valid: false},
		{name: "a pasted scheme", in: "https://acme.myshopify.com", valid: false},
		{name: "credentials smuggled in the authority", in: "acme.myshopify.com@attacker.example", valid: false},
		{name: "an explicit port", in: "acme.myshopify.com:8080", valid: false},
		{name: "a path", in: "acme.myshopify.com/admin", valid: false},
		{name: "a query", in: "acme.myshopify.com?x=1", valid: false},
		{name: "a backslash authority", in: `acme.myshopify.com\@attacker.example`, valid: false},
		{name: "the cloud metadata address", in: "169.254.169.254", valid: false},
		{name: "an in-cluster service", in: "identity.memql.svc.cluster.local", valid: false},
		{name: "loopback", in: "127.0.0.1", valid: false},
		{name: "empty", in: "   ", valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeShopDomain(tc.in)
			if tc.valid {
				if err != nil {
					t.Fatalf("NormalizeShopDomain(%q) error = %v, want it accepted", tc.in, err)
				}
				if got != tc.want {
					t.Errorf("NormalizeShopDomain(%q) = %q, want %q", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("NormalizeShopDomain(%q) = %q with no error; a request to it would carry the store's Admin token", tc.in, got)
			}
			if got != "" {
				t.Errorf("NormalizeShopDomain(%q) returned %q alongside its error; a caller ignoring the error must not get a usable host", tc.in, got)
			}
		})
	}
}

func TestTheAdminEndpointRefusesAStoreRowItCannotTrust(t *testing.T) {
	t.Parallel()
	if _, err := AdminEndpoint("attacker.example", "2026-07"); err == nil {
		t.Error("AdminEndpoint accepted a non-shop domain")
	}
	// The version lands in a path segment off the same operator-written row.
	if _, err := AdminEndpoint("acme.myshopify.com", "../../../"); err == nil {
		t.Error("AdminEndpoint accepted a traversal as the API version")
	}
	if _, err := AdminEndpoint("acme.myshopify.com", "unstable"); err != nil {
		t.Errorf("AdminEndpoint refused the unstable channel: %v", err)
	}
}

func TestOnlyAPublicHttpsTargetIsDownloaded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		in    string
		valid bool
	}{
		{name: "shopify's object storage", in: "https://storage.googleapis.com/shopify-tiers-assets/x.jsonl?sig=a", valid: true},
		{name: "a public literal address", in: "https://93.184.216.34/x.jsonl", valid: true},

		{name: "plaintext", in: "http://storage.googleapis.com/x.jsonl", valid: false},
		{name: "a file url", in: "file:///etc/passwd", valid: false},
		{name: "credentials", in: "https://user:pw@storage.googleapis.com/x.jsonl", valid: false},
		{name: "loopback", in: "https://127.0.0.1/x.jsonl", valid: false},
		{name: "ipv6 loopback", in: "https://[::1]/x.jsonl", valid: false},
		{name: "a private range", in: "https://10.0.0.5/x.jsonl", valid: false},
		{name: "the cloud metadata address", in: "https://169.254.169.254/latest/meta-data/", valid: false},
		{name: "no host", in: "https:///x.jsonl", valid: false},
		{name: "empty", in: "  ", valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkBulkDownloadURL(tc.in)
			if tc.valid && err != nil {
				t.Fatalf("checkBulkDownloadURL(%q) error = %v, want it accepted", tc.in, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("checkBulkDownloadURL(%q) accepted a target the connector must not fetch", tc.in)
			}
		})
	}
}

// A guard on the first hop only is the usual way a guard like this is
// defeated, so the redirect policy is tested against a live redirect rather
// than asserted about.
func TestABulkDownloadRedirectIntoTheClusterIsRefused(t *testing.T) {
	t.Parallel()
	// TLS, not plaintext: over http the redirect is refused on the SCHEME,
	// which passes the test while proving nothing about the address check.
	private := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this must never be read"))
	}))
	defer private.Close()

	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/x.jsonl", http.StatusFound)
	}))
	defer hop.Close()

	// httptest listens on loopback, which is exactly the shape being refused.
	// The FIRST hop is therefore reached directly rather than through
	// checkBulkDownloadURL, so what the test proves is the redirect policy
	// and not the entry check.
	client := bulkDownloadClient(hop.Client(), nil)
	req, err := http.NewRequest(http.MethodGet, hop.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the redirect into loopback was followed")
	}
	if !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

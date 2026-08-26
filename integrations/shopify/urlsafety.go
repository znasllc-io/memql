package shopify

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// shopHostPattern is the whole of what an Admin API host may be.
//
// Shopify serves the Admin API from <shop>.myshopify.com and nowhere else.
// The storefront's public domain changes; this one does not, which is why
// dsl/shopify/overlay/concepts.memql calls it "the one identifier Shopify
// never changes" and why StoreIDForDomain already assumes the suffix when it
// derives an id. The pattern turns that documented convention into something
// the request path enforces rather than something it trusts.
var shopHostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\.myshopify\.com$`)

// apiVersionPattern bounds the other half of the composed URL. Shopify's
// versions are calendar quarters plus the moving "unstable" channel, and the
// value lands in a PATH segment -- so it is validated for the same reason the
// host is, not merely for tidiness.
var apiVersionPattern = regexp.MustCompile(`^(\d{4}-\d{2}|unstable)$`)

// NormalizeShopDomain lowercases a configured store domain and refuses
// anything that is not a bare <shop>.myshopify.com host.
//
// THIS IS AN SSRF CONTROL (CodeQL go/request-forgery), not tidiness. The
// domain arrives on a v1:shopify:store row an operator writes, and it used to
// be interpolated straight into https://%s/admin/api/... -- so a row naming
// "attacker.example" sent that store's Admin API access token to whoever
// owned the name, and a row naming a private address turned the connector
// into a probe of the cluster's own network. Neither needed a bug anywhere
// else to work: the row IS the whole exploit, and the row is the one field of
// this concept an operator is expected to type by hand.
//
// Refusing rather than repairing is deliberate. A domain that is not a shop
// host cannot be corrected into the one the operator meant, and quietly
// rewriting it would mirror somebody else's catalog under this store's id.
func NormalizeShopDomain(domain string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimSuffix(d, "/")
	if d == "" {
		return "", fmt.Errorf("shopify: store domain is empty")
	}
	// A scheme, credentials, a port and a path are each a way to make the
	// host end up somewhere other than where the string reads, so none are
	// accepted -- not even the https:// an operator might paste in. Checked
	// before the pattern so the error names the actual problem.
	if strings.ContainsAny(d, `/\@:?#`) {
		return "", fmt.Errorf("shopify: store domain %q must be a bare host, with no scheme, port, credentials or path", domain)
	}
	if !shopHostPattern.MatchString(d) {
		return "", fmt.Errorf("shopify: store domain %q is not a <shop>.myshopify.com host", domain)
	}
	return d, nil
}

// normalizeAPIVersion refuses a version that is not a version. It shares
// NormalizeShopDomain's reasoning: both halves of the composed URL come off
// the same operator-written row.
func normalizeAPIVersion(apiVersion string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(apiVersion))
	if v == "" {
		return "", fmt.Errorf("shopify: store apiVersion is empty")
	}
	if !apiVersionPattern.MatchString(v) {
		return "", fmt.Errorf("shopify: store apiVersion %q is not a YYYY-MM version or \"unstable\"", apiVersion)
	}
	return v, nil
}

// checkBulkDownloadURL refuses a download target that is not a plain https
// fetch from a public host.
//
// The URL is one Shopify handed back in a bulk-operation status, so the first
// control is NormalizeShopDomain above: an answer can only have come from a
// real shop host. This is the second (CodeQL go/request-forgery). The host is
// deliberately NOT allowlisted -- Shopify serves bulk results from object
// storage and which bucket host that is stays theirs to change, so an
// allowlist here would break a backfill on a vendor's routine migration. What
// is checked instead is the SHAPE: https, no credentials, and, when the host
// is a literal address, a public one. That last clause is what stops a
// spoofed or redirected answer turning a backfill into a read of
// 169.254.169.254.
func checkBulkDownloadURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("shopify: bulk download URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("shopify: bulk download URL is unparseable: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("shopify: bulk download URL must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("shopify: bulk download URL carries credentials")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("shopify: bulk download URL has no host")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("shopify: bulk download URL points at the non-public address %s", host)
	}
	return nil
}

// isPublicIP reports whether a literal address is one a bulk result could
// legitimately be served from: not the cluster's own network, not the
// loopback, and not the link-local range every cloud's metadata service sits
// on.
func isPublicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() &&
		!ip.IsPrivate() &&
		!ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsInterfaceLocalMulticast() &&
		!ip.IsUnspecified()
}

// bulkDownloadClient returns a client that re-validates every redirect hop.
//
// Without it checkBulkDownloadURL would guard only the FIRST hop and a 302
// would sail past it, which is the usual way a guard like this is defeated.
// The copy shares the transport, and so the connection pool, and differs only
// in the redirect policy -- the one thing that must not be shared, because
// the admin client's own calls do not redirect and attaching this rule to
// them would be a rule that never fires living on the wrong object.
func bulkDownloadClient(base *http.Client, check func(string) error) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	if check == nil {
		check = checkBulkDownloadURL
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("shopify: bulk download redirected more than 10 times")
		}
		return check(req.URL.String())
	}
	return &c
}

package library

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// An unusable remote resource is evidence for the DSL to choose another source,
// not a failure of the caller's MemQL permissions or of durable asset storage.
type imageSourceUnavailable struct{ reason string }

func (e *imageSourceUnavailable) Error() string { return "image source unusable: " + e.reason }
func unavailableImageSource(format string, args ...any) error {
	return &imageSourceUnavailable{reason: fmt.Sprintf(format, args...)}
}

func imagePublicAddress(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	// Shared address space and protocol-transition ranges can route internally.
	for _, cidr := range []string{"100.64.0.0/10", "0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32"} {
		if netip.MustParsePrefix(cidr).Contains(addr) {
			return false
		}
	}
	return true
}
func checkImageURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return fmt.Errorf("image sources must use public HTTPS URLs without credentials")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !imagePublicAddress(ip) {
		return fmt.Errorf("image source is not a public address")
	}
	return nil
}
func revisionImageClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("image source has no address")
		}
		for _, ip := range ips {
			if !imagePublicAddress(ip.IP) {
				return nil, fmt.Errorf("image source resolves outside the public network")
			}
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("image source redirected too many times")
		}
		return checkImageURL(req.URL.String())
	}}
}
func fetchRevisionImage(ctx context.Context, source string) ([]byte, error) {
	if err := checkImageURL(source); err != nil {
		return nil, err
	}
	client := revisionImageClient()
	defer client.CloseIdleConnections()
	return fetchRevisionImageWithClient(ctx, source, client)
}

func fetchRevisionImageWithClient(ctx context.Context, source string, client *http.Client) ([]byte, error) {
	if err := checkImageURL(source); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MemQL-Document-Images/1.0")
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp")
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailableImageSource("could not retrieve the public image: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableImageSource("remote server returned HTTP %d", response.StatusCode)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "image/") {
		return nil, unavailableImageSource("source returned a page instead of a raster image")
	}
	if response.ContentLength > 8<<20 {
		return nil, unavailableImageSource("image exceeds 8 MiB")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 8<<20 {
		return nil, unavailableImageSource("image exceeds 8 MiB")
	}
	return body, nil
}

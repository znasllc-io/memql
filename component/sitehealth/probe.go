// Package sitehealth measures published websites independently of their desired
// deployment state. The core automation schedules probes; browsers only read.
package sitehealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/httptls"
)

const ProbeUserAgent = "MemQL-Site-Health/1.0"

type Site struct {
	ID, Hostname, BundleRef, Status string
}

type Observation struct {
	SiteID     string    `bun:"site_id" json:"siteId"`
	Hostname   string    `bun:"hostname" json:"hostname"`
	BundleRef  string    `bun:"bundle_ref" json:"bundleRef"`
	CheckedAt  time.Time `bun:"checked_at" json:"checkedAt"`
	State      string    `bun:"state" json:"state"`
	Reason     string    `bun:"reason" json:"reason"`
	HTTPStatus int       `bun:"http_status" json:"httpStatus"`
	DurationMs int64     `bun:"duration_ms" json:"durationMs"`
}

type Prober struct {
	Client *http.Client
}

// dialAddress is deployment-owned, never supplied by a site or a browser. It
// routes the cluster's own loopback domain through its real ingress, retaining
// the original Host and TLS server name. Other hosts still use their own DNS.
func NewProber(domain, dialAddress, caFile string) (*Prober, error) {
	client, err := httptls.Client(5*time.Second, nil)
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	if configured, ok := client.Transport.(*http.Transport); ok {
		base = configured.Clone()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read website health CA: %w", err)
		}
		if base.TLSClientConfig == nil {
			base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		if base.TLSClientConfig.RootCAs == nil {
			base.TLSClientConfig.RootCAs, _ = x509.SystemCertPool()
		}
		if base.TLSClientConfig.RootCAs == nil {
			base.TLSClientConfig.RootCAs = x509.NewCertPool()
		}
		if !base.TLSClientConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("website health CA contains no certificates")
		}
	}
	base.Proxy = nil // Never carry ambient proxy credentials to a monitored site.
	dialer := &net.Dialer{Timeout: 4 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		trusted := domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
		if trusted && dialAddress != "" {
			return dialer.DialContext(ctx, network, dialAddress)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("hostname has no address")
		}
		// Resolve once and dial the checked address: DNS rebinding cannot turn a
		// public custom hostname into a metadata-service request between checks.
		for _, ip := range ips {
			if !trusted && (!ip.IP.IsGlobalUnicast() || ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast()) {
				return nil, fmt.Errorf("hostname resolves outside the permitted public network")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	client.Transport = base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Prober{Client: client}, nil
}

func (p *Prober) Check(ctx context.Context, site Site) Observation {
	start := time.Now()
	o := Observation{SiteID: site.ID, Hostname: site.Hostname, BundleRef: site.BundleRef, CheckedAt: start.UTC(), State: "unknown"}
	host := strings.TrimSpace(site.Hostname)
	u, err := url.Parse("https://" + host + "/")
	if err != nil || u.Hostname() != host || u.User != nil || strings.ContainsAny(host, "/\\?#:@ \t\n") || host == "" {
		o.Reason = "Invalid website hostname"
		return o
	}
	if p == nil || p.Client == nil {
		o.Reason = "Health checker is not configured"
		return o
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		o.Reason = "Could not create health request"
		return o
	}
	req.Header.Set("User-Agent", ProbeUserAgent)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := p.Client.Do(req)
	o.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		o.State = "unavailable"
		o.Reason = "Website could not be reached from the cluster (DNS, TLS, connection or timeout)"
		return o
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)); err != nil {
		o.State = "unavailable"
		o.Reason = "Website response could not be read"
		return o
	}
	o.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		o.State = "reachable"
		o.Reason = "Website answered successfully from the cluster"
	} else if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		o.State = "unknown"
		o.Reason = "Website redirects outside the checked address"
	} else if resp.StatusCode == 401 || resp.StatusCode == 403 {
		o.State = "unknown"
		o.Reason = "Website requires access; its content could not be verified"
	} else {
		o.State = "unavailable"
		o.Reason = fmt.Sprintf("Website returned HTTP %d", resp.StatusCode)
	}
	return o
}

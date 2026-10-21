package npmregistry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

// Observe verifies one published version's complete bytes without publishing,
// executing package scripts, or moving a dist-tag. A historical publication
// receipt alone does not establish that the archive is still available.
func (p *Publisher) Observe(ctx context.Context, want Expected) (Receipt, error) {
	if p == nil || p.target.Name != want.Name || p.target.Version != want.Version ||
		!validIdentity(want.Name, want.Version) || !digestPattern.MatchString(want.SHA256) || want.Size <= 0 || want.Size > maxArchive {
		return Receipt{}, errors.New("npm availability requires an exact bounded package identity and digest")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.target.RootCAs},
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// readback consumes only the expected identity, size and digest. No local
	// archive is needed, and no publication method is reachable on this path.
	exists, err := p.readback(ctx, client, &VerifiedPackage{expected: want})
	if err != nil {
		return Receipt{}, err
	}
	if !exists {
		return Receipt{}, errors.New("published npm archive is unavailable")
	}
	return Receipt{Origin: p.target.Origin, Name: want.Name, Version: want.Version, SHA256: want.SHA256, Size: want.Size}, nil
}

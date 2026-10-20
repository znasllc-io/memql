package npmregistry

import (
	"bytes"
	"context"
	"crypto/sha1" // npm's legacy shasum field; SHA-256 receipt gates publication.
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Target is operator configuration, bound into the caller's approval. Token is
// supplied only by a native secret resolver. DownloadOrigins never receive it.
type Target struct {
	Origin, Name, Version, DistTag, Token string
	DownloadOrigins                       []string
	RootCAs                               *x509.CertPool
	AllowLoopbackHTTP                     bool
}
type Publisher struct {
	target    Target
	downloads map[string]bool
}
type Receipt struct {
	Origin, Name, Version, SHA256 string
	Size                          int64
}

var ErrConflict = errors.New("npm version conflicts with the approved archive")
var ErrUncertain = errors.New("npm publication has no verified readback; preserve its intent and reconcile the same version")

func origin(raw string, loopback bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(u.Hostname(), "*%\\ \t\r\n") {
		return "", errors.New("npm endpoint must be one explicit origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !loopback || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return "", errors.New("npm endpoint requires HTTPS")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

// NewPublisher performs no network requests. Empty credentials allow validation
// of reviewed configuration; Publish refuses until a credential is supplied.
func NewPublisher(t Target) (*Publisher, error) {
	base, err := origin(t.Origin, t.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	if !validIdentity(t.Name, t.Version) || len(t.DistTag) == 0 || len(t.DistTag) > 64 || strings.Trim(t.DistTag, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" || (t.DistTag[0] >= '0' && t.DistTag[0] <= '9') || len(t.Token) > 16<<10 || len(t.DownloadOrigins) > 16 {
		return nil, errors.New("npm target requires exact bounded package identity and dist-tag")
	}
	for _, c := range t.Token {
		if c < 33 || c > 126 {
			return nil, errors.New("npm credential is invalid")
		}
	}
	downloads := map[string]bool{}
	for _, raw := range t.DownloadOrigins {
		o, err := origin(raw, false)
		if err != nil || o == base || downloads[o] {
			return nil, errors.New("npm download origins must be distinct HTTPS origins")
		}
		downloads[o] = true
	}
	t.Origin = base
	t.DownloadOrigins = append([]string(nil), t.DownloadOrigins...)
	if t.RootCAs != nil {
		t.RootCAs = t.RootCAs.Clone()
	}
	return &Publisher{target: t, downloads: downloads}, nil
}

// Publish writes one immutable version and verifies its bytes by reading them
// back. An existing identical version is recovered without writing or moving a
// dist-tag. A conflict never deletes or overwrites anything. Scripts, .npmrc,
// ambient proxies and Git credentials are never consulted.
func (p *Publisher) Publish(ctx context.Context, v *VerifiedPackage) (Receipt, error) {
	if p == nil || v == nil || len(v.archive) == 0 || p.target.Token == "" || p.target.Name != v.expected.Name || p.target.Version != v.expected.Version {
		return Receipt{}, errors.New("npm publication requires matching verified bytes and explicit credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.target.RootCAs},
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	exists, err := p.readback(ctx, client, v)
	if err != nil {
		return Receipt{}, err
	}
	if !exists {
		payload, err := p.payload(v)
		if err != nil {
			return Receipt{}, err
		}
		response, writeErr := p.request(ctx, client, http.MethodPut, p.packageURL(), payload)
		if response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
		}
		// Do not trust an upload response, including a 409 after another replica
		// completed the same immutable intent. Only exact readback can succeed.
		if writeErr != nil {
			return Receipt{}, ErrUncertain
		}
		exists, err = p.readback(ctx, client, v)
		if err != nil || !exists {
			return Receipt{}, ErrUncertain
		}
	}
	return Receipt{Origin: p.target.Origin, Name: v.expected.Name, Version: v.expected.Version, SHA256: v.expected.SHA256, Size: v.expected.Size}, nil
}

func (p *Publisher) packageURL() string { return p.target.Origin + "/" + url.PathEscape(p.target.Name) }

func (p *Publisher) payload(v *VerifiedPackage) ([]byte, error) {
	manifest := make(map[string]json.RawMessage, len(v.manifest)+2)
	for k, value := range v.manifest {
		manifest[k] = value
	}
	// These fields are publishing protocol data, not archive-provided authority.
	delete(manifest, "dist")
	delete(manifest, "_id")
	delete(manifest, "tag")
	delete(manifest, "patchedDependencies")
	digest512 := sha512.Sum512(v.archive)
	digest1 := sha1.Sum(v.archive)
	filename := p.target.Name + "-" + p.target.Version + ".tgz"
	dist := map[string]string{"integrity": "sha512-" + base64.StdEncoding.EncodeToString(digest512[:]), "shasum": hex.EncodeToString(digest1[:]), "tarball": p.target.Origin + "/" + p.target.Name + "/-/" + filename}
	manifest["dist"], _ = json.Marshal(dist)
	manifest["_id"], _ = json.Marshal(p.target.Name + "@" + p.target.Version)
	return json.Marshal(map[string]any{"_id": p.target.Name, "name": p.target.Name,
		"dist-tags":    map[string]string{p.target.DistTag: p.target.Version},
		"versions":     map[string]any{p.target.Version: manifest},
		"_attachments": map[string]any{filename: map[string]any{"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(v.archive), "length": len(v.archive)}}})
}

func (p *Publisher) request(ctx context.Context, client *http.Client, method, endpoint string, body []byte) (*http.Response, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.Opaque != "" || u.Fragment != "" || len(endpoint) > 8192 {
		return nil, errors.New("npm response named an invalid URL")
	}
	o := u.Scheme + "://" + u.Host
	if o != p.target.Origin && (method != http.MethodGet || !p.downloads[o]) {
		return nil, errors.New("npm response named an unapproved download origin")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("npm request could not be constructed")
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	if o == p.target.Origin {
		req.Header.Set("Authorization", "Bearer "+p.target.Token)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("npm transport failed")
	} // never expose signed query strings or credentials
	return response, nil
}

func (p *Publisher) readback(ctx context.Context, client *http.Client, v *VerifiedPackage) (bool, error) {
	response, err := p.request(ctx, client, http.MethodGet, p.packageURL(), nil)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, errors.New("npm package metadata could not be read")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20+1))
	if err != nil || len(body) > 16<<20 {
		return false, errors.New("npm metadata exceeds its limit or could not be read")
	}
	if err := uniqueJSON(body); err != nil {
		return false, errors.New("npm metadata contains ambiguous JSON")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(body, &metadata); err != nil || jsonString(metadata["name"]) != p.target.Name {
		return false, errors.New("npm metadata names another package")
	}
	var versions map[string]json.RawMessage
	if json.Unmarshal(metadata["versions"], &versions) != nil || versions == nil {
		return false, errors.New("npm metadata has no version map")
	}
	raw, exists := versions[p.target.Version]
	if !exists {
		return false, nil
	}
	var version, dist map[string]json.RawMessage
	if json.Unmarshal(raw, &version) != nil || json.Unmarshal(version["dist"], &dist) != nil || jsonString(version["name"]) != p.target.Name || jsonString(version["version"]) != p.target.Version || jsonString(dist["tarball"]) == "" {
		return false, ErrConflict
	}
	response, err = p.download(ctx, client, jsonString(dist["tarball"]))
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, errors.New("npm archive could not be read back")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(response.Body, v.expected.Size+1))
	if err != nil {
		return false, errors.New("npm archive readback failed")
	}
	if n != v.expected.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != v.expected.SHA256 {
		return false, ErrConflict
	}
	return true, nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func (p *Publisher) download(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	for redirects := 0; redirects <= 4; redirects++ {
		response, err := p.request(ctx, client, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		switch response.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			next, err := response.Location()
			_ = response.Body.Close()
			if err != nil {
				return nil, errors.New("npm archive redirect has no valid destination")
			}
			endpoint = next.String() // the next request rechecks the frozen allowlist
		default:
			return response, nil
		}
	}
	return nil, errors.New("npm archive exceeded the redirect limit")
}

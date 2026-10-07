package ociregistry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Target is trusted operator configuration, never artifact or DSL-supplied
// connection material. TokenEndpoint and TokenService explicitly authorize a
// bearer exchange; an unconfigured challenge cannot choose a credential sink.
// No ambient keychain, proxy, credential helper or registry discovery is used.
type Target struct {
	Origin, Repository              string
	Username, Password, BearerToken string
	TokenEndpoint, TokenService     string
	RootCAs                         *x509.CertPool
	// AllowLoopbackHTTP is an explicit installation setting for a loopback
	// registry; it never enables plaintext for a remote registry or token host.
	AllowLoopbackHTTP bool
}

type Publisher struct {
	target        Target
	origin, token *url.URL
	repo          name.Repository
}

type Receipt struct {
	Repository    string
	ArchiveSHA256 string
	ArchiveSize   int64
	ImageDigest   string
	Platform      string
}

// ErrUncertain means a write may have occurred without verified readback. Keep
// the caller's durable publication intent and artifact pins; retry the same
// candidate and target. Do not select a new version, delete data or report success.
var ErrUncertain = errors.New("OCI publication outcome is uncertain")

var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*$`)
var uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)

func endpoint(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return nil, errors.New("invalid configured registry endpoint")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !allowHTTP || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("registry endpoints require HTTPS")
		}
	}
	return u, nil
}

func NewPublisher(t Target) (*Publisher, error) {
	u, err := endpoint(t.Origin, t.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	if (u.Path != "" && u.Path != "/") || !repositoryPattern.MatchString(t.Repository) || len(t.Repository) > 255 {
		return nil, errors.New("invalid configured registry repository")
	}
	if (t.Username == "") != (t.Password == "") || (t.BearerToken != "" && t.Username != "") || strings.ContainsAny(t.BearerToken, "\r\n") {
		return nil, errors.New("configure one explicit registry credential")
	}
	var token *url.URL
	if t.TokenEndpoint != "" {
		token, err = endpoint(t.TokenEndpoint, t.AllowLoopbackHTTP)
		if err != nil {
			return nil, err
		}
		if token.Path == "" || t.TokenService == "" {
			return nil, errors.New("token endpoint requires an exact path and service")
		}
	} else if t.TokenService != "" {
		return nil, errors.New("token service requires an endpoint")
	}
	options := []name.Option{name.StrictValidation}
	if u.Scheme == "http" {
		options = append(options, name.Insecure)
	}
	repo, err := name.NewRepository(u.Host+"/"+t.Repository, options...)
	if err != nil || repo.RegistryStr() != u.Host || repo.RepositoryStr() != t.Repository {
		return nil, errors.New("registry repository must be explicit and canonical")
	}
	if t.RootCAs != nil {
		t.RootCAs = t.RootCAs.Clone()
	}
	return &Publisher{target: t, origin: u, token: token, repo: repo}, nil
}

// Publish verifies registry bytes by digest before returning a receipt. It is a
// bounded protocol effect, not a release workflow. The caller must first bind
// approval, destination, immutable source reader, pins and durable intent.
func (p *Publisher) Publish(ctx context.Context, v *VerifiedImage) (Receipt, error) {
	if p == nil || v == nil {
		return Receipt{}, errors.New("publisher and verified image are required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dir == "" || v.image == nil {
		return Receipt{}, errors.New("verified image is closed or uninitialized")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	httpTransport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.target.RootCAs},
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10, MaxIdleConns: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
		DisableCompression: true,
	}
	defer httpTransport.CloseIdleConnections()
	allowed := map[string]int64{}
	for _, b := range v.blobs {
		allowed[b.Digest] = b.Size
	}
	guard := &scopedTransport{publisher: p, inner: httpTransport, image: v.expected.ImageDigest, blobs: allowed}
	opts := []remote.Option{
		remote.WithContext(ctx), remote.WithTransport(guard), remote.WithJobs(2),
		remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: p.target.Username, Password: p.target.Password, RegistryToken: p.target.BearerToken})),
		remote.WithRetryBackoff(remote.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 3}),
	}
	ref := p.repo.Digest(v.expected.ImageDigest)
	ready, err := p.readback(ctx, v, opts)
	if err == nil && !ready {
		writeErr := remote.Write(ref, v.image, opts...)
		// Even after a lost final response, immutable digest readback can prove
		// success. It cannot turn partial blobs into a publication receipt.
		ready, err = p.readback(ctx, v, opts)
		if err == nil && !ready {
			err = writeErr
			if err == nil {
				err = errors.New("published manifest is absent")
			}
		}
	}
	if err != nil || !ready {
		// Registry/token response bodies and signed URLs are untrusted and may
		// contain secrets. Do not surface library error text to a caller/log.
		if guard.wrote.Load() {
			return Receipt{}, ErrUncertain
		}
		if ctx.Err() != nil {
			return Receipt{}, ctx.Err()
		}
		return Receipt{}, errors.New("OCI registry verification failed before publication")
	}
	w := v.expected
	return Receipt{Repository: p.repo.Name(), ArchiveSHA256: w.ArchiveSHA256, ArchiveSize: w.ArchiveSize, ImageDigest: w.ImageDigest, Platform: w.Platform}, nil
}

func (p *Publisher) readback(ctx context.Context, v *VerifiedImage, opts []remote.Option) (bool, error) {
	desc, err := remote.Get(p.repo.Digest(v.expected.ImageDigest), opts...)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	h, _, err := hashCopy(ctx, nil, strings.NewReader(string(desc.Manifest)), maxJSON)
	if err != nil || h != v.expected.ImageDigest {
		return false, errors.New("registry manifest bytes differ from digest")
	}
	seen := map[string]bool{}
	for _, b := range v.blobs {
		if seen[b.Digest] {
			continue
		}
		seen[b.Digest] = true
		layer, err := remote.Layer(p.repo.Digest(b.Digest), opts...)
		if err != nil {
			return false, err
		}
		r, err := layer.Compressed()
		if err != nil {
			return false, err
		}
		h, n, err := hashCopy(ctx, nil, r, b.Size)
		closeErr := r.Close()
		if err != nil {
			return false, err
		}
		if closeErr != nil {
			return false, closeErr
		}
		if h != b.Digest || n != b.Size {
			return false, errors.New("registry blob bytes differ from digest")
		}
	}
	return true, nil
}

type scopedTransport struct {
	publisher *Publisher
	inner     http.RoundTripper
	image     string
	blobs     map[string]int64
	wrote     atomic.Bool
	requests  atomic.Int64
}

func (s *scopedTransport) permitted(r *http.Request) (int64, error) {
	p, u := s.publisher, r.URL
	if u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") || len(u.RawQuery) > 16384 || (r.Host != "" && r.Host != u.Host) {
		return 0, errors.New("noncanonical registry request")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return 0, errors.New("invalid registry query")
	}
	if p.token != nil && u.Scheme == p.token.Scheme && u.Host == p.token.Host && u.Path == p.token.Path {
		if r.Method != http.MethodGet || len(q["service"]) != 1 || q.Get("service") != p.target.TokenService || len(q["scope"]) != 1 || len(q) != 2 {
			return 0, errors.New("token exchange differs from configured scope")
		}
		scope := q.Get("scope")
		if scope != "repository:"+p.target.Repository+":pull" && scope != "repository:"+p.target.Repository+":pull,push" && scope != "repository:"+p.target.Repository+":push,pull" {
			return 0, errors.New("token scope differs from configured repository")
		}
		return 64 << 10, nil
	}
	if u.Scheme != p.origin.Scheme || u.Host != p.origin.Host {
		return 0, errors.New("registry request left configured origin")
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if u.Path == "/v2/" && read && len(q) == 0 {
		return 64 << 10, nil
	}
	prefix := "/v2/" + p.target.Repository + "/"
	if u.Path == prefix+"manifests/"+s.image && len(q) == 0 && (read || r.Method == http.MethodPut) {
		return maxJSON, nil
	}
	if strings.HasPrefix(u.Path, prefix+"blobs/sha256:") && read && len(q) == 0 {
		d := strings.TrimPrefix(u.Path, prefix+"blobs/")
		if n, ok := s.blobs[d]; ok {
			return n, nil
		}
	}
	uploads := prefix + "blobs/uploads/"
	if u.Path == uploads && r.Method == http.MethodPost && len(q) == 0 {
		return 64 << 10, nil
	}
	if strings.HasPrefix(u.Path, uploads) && uploadIDPattern.MatchString(strings.TrimPrefix(u.Path, uploads)) && (r.Method == http.MethodPatch || r.Method == http.MethodPut || read) {
		for k, vs := range q {
			if len(vs) != 1 || (k != "digest" && k != "_state") {
				return 0, errors.New("unsupported upload query")
			}
			if k == "digest" {
				if _, ok := s.blobs[vs[0]]; !ok {
					return 0, errors.New("upload digest outside verified image")
				}
			}
		}
		if r.Method == http.MethodPut && q.Get("digest") == "" {
			return 0, errors.New("upload completion requires verified digest")
		}
		return 64 << 10, nil
	}
	return 0, errors.New("registry operation outside configured image and repository")
}

func (s *scopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	limit, err := s.permitted(r)
	if err != nil {
		return nil, err
	}
	if s.requests.Add(1) > 8*maxEntries+64 {
		return nil, errors.New("registry request count exceeds limit")
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut {
		s.wrote.Store(true)
	}
	resp, err := s.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// Upload session Location is validated when used, as is every request made
	// by the library. HTTP redirects are refused before following any location.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("registry redirects require an explicit transport contract")
	}
	if resp.StatusCode >= 400 {
		limit = 64 << 10
	}
	if r.Method == http.MethodHead {
		limit = 0
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: limit}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("registry response exceeds byte limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

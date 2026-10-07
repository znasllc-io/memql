// Package releasecatalog reads signed release records from another MemQL
// installation. This integration adapter lives in the root module because its
// canonical SDK transport belongs to that module; integrations/go.mod cannot
// import the root module. Its taxonomy does not depend on its package location.
// Selection, scheduling and installation remain separate DSL/native operations.
package releasecatalog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

const ConfigurationVariable = "MEMQL_RELEASE_SOURCES"

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type source struct {
	ID               string            `json:"id"`
	Publisher        string            `json:"publisher"`
	Endpoint         string            `json:"endpoint"`
	CredentialSecret string            `json:"credentialSecret"`
	PublicKeys       map[string]string `json:"publicKeys"`
	RootCAPEM        string            `json:"rootCaPem,omitempty"`
}

type configuration struct {
	FormatVersion int      `json:"formatVersion"`
	Sources       []source `json:"sources"`
}

type SourceSummary struct {
	ID        string `json:"id"`
	Publisher string `json:"publisher"`
}

// Reader resolves current operator configuration for every call. It retains no
// per-process trust, credentials, publication evidence or installation authority.
type Reader struct {
	variable func(context.Context, string) (string, error)
	secret   func(context.Context, string) (string, error)
}

func New(variable, secret func(context.Context, string) (string, error)) *Reader {
	return &Reader{variable: variable, secret: secret}
}

func admit(ctx context.Context) error {
	a, ok := auth.AccessFromContext(ctx)
	if !ok || a == nil || a.IsAnonymous || a.Synthetic || a.RoleStandIn || memql.BareShortId(a.UserId) == "" || (a.Role != auth.RoleOwner && a.Role != auth.RoleAdmin && a.Role != auth.RoleDeveloper) {
		return errors.New("release discovery requires an identified developer, admin or owner")
	}
	return nil
}

func (r *Reader) configuration(ctx context.Context) (configuration, error) {
	var cfg configuration
	if err := admit(ctx); err != nil {
		return cfg, err
	}
	if r == nil || r.variable == nil {
		return cfg, errors.New("release source configuration is unavailable")
	}
	raw, err := r.variable(memql.ContextWithFreshRead(ctx), ConfigurationVariable)
	if memql.IsVariableNotFound(err) {
		return configuration{FormatVersion: 1, Sources: []source{}}, nil
	}
	if err != nil {
		return cfg, errors.New("release source configuration could not be read")
	}
	if raw == "" {
		return configuration{FormatVersion: 1, Sources: []source{}}, nil
	}
	if len(raw) > 256<<10 || decode([]byte(raw), &cfg) != nil || cfg.FormatVersion != 1 || len(cfg.Sources) > 16 {
		return cfg, errors.New("release sources require bounded versioned configuration")
	}
	seen := map[string]bool{}
	for _, s := range cfg.Sources {
		if seen[s.ID] {
			return cfg, errors.New("release source identities must be unique")
		}
		seen[s.ID] = true
		if _, _, err := s.trust(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func (s source) trust() (map[string]ed25519.PublicKey, *x509.CertPool, error) {
	if !sourceName.MatchString(s.ID) || !sourceName.MatchString(s.Publisher) || !secretName.MatchString(s.CredentialSecret) || len(s.PublicKeys) == 0 || len(s.PublicKeys) > 32 || len(s.RootCAPEM) > 64<<10 {
		return nil, nil, errors.New("release source requires an identity, credential reference and publisher keys")
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || len(s.Endpoint) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(s.Endpoint, "\r\n\t ") {
		return nil, nil, errors.New("release source must name one authenticated HTTPS gRPC endpoint")
	}
	keys := map[string]ed25519.PublicKey{}
	for id, encoded := range s.PublicKeys {
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if !sourceName.MatchString(id) || err != nil || len(key) != ed25519.PublicKeySize {
			return nil, nil, errors.New("release source contains an invalid publisher key")
		}
		keys[id] = ed25519.PublicKey(key)
	}
	var roots *x509.CertPool
	if s.RootCAPEM != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(s.RootCAPEM)) {
			return nil, nil, errors.New("release source contains no valid root certificate")
		}
	}
	return keys, roots, nil
}

func (s source) endpoint() string {
	u, _ := url.Parse(s.Endpoint) // validated before use
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return "https://" + net.JoinHostPort(u.Hostname(), port)
}

func (r *Reader) Sources(ctx context.Context) ([]SourceSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg, err := r.configuration(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SourceSummary, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		out = append(out, SourceSummary{ID: s.ID, Publisher: s.Publisher})
	}
	return out, nil
}

func decode(body []byte, value any) error {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return errors.New("trailing catalog response")
	}
	return nil
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("catalog JSON exceeds its nesting bound")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == json.Delim('{') {
		keys := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || keys[strings.ToLower(key)] {
				return errors.New("catalog JSON contains ambiguous fields")
			}
			keys[strings.ToLower(key)] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	} else if token == json.Delim('[') {
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

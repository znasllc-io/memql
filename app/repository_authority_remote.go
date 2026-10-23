package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	"github.com/znasllc-io/memql/sdk/go/client"
)

const repositoryAuthorityPeersVariable = "MEMQL_REPOSITORY_AUTHORITY_PEERS"

type repositoryAuthorityPeer struct {
	Endpoint         string `json:"endpoint"`
	CredentialSecret string `json:"credentialSecret"`
	RootsSecret      string `json:"rootsSecret,omitempty"`
}

func repositoryAuthorityPeerConfig(raw, name string) (repositoryAuthorityPeer, error) {
	var cfg struct {
		FormatVersion int                                `json:"formatVersion"`
		Peers         map[string]repositoryAuthorityPeer `json:"peers"`
	}
	if len(raw) > 64<<10 || name == "" {
		return repositoryAuthorityPeer{}, errors.New("repository authority peer is not configured")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF || cfg.FormatVersion != 1 {
		return repositoryAuthorityPeer{}, errors.New("repository authority peer configuration is invalid")
	}
	p, ok := cfg.Peers[name]
	if !ok || p.CredentialSecret == "" || len(p.CredentialSecret) > 200 || strings.ContainsAny(p.CredentialSecret, "\r\n\x00") {
		return repositoryAuthorityPeer{}, errors.New("repository authority peer needs a request credential")
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return repositoryAuthorityPeer{}, errors.New("repository authority peer needs an exact TLS endpoint")
	}
	if _, tls, err := client.ParseClusterEndpoint(p.Endpoint); err != nil || !tls {
		return repositoryAuthorityPeer{}, errors.New("repository authority peer TLS endpoint is invalid")
	}
	return p, nil
}

// A binding contains only connection references and the peer-authenticated
// requester. Tokens and CA bytes can rotate in the same references without
// moving an in-flight request to another endpoint or requester identity.
func repositoryAuthorityPeerFingerprint(p repositoryAuthorityPeer, requester string) (string, error) {
	requester = memql.BareShortId(requester)
	if requester == "" || len(requester) > 200 || strings.ContainsAny(requester, " \r\n\t\x00") {
		return "", errors.New("repository authority peer did not identify its authenticated requester")
	}
	b, err := json.Marshal(struct {
		Peer      repositoryAuthorityPeer
		Requester string
	}{p, requester})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func repositoryAuthorityPeerDispatch(p repositoryAuthorityPeer, requester, expected string, call func() error) (string, error) {
	binding, err := repositoryAuthorityPeerFingerprint(p, requester)
	if err != nil {
		return "", err
	}
	if expected != "" && binding != expected {
		return "", errors.New("repository authority peer connection or requester changed; restore its binding or open a new request")
	}
	if call != nil {
		if expected == "" {
			return "", errors.New("repository authority transport requires its durable peer binding")
		}
		if err := call(); err != nil {
			return "", err
		}
	}
	return binding, nil
}

// Peer addresses, TLS roots and credentials come only from operator settings.
// Resolve the authenticated requester on this exact connection before a call.
// Neither configuration nor credential rotation can redirect a saved request.
func (a *App) repositoryAuthorityPeerSession(ctx context.Context, name, expected string, call func(context.Context, *client.QueryClient) error) (string, error) {
	raw, err := a.engine.ResolveSystemVariable(memql.ContextWithFreshRead(ctx), repositoryAuthorityPeersVariable)
	if err != nil {
		return "", errors.New("repository authority peer configuration is unavailable")
	}
	p, err := repositoryAuthorityPeerConfig(raw, name)
	if err != nil {
		return "", err
	}
	token, err := a.engine.ResolveSystemSecret(ctx, p.CredentialSecret)
	if err != nil || token == "" || len(token) > 16<<10 || strings.ContainsAny(token, " \r\n\t") {
		return "", errors.New("repository request credential is unavailable")
	}
	var roots *x509.CertPool
	if p.RootsSecret != "" {
		pem, e := a.engine.ResolveSystemSecret(ctx, p.RootsSecret)
		roots = x509.NewCertPool()
		if e != nil || !roots.AppendCertsFromPEM([]byte(pem)) {
			return "", errors.New("repository authority peer trust roots are unavailable")
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := client.Connect(callCtx, client.ConnectConfig{Endpoint: p.Endpoint, Token: token, RootCAs: roots})
	if err != nil {
		return "", errors.New("repository authority peer could not be reached securely")
	}
	defer conn.Close()
	q := client.NewQueryClient(conn.Dispatcher())
	identity, err := q.CurrentUser(callCtx, client.CurrentUserArgs{})
	if err != nil || identity == nil || len(identity.Rows()) != 1 {
		return "", errors.New("repository authority peer requester is unavailable")
	}
	requester, _ := identity.Rows()[0]["id"].(string)
	var dispatch func() error
	if call != nil {
		dispatch = func() error { return call(callCtx, q) }
	}
	return repositoryAuthorityPeerDispatch(p, requester, expected, dispatch)
}

func (a *App) repositoryAuthorityRemoteBinding(ctx context.Context, peer string) (string, error) {
	return a.repositoryAuthorityPeerSession(ctx, peer, "", nil)
}

func (a *App) repositoryAuthorityRemote(ctx context.Context, name, expected string, call func(context.Context, *client.QueryClient) (*client.Result, error)) (repositoryauthority.Receipt, error) {
	var out repositoryauthority.Receipt
	_, err := a.repositoryAuthorityPeerSession(ctx, name, expected, func(c context.Context, q *client.QueryClient) error {
		r, err := call(c, q)
		if err != nil {
			return errors.New("repository authority peer did not confirm the request; retry its same request identity")
		}
		if r == nil || len(r.Rows()) != 1 {
			return errors.New("repository authority peer returned no exact request receipt")
		}
		b, err := json.Marshal(r.Rows()[0])
		if err != nil {
			return errors.New("repository authority peer receipt is invalid")
		}
		d := json.NewDecoder(bytes.NewReader(b))
		if d.Decode(&out) != nil || out.ID == "" || out.RepositoryID <= 0 || out.Repository == "" || out.ExpiresAt == "" {
			return errors.New("repository authority peer receipt is incomplete")
		}
		return nil
	})
	if err != nil {
		return repositoryauthority.Receipt{}, err
	}
	return out, nil
}

func (a *App) repositoryAuthorityRemoteRequest(ctx context.Context, peer, binding string, transfer repositoryauthority.Transfer) (repositoryauthority.Receipt, error) {
	b, err := json.Marshal(transfer)
	if err != nil {
		return repositoryauthority.Receipt{}, err
	}
	var args map[string]any
	if err = json.Unmarshal(b, &args); err != nil {
		return repositoryauthority.Receipt{}, err
	}
	return a.repositoryAuthorityRemote(ctx, peer, binding, func(c context.Context, q *client.QueryClient) (*client.Result, error) {
		return q.RepositoryChangeReceive(c, client.RepositoryChangeReceiveArgs{Transfer: args})
	})
}

func (a *App) repositoryAuthorityRemoteStatus(ctx context.Context, peer, binding, requestID string) (repositoryauthority.Receipt, error) {
	return a.repositoryAuthorityRemote(ctx, peer, binding, func(c context.Context, q *client.QueryClient) (*client.Result, error) {
		return q.RepositoryChangeStatus(c, client.RepositoryChangeStatusArgs{RequestId: requestID})
	})
}

package releasecatalog

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/sdk/go/client"
)

const maxPage = 10

type ComponentSummary struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type ReleaseSummary struct {
	CandidateID string             `json:"candidateId"`
	Digest      string             `json:"catalogDigest"`
	Components  []ComponentSummary `json:"components"`
}

type Page struct {
	SourceID   string           `json:"sourceId"`
	Publisher  string           `json:"publisher"`
	Releases   []ReleaseSummary `json:"releases"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type publisherConnection struct {
	connection      *client.Connection
	endpoint, token string
	roots           *x509.CertPool
}

func (p *publisherConnection) Close() {
	if p.connection != nil {
		p.connection.Close()
	}
}
func (p *publisherConnection) read(ctx context.Context, request releasewire.Request) (client.Row, error) {
	if p.connection == nil {
		return ReadIdentityHTTP(ctx, p.endpoint, p.token, p.roots, request)
	}
	query := client.NewQueryClient(p.connection.Dispatcher())
	var result *client.Result
	var err error
	switch request.Operation {
	case "catalog":
		result, err = query.ReleaseGetPublishedCandidate(ctx, client.ReleaseGetPublishedCandidateArgs{CandidateId: request.CandidateID})
	case "list":
		result, err = query.ReleaseListPublishedCandidates(ctx, client.ReleaseListPublishedCandidatesArgs{Cursor: request.Cursor, Limit: request.Limit})
	default:
		return nil, errors.New("invalid publisher read")
	}
	if err != nil {
		return nil, transportError(ctx)
	}
	return oneRow(result)
}

func (r *Reader) open(ctx context.Context, sourceID string) (*publisherConnection, source, map[string]ed25519.PublicKey, error) {
	cfg, err := r.configuration(ctx)
	if err != nil {
		return nil, source{}, nil, err
	}
	var selected source
	for _, s := range cfg.Sources {
		if s.ID == sourceID {
			selected = s
			break
		}
	}
	if selected.ID == "" || r.secret == nil {
		return nil, source{}, nil, errors.New("release source is not configured")
	}
	keys, roots, err := selected.trust()
	if err != nil {
		return nil, source{}, nil, err
	}
	token, err := r.secret(memql.ContextWithFreshRead(ctx), selected.CredentialSecret)
	if err != nil || token == "" || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\t ") {
		return nil, source{}, nil, errors.New("release source credential is unavailable")
	}
	endpoint := selected.endpoint()
	u, _ := url.Parse(endpoint) // trust already validated the exact transport path
	if u.Path == releasewire.ReadPath {
		return &publisherConnection{endpoint: endpoint, token: token, roots: roots}, selected, keys, nil
	}
	if strings.HasPrefix(token, "mql_pat_") {
		return nil, source{}, nil, errors.New("personal access tokens require the Identity installation-read HTTPS endpoint")
	}
	conn, err := client.Connect(ctx, client.ConnectConfig{Endpoint: endpoint, Token: token, RootCAs: roots})
	if err != nil {
		return nil, source{}, nil, transportError(ctx)
	}
	return &publisherConnection{connection: conn}, selected, keys, nil
}

func transportError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Publisher diagnostics may include private configuration or credentials.
	return errors.New("release publisher could not be read through its authenticated connection")
}

func oneRow(result *client.Result) (client.Row, error) {
	rows := result.Rows()
	if len(rows) != 1 {
		return nil, errors.New("release publisher returned an invalid result")
	}
	return rows[0], nil
}

func get(ctx context.Context, query *publisherConnection, s source, keys map[string]ed25519.PublicKey, candidateID, digest string) (pl.VerifiedPublishedRelease, error) {
	var zero pl.VerifiedPublishedRelease
	row, err := query.read(ctx, releasewire.Request{Operation: "catalog", CandidateID: candidateID})
	if err != nil {
		return zero, transportError(ctx)
	}
	body, err := json.Marshal(row["envelope"])
	if err != nil {
		return zero, errors.New("release publisher returned no portable signed envelope")
	}
	verified, err := pl.VerifyPublishedRelease(body, s.Publisher, keys)
	if err != nil {
		return zero, errors.New("release does not verify under the configured publisher keys")
	}
	release, err := verified.Release()
	if err != nil || release.CandidateID != candidateID || verified.Digest() != digest || row["candidateId"] != candidateID || row["catalogDigest"] != digest {
		return zero, errors.New("release publisher returned a different candidate or catalog digest")
	}
	return verified, nil
}

// Get independently reads and verifies one exact selection under CURRENT source
// configuration. The caller receives an opaque verified value, never authority
// reconstructed from a browser's envelope or a serialized "verified" flag.
func (r *Reader) Get(ctx context.Context, sourceID, candidateID, expectedCatalogDigest string) (pl.VerifiedPublishedRelease, error) {
	var zero pl.VerifiedPublishedRelease
	if err := admit(ctx); err != nil {
		return zero, err
	}
	if !sourceName.MatchString(sourceID) || !digestPattern.MatchString(candidateID) || !digestPattern.MatchString(expectedCatalogDigest) {
		return zero, errors.New("release selection requires a source and exact candidate and catalog identities")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, s, keys, err := r.open(ctx, sourceID)
	if err != nil {
		return zero, err
	}
	defer conn.Close()
	return get(ctx, conn, s, keys, candidateID, expectedCatalogDigest)
}

// List reads one page, then verifies each suggested record. Unsigned list
// metadata is only a locator: every displayed component comes from its verified
// envelope. No automatic update choice, pagination loop or retry is hidden here.
func (r *Reader) List(ctx context.Context, sourceID, cursor string, limit int) (Page, error) {
	out := Page{SourceID: sourceID, Releases: []ReleaseSummary{}}
	if err := admit(ctx); err != nil {
		return out, err
	}
	if !sourceName.MatchString(sourceID) || len(cursor) > 1024 || limit < 1 || limit > maxPage {
		return out, errors.New("release discovery requires one source, a bounded cursor and a limit from 1 to 10")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, s, keys, err := r.open(ctx, sourceID)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	out.Publisher = s.Publisher
	query := conn
	row, err := query.read(ctx, releasewire.Request{Operation: "list", Cursor: cursor, Limit: limit})
	if err != nil {
		return out, transportError(ctx)
	}
	body, err := json.Marshal(row["releases"])
	var hints []ReleaseSummary
	if err != nil || len(body) > 256<<10 || decode(body, &hints) != nil || hints == nil || len(hints) > limit {
		return out, errors.New("release publisher exceeded or malformed its discovery page")
	}
	if next, found := row["nextCursor"]; found {
		value, ok := next.(string)
		if !ok || len(value) > 1024 || (value != "" && value == cursor) {
			return out, errors.New("release publisher returned an invalid cursor")
		}
		out.NextCursor = value
	}
	seen := map[string]bool{}
	for _, hint := range hints {
		if !digestPattern.MatchString(hint.CandidateID) || !digestPattern.MatchString(hint.Digest) || seen[hint.CandidateID] {
			return Page{}, errors.New("release publisher returned an invalid or duplicate selection")
		}
		seen[hint.CandidateID] = true
		verified, err := get(ctx, query, s, keys, hint.CandidateID, hint.Digest)
		if err != nil {
			return Page{}, err // never expose a partially verified page
		}
		release, _ := verified.Release()
		summary := ReleaseSummary{CandidateID: release.CandidateID, Digest: verified.Digest(), Components: []ComponentSummary{}}
		for _, c := range release.Components {
			summary.Components = append(summary.Components, ComponentSummary{Name: c.Name, Version: c.Version, Repository: c.Repository, Commit: c.Commit})
		}
		out.Releases = append(out.Releases, summary)
	}
	return out, nil
}

package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

const ReleaseCatalogConfigurationVariable = "MEMQL_RELEASE_CATALOG"

type catalogConfiguration struct {
	FormatVersion    int               `json:"formatVersion"`
	Publisher        string            `json:"publisher"`
	KeyID            string            `json:"keyId,omitempty"`
	SigningKeySecret string            `json:"signingKeySecret,omitempty"`
	PublicKeys       map[string]string `json:"publicKeys"`
}

func (c catalogConfiguration) trust() (map[string]ed25519.PublicKey, error) {
	if c.FormatVersion != 1 || !candidateTargetName.MatchString(c.Publisher) || len(c.PublicKeys) == 0 || len(c.PublicKeys) > 32 || (c.KeyID == "") != (c.SigningKeySecret == "") || (c.KeyID != "" && (!candidateTargetName.MatchString(c.KeyID) || !candidateSecretName.MatchString(c.SigningKeySecret))) {
		return nil, errors.New("catalog requires explicit bounded publisher and signing trust")
	}
	out := map[string]ed25519.PublicKey{}
	for key, value := range c.PublicKeys {
		b, err := base64.StdEncoding.Strict().DecodeString(value)
		if !candidateTargetName.MatchString(key) || err != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New("invalid catalog public key")
		}
		out[key] = ed25519.PublicKey(b)
	}
	if c.KeyID != "" && out[c.KeyID] == nil {
		return nil, errors.New("catalog signing identity is not trusted")
	}
	return out, nil
}

func (i *Integration) catalog(ctx context.Context) (*candidateLedger, catalogConfiguration, map[string]ed25519.PublicKey, error) {
	var cfg catalogConfiguration
	if err := catalogReader(ctx); err != nil {
		return nil, cfg, nil, err
	}
	if i == nil || i.resolver.systemVariable == nil {
		return nil, cfg, nil, errors.New("catalog configuration is unavailable")
	}
	raw, err := i.resolver.systemVariable(memql.ContextWithFreshRead(ctx), ReleaseCatalogConfigurationVariable)
	if err != nil || decodeCandidateObject([]byte(raw), &cfg) != nil {
		return nil, cfg, nil, errors.New("invalid release catalog configuration")
	}
	trust, err := cfg.trust()
	if err != nil {
		return nil, cfg, nil, err
	}
	database := i.candidateDB
	if deps := i.candidateDeps.Load(); deps != nil {
		database = deps.Database
	}
	ledger := &candidateLedger{db: database}
	if _, err := ledger.database(); err != nil {
		return nil, cfg, nil, err
	}
	return ledger, cfg, trust, nil
}

type catalogItem struct {
	CandidateID string              `json:"candidateId"`
	Digest      string              `json:"catalogDigest"`
	Envelope    json.RawMessage     `json:"envelope"`
	Release     pl.PublishedRelease `json:"release"`
}

func catalogResult(body []byte, verified pl.VerifiedPublishedRelease) ([]memorynodes.MemoryNode, error) {
	release, err := verified.Release()
	if err != nil {
		return nil, err
	}
	return resultNode("publishedRelease", release.CandidateID, catalogItem{CandidateID: release.CandidateID, Digest: verified.Digest(), Envelope: body, Release: release})
}

// Seal is one native local authority operation. The DSL/user chooses when to
// invoke it; it cannot turn incomplete effects into publication evidence. No
// provider mutation, network call, or signing key enters the DSL.
func (i *Integration) handleCatalogSeal(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return nil, err
	}
	ledger, cfg, trust, err := i.catalog(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.SigningKeySecret == "" || i.resolver.systemSecret == nil {
		return nil, errors.New("release catalog signing is not configured")
	}
	encoded, err := i.resolver.systemSecret(ctx, cfg.SigningKeySecret)
	if err != nil || len(encoded) > 128 {
		return nil, errors.New("release catalog signing key unavailable")
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("release signing secret must contain a base64 Ed25519 seed")
	}
	private := ed25519.NewKeyFromSeed(seed)
	defer clear(seed)
	defer clear(private)
	body, err := ledger.sealCatalog(ctx, asString(args["candidateId"]), asString(args["approvalId"]), cfg.Publisher, cfg.KeyID, private, trust)
	if err != nil {
		return nil, err
	}
	verified, err := pl.VerifyPublishedRelease(body, cfg.Publisher, trust)
	if err != nil {
		return nil, err
	}
	return catalogResult(body, verified)
}

func (i *Integration) handleCatalogGet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ledger, cfg, trust, err := i.catalog(ctx)
	if err != nil {
		return nil, err
	}
	db, err := ledger.database()
	if err != nil {
		return nil, err
	}
	body, verified, err := readCatalog(ctx, db, asString(args["candidateId"]), cfg.Publisher, trust)
	if err != nil {
		return nil, err
	}
	return catalogResult(body, verified)
}

type catalogSummary struct {
	CandidateID string                      `json:"candidateId"`
	Digest      string                      `json:"catalogDigest"`
	Components  []candidateComponentSummary `json:"components"`
}
type catalogPage struct {
	Releases   []catalogSummary `json:"releases"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

func listCatalog(ctx context.Context, db *sql.DB, publisher string, trust map[string]ed25519.PublicKey, cursor string, limit int) (catalogPage, error) {
	out := catalogPage{Releases: []catalogSummary{}}
	if err := catalogReader(ctx); err != nil {
		return out, err
	}
	if db == nil || limit < 1 || limit > 20 || len(cursor) > 1024 {
		return out, errors.New("catalog list requires storage, a bounded cursor and limit 1 to 20")
	}
	var position candidateCursor
	var before any
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || decodeCandidateObject(b, &position) != nil || position.CreatedAt.IsZero() || len(position.ID) != 71 || position.ID[:7] != "sha256:" || !candidateArtifactDigest(position.ID[7:]) {
			return out, errors.New("invalid catalog cursor")
		}
		before = position.CreatedAt
	}
	rows, err := db.QueryContext(ctx, `SELECT candidate_id,created_at FROM release_catalog WHERE publisher=$1 AND ($2::timestamptz IS NULL OR (created_at,candidate_id)<($2::timestamptz,$3)) ORDER BY created_at DESC,candidate_id DESC LIMIT $4`, publisher, before, position.ID, limit+1)
	if err != nil {
		return out, err
	}
	positions := []candidateCursor{}
	for rows.Next() {
		var p candidateCursor
		if e := rows.Scan(&p.ID, &p.CreatedAt); e != nil {
			rows.Close()
			return out, e
		}
		positions = append(positions, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(positions) > limit {
		positions = positions[:limit]
		b, e := json.Marshal(positions[len(positions)-1])
		if e != nil {
			return out, e
		}
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	for _, p := range positions {
		v, e := PublishedCatalog(ctx, db, p.ID, publisher, trust)
		if e != nil {
			return out, e
		}
		r, e := v.Release()
		if e != nil {
			return out, e
		}
		s := catalogSummary{CandidateID: r.CandidateID, Digest: v.Digest(), Components: []candidateComponentSummary{}}
		for _, c := range r.Components {
			s.Components = append(s.Components, candidateComponentSummary{Name: c.Name, Version: c.Version, Repository: c.Repository, Commit: c.Commit})
		}
		out.Releases = append(out.Releases, s)
	}
	return out, nil
}

func (i *Integration) handleCatalogList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ledger, cfg, trust, err := i.catalog(ctx)
	if err != nil {
		return nil, err
	}
	limit := 20
	if value, ok := args["limit"]; ok && value != nil {
		b, e := json.Marshal(value)
		if e != nil || json.Unmarshal(b, &limit) != nil {
			return nil, errors.New("catalog limit must be an integer")
		}
	}
	cursor := ""
	if value, ok := args["cursor"]; ok && value != nil {
		var valid bool
		cursor, valid = value.(string)
		if !valid {
			return nil, errors.New("catalog cursor must be a string")
		}
	}
	db, err := ledger.database()
	if err != nil {
		return nil, err
	}
	page, err := listCatalog(ctx, db, cfg.Publisher, trust, cursor, limit)
	if err != nil {
		return nil, err
	}
	return resultNode("publishedReleases", "", page)
}

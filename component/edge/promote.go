package edge

// promote.go -- artifact promotion (epic memql#3748 / memql#3768, design §4.2).
//
// "Promote the artifacts, not the cluster", made concrete.
//
// # Why this is not a DSL mutation
//
// Every other write to v1:platform:site goes through the engine, and this one
// deliberately cannot. The engine reaches exactly one schema -- the connection's
// search path decides which, and that is the whole environment boundary
// (memql#3765). A mutation body has no way to name another schema, so a promote
// expressed as a DSL mutation would write the row it read, in the environment it
// was already in, and do nothing.
//
// So the promote is the one site write that names its schemas explicitly. It is
// still ONE connection -- both schemas live in one database, which is precisely
// what D1 bought by choosing two schemas over two databases -- so it is also one
// transaction. With two databases this would be two connections and a copy
// through the application, and "one transaction" would be a lie.
//
// # What moves, and what conspicuously does not
//
// Only the REFERENCE moves. The bundle itself is immutable and versioned by
// prefix in shared object storage (`blob://sites/<id>/<version>/`), written once
// by the publish path and referenced by both environments. A promote that
// re-uploaded bytes would break the immutability the version prefix depends on
// and make rollback non-atomic, so this file performs no object-storage
// operation of any kind -- asserted by TestPromotePerformsNoObjectStorageWrite,
// because "no write happens" is the kind of claim that rots silently.
//
// The hostname does not move either, and that is not an oversight. A staging
// site lives under the CLUSTER's domain (`shop.staging.<clusterdomain>`) while
// its production counterpart lives wherever the customer's DNS points
// (`shop.acme.com`) -- decision D5. The two rows share an id and differ in
// hostname by design, so copying the source payload wholesale would move a
// staging hostname into production. Promote therefore replaces exactly one
// field on the target row and leaves the rest of it alone.
//
// # Rollback is not a code path
//
// Rollback is the same write with the previous value. That is why the write half
// is factored out as SetBundleRef and Promote is a thin resolve-then-write over
// it: if rollback had its own function it would drift, and the property the
// design leans on -- that a rollback is exactly as atomic as the promote it
// undoes -- would stop being structural.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// SiteConcept is the concept id promote reads and writes. Named here rather than
// inlined so a rename shows up as a compile error in one place.
const SiteConcept = "v1:platform:site"

// DefaultEnvironmentSchemaPrefix is what an environment's schema name is built
// from. The deployment already names them this way -- each namespace's secret
// carries `MEMQL_DB_SEARCH_PATH=<prefix><environment>, public` -- so this
// composes the same string the connection is already using.
const DefaultEnvironmentSchemaPrefix = "memql_"

// SchemaFor composes an environment's schema name from a prefix and the
// environment's own name.
//
// COMPOSED, never looked up, and that is the point. TestNoEnvironmentBranching-
// InEngineCode fails the build on engine code so much as NAMING an environment,
// in any form -- comparison, switch case, or map key -- because a name is what a
// branch is built out of. A map from environment to schema would be exactly the
// second way to deploy the parity standard rejects, so the environment arrives
// as a VALUE from the caller and leaves as a string. Adding an environment is
// then a deployment change and touches nothing here.
//
// The result is validated, because it is about to be interpolated into SQL.
func SchemaFor(prefix, environment string) (string, error) {
	schema := strings.TrimSpace(prefix) + strings.TrimSpace(environment)
	if err := validateSchemas(schema); err != nil {
		return "", fmt.Errorf("environment %q: %w", environment, err)
	}
	return schema, nil
}

// systemPromoteActor attributes the row a promote writes. A promote is an
// operator act performed by the engine on the operator's behalf; the actor that
// authorized it is carried in the audit trail of the surface that called this,
// not smuggled into createdBy where it would look like a user write.
const systemPromoteActor = "system:site-promote"

// schemaIdentifier is the same rule component/database applies to a search-path
// element. Duplicated rather than exported across the module boundary because it
// is one regexp and a shared helper would couple the edge to the driver; the
// PROPERTY that matters -- that a schema name reaching SQL string interpolation
// is validated first -- is enforced in both places.
var schemaIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ErrNoSourceSite is returned when the source environment has no row for the
// site. There is nothing to promote, and inventing a bundleRef would be worse
// than refusing.
var ErrNoSourceSite = errors.New("edge: site does not exist in the source environment")

// ErrHostnameRequired is returned when the target environment has no row for the
// site and the caller supplied no hostname to create it with. See D5: a
// production hostname is NOT derivable from a staging one.
var ErrHostnameRequired = errors.New("edge: site does not exist in the target environment; a hostname is required to create it")

// PromoteResult reports what a promote or rollback did. PreviousBundleRef is the
// value to hand back to SetBundleRef to undo it -- empty when the row was
// created, since there is nothing to go back to.
type PromoteResult struct {
	SiteID            string
	FromSchema        string
	ToSchema          string
	PreviousBundleRef string
	BundleRef         string
	Created           bool
	NoOp              bool
}

// Promoter performs cross-schema site promotion on one database connection.
type Promoter struct {
	db *bun.DB
}

// NewPromoter returns a Promoter over db. The connection's own search path is
// irrelevant here -- every statement this type issues names its schema -- which
// is what lets the same Promoter promote in either direction.
func NewPromoter(db *bun.DB) *Promoter { return &Promoter{db: db} }

// Promote reads the site's bundleRef in the `from` schema and writes it to the
// same site in the `to` schema, in one transaction.
//
// hostname is used only when the target row does not exist yet, in which case
// promote CREATES it: first-publish and promote are the same act, per the issue.
// When the target row exists, hostname is ignored -- the production hostname is
// production's business and a promote must not rewrite it.
func (p *Promoter) Promote(ctx context.Context, from, to, siteID, hostname string) (PromoteResult, error) {
	if err := validateSchemas(from, to); err != nil {
		return PromoteResult{}, err
	}
	if siteID == "" {
		return PromoteResult{}, errors.New("edge: promote requires a site id")
	}

	var out PromoteResult
	err := p.db.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		source, err := latestSiteRow(ctx, tx, from, siteID)
		if err != nil {
			return err
		}
		if source == nil {
			return fmt.Errorf("%w: %s in %s", ErrNoSourceSite, siteID, from)
		}
		ref, err := bundleRefOf(source.Payload)
		if err != nil {
			return fmt.Errorf("reading bundleRef of %s in %s: %w", siteID, from, err)
		}
		out, err = writeBundleRef(ctx, tx, from, to, siteID, ref, hostname, source)
		if err != nil {
			return err
		}
		out.FromSchema, out.ToSchema = from, to
		return nil
	})
	return out, err
}

// SetBundleRef writes bundleRef onto the site in the `target` schema. This is
// the write half of Promote, exported because it is ALSO the rollback: hand it
// the PreviousBundleRef a promote returned and the site is back where it was, by
// the same code path and with the same atomicity.
func (p *Promoter) SetBundleRef(ctx context.Context, target, siteID, bundleRef string) (PromoteResult, error) {
	if err := validateSchemas(target); err != nil {
		return PromoteResult{}, err
	}
	if siteID == "" {
		return PromoteResult{}, errors.New("edge: setting a bundleRef requires a site id")
	}
	if bundleRef == "" {
		return PromoteResult{}, errors.New("edge: refusing to write an empty bundleRef")
	}

	var out PromoteResult
	err := p.db.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = writeBundleRef(ctx, tx, "", target, siteID, bundleRef, "", nil)
		if err != nil {
			return err
		}
		out.ToSchema = target
		return nil
	})
	return out, err
}

// siteRow is one version of a site row, as stored.
type siteRow struct {
	Payload  json.RawMessage
	Schema   json.RawMessage
	Metadata json.RawMessage
	Type     string
}

// writeBundleRef appends a new version of the site row in `target` carrying
// bundleRef, and reports what changed. It runs inside the caller's transaction
// so promote's read and write commit together.
//
// fallback is the source row, used only to seed a row that does not exist yet;
// SetBundleRef passes nil because a rollback of a row that is not there is a
// mistake worth surfacing rather than papering over.
func writeBundleRef(ctx context.Context, tx bun.Tx, source, target, siteID, bundleRef, hostname string, fallback *siteRow) (PromoteResult, error) {
	out := PromoteResult{SiteID: siteID, BundleRef: bundleRef}

	current, err := latestSiteRow(ctx, tx, target, siteID)
	if err != nil {
		return out, err
	}

	var payload map[string]any
	switch {
	case current != nil:
		out.PreviousBundleRef, err = bundleRefOf(current.Payload)
		if err != nil {
			return out, fmt.Errorf("reading bundleRef of %s in %s: %w", siteID, target, err)
		}
		// The row exists, so only the reference moves. Everything else --
		// hostname above all -- is the target environment's own state.
		if err := json.Unmarshal(current.Payload, &payload); err != nil {
			return out, fmt.Errorf("decoding payload of %s in %s: %w", siteID, target, err)
		}
	case fallback == nil:
		return out, fmt.Errorf("%w: %s in %s", ErrNoSourceSite, siteID, target)
	case hostname == "":
		return out, fmt.Errorf("%w: %s in %s", ErrHostnameRequired, siteID, target)
	default:
		// First publish into this environment. Seed from the source row so kind,
		// apiProxy and the rest carry over, then take the hostname the caller
		// supplied -- never the source's, which belongs to the other environment.
		if err := json.Unmarshal(fallback.Payload, &payload); err != nil {
			return out, fmt.Errorf("decoding source payload of %s: %w", siteID, err)
		}
		payload["hostname"] = hostname
		out.Created = true
	}

	// Promoting the version already pinned is a no-op rather than a fresh row.
	// The graph's row history IS the version list for a site, so an idempotent
	// re-promote that appended would put a phantom deploy in the operator's
	// timeline.
	if !out.Created && out.PreviousBundleRef == bundleRef {
		out.NoOp = true
		return out, nil
	}

	payload["bundleRef"] = bundleRef
	encoded, err := json.Marshal(payload)
	if err != nil {
		return out, fmt.Errorf("encoding payload of %s: %w", siteID, err)
	}

	seed := current
	if seed == nil {
		seed = fallback
	}
	rowType := seed.Type
	if rowType == "" {
		rowType = "object"
	}
	metadata := seed.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}

	// Provenance is NOT NULL on the table and the mutation executor refuses
	// writes that carry none, so a row this path writes must be attributable
	// too -- a promoted row that looked provenance-less would be the one row in
	// the graph nobody could explain.
	via := target
	if source != "" {
		via = source + " -> " + target
	}
	provenance, err := json.Marshal(map[string]any{
		"kind": "promote",
		"name": "promoteSite",
		"via":  via,
	})
	if err != nil {
		return out, fmt.Errorf("encoding provenance: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+quoteSchema(target)+`."MemoryNodes"
		   (id, "createdAt", "createdBy", concept, "type", schema, payload, metadata, provenance)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		siteID, time.Now().UTC(), systemPromoteActor, SiteConcept, rowType,
		string(seed.Schema), string(encoded), string(metadata), string(provenance),
	); err != nil {
		return out, fmt.Errorf("writing %s in %s: %w", siteID, target, err)
	}
	return out, nil
}

// latestSiteRow reads the newest version of a site row in one schema, or nil
// when the site is not there. Nodes are append-only, so "the row" is the newest
// version by createdAt.
func latestSiteRow(ctx context.Context, tx bun.Tx, schema, siteID string) (*siteRow, error) {
	var r siteRow
	err := tx.QueryRowContext(ctx,
		`SELECT payload, schema, metadata, "type"
		   FROM `+quoteSchema(schema)+`."MemoryNodes"
		  WHERE id = ? AND concept = ?
		  ORDER BY "createdAt" DESC
		  LIMIT 1`,
		siteID, SiteConcept,
	).Scan(&r.Payload, &r.Schema, &r.Metadata, &r.Type)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s in %s: %w", siteID, schema, err)
	}
	return &r, nil
}

// bundleRefOf pulls the one field a promote moves out of a site payload.
func bundleRefOf(payload json.RawMessage) (string, error) {
	var p struct {
		BundleRef string `json:"bundleRef"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", err
	}
	if p.BundleRef == "" {
		return "", errors.New("site payload carries no bundleRef")
	}
	return p.BundleRef, nil
}

// validateSchemas refuses anything that is not a plain lower-case identifier.
// These names reach SQL by string interpolation -- a schema cannot be a bound
// parameter -- so this is the check that makes that safe, and it runs before any
// statement is built rather than beside one.
func validateSchemas(names ...string) error {
	for _, n := range names {
		if !schemaIdentifier.MatchString(n) {
			return fmt.Errorf("edge: %q is not a valid schema name", n)
		}
	}
	return nil
}

// quoteSchema renders a name validated by validateSchemas. Callers must have
// validated first; the quoting is belt-and-braces, not the check.
func quoteSchema(name string) string { return `"` + name + `"` }

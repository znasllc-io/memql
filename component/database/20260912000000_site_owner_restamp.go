package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Cloud repair: re-stamp the owner on v1:platform:site rows blanked by the
// owner erasure (memql#5292; the erasure is PR #5284's subject).
//
// ===========================================================================
// WHAT IT REPAIRS
// ===========================================================================
// Every developer deploy before PR #5284 wrote a site row in three steps:
// createSite as the developer (stamping them as ownerUserId), then
// recordSitePackageOrigin through store.writeInternal -- the SAME developer
// context with internal origin stamped -- naming only packageId and
// packageDeployableName. That second write was privileged, its merged payload
// carried the developer's own id, and applySiteOwnerStamp's self-match undo
// DELETED the key it had not just stamped. The row it left is cluster-owned:
// its latest version has NO ownerUserId and a non-empty packageId, so the
// developer who deployed it cannot read it through sitesAll, flip it live, or
// delete it. #5284 stops the erasure going forward; this migration re-stamps
// the rows it already left.
//
// ===========================================================================
// SCOPED BY CONCEPT, LATEST VERSION ONLY, A NEW VERSION
// ===========================================================================
// Selection is by concept (`v1:platform:site`), never by key name, and reads
// each id's LATEST version -- `DISTINCT ON (id) ... ORDER BY id, "createdAt"
// DESC`, the same collapse every engine read makes. A row whose latest version
// already carries an owner is not a candidate even if an older version was
// blanked: the engine reads the newest, and that one is fine.
//
// Unlike the retired-field migrations beside this one, the repair does not
// rewrite stored versions. MemoryNodes is append-only and a site's history is
// its versions, so the repair is ONE NEW VERSION per row: the latest version
// copied whole -- createdBy, type, schema, metadata, every payload key --
// with ownerUserId set and nothing else changed, under a `migration`
// provenance naming this file. Idempotent by construction: the second run
// finds the latest version owned and selects nothing. The down direction
// deletes exactly the versions this migration appended, found by that
// provenance, which restores the prior state without inventing anything.
//
// ===========================================================================
// WHERE THE OWNER COMES FROM
// ===========================================================================
// The deployment row that produced the site. v1:platform:packageDeployment's
// `requestedBy` is stamped from the deploying actor's userId -- the same value
// createSite stamped as ownerUserId before the undo deleted it -- and its
// `deployables[]` names the site id it published. So the owner is the
// `requestedBy` of the NEWEST deployment for the site's packageId whose
// deployables[] names the site; when none names it (a run that closed before
// outcomes were recorded), the package row's own ownerUserId; and when that is
// empty too the row is LEFT ALONE and logged by id, because writing a guessed
// owner onto a row would record a guarantee nothing provides.
//
// The values are copied verbatim. requestedBy, package.ownerUserId and
// site.ownerUserId are all stamped from the same actor.userId, so they carry
// one spelling by construction and nothing here composes or parses an id.
//
// ===========================================================================
// WHY GO AND NOT SQL
// ===========================================================================
// The neighbouring migrations are .sql files and this one is registered in the
// SAME bun set under the same 14-digit version, ordered with them. It is Go
// because the repair must LOG: the count it changed and, by id, every row it
// could not resolve -- a cloud operator has no other way to learn that a site
// stayed cluster-owned. A RAISE NOTICE from SQL cannot do that here: pgdriver
// DISCARDS NoticeResponse messages (driver.go / proto.go), so a SQL migration
// can log nothing at all through this stack.
//
// RestampSiteOwners is exported so the database test in component/packages
// runs exactly what boot runs, against rows the engine wrote, and asserts the
// developer can then read the site through sitesAll and write
// updateSiteStatus(live).

const (
	// siteOwnerRestampMigrationName is the provenance name stamped on every
	// version this migration appends, and what the down direction deletes by.
	siteOwnerRestampMigrationName = "20260912000000_site_owner_restamp"

	siteOwnerRestampSiteConcept       = "v1:platform:site"
	siteOwnerRestampPackageConcept    = "v1:platform:package"
	siteOwnerRestampDeploymentConcept = "v1:platform:packageDeployment"

	// SiteOwnerRestampSourceDeployment names a restamp whose owner came from
	// the newest packageDeployment naming the site in its deployables[].
	SiteOwnerRestampSourceDeployment = "deployment"
	// SiteOwnerRestampSourcePackage names a restamp whose owner came from the
	// package row, because no deployment named the site.
	SiteOwnerRestampSourcePackage = "package"
)

// SiteOwnerRestamp is one site the migration decided about.
type SiteOwnerRestamp struct {
	SiteId    string
	PackageId string
	// OwnerUserId is the owner written, or empty for an unresolved row.
	OwnerUserId string
	// Source is SiteOwnerRestampSourceDeployment or
	// SiteOwnerRestampSourcePackage, or empty for an unresolved row.
	Source string
}

// SiteOwnerRestampReport is what one run of the migration did.
type SiteOwnerRestampReport struct {
	// Restamped lists every site that received a new version with its owner
	// set.
	Restamped []SiteOwnerRestamp
	// Unresolved lists every blanked site left alone because neither a
	// deployment nor the package row could name its owner.
	Unresolved []SiteOwnerRestamp
}

// registerSiteOwnerRestamp adds the migration to the set the SQL files are
// discovered into. It MUST be called from this file: bun names a Go migration
// after the file its Register call sits in, and this file's name carries the
// version.
func registerSiteOwnerRestamp(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil {
		return
	}
	for _, existing := range m.Sorted() {
		if existing.Name == strings.SplitN(siteOwnerRestampMigrationName, "_", 2)[0] {
			return
		}
	}
	m.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			_, err := RestampSiteOwners(ctx, db, logger)
			return err
		},
		func(ctx context.Context, db *bun.DB) error {
			return RevertSiteOwnerRestamp(ctx, db, logger)
		},
	)
}

// siteOwnerRestampSelectSQL lists every blanked site with the two candidate
// owners. No `?` anywhere in it: bun rewrites that character as a placeholder,
// so the jsonb key-exists operator is spelled through COALESCE instead.
const siteOwnerRestampSelectSQL = `
WITH latest_site AS (
  SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
   WHERE concept = '` + siteOwnerRestampSiteConcept + `'
   ORDER BY id, "createdAt" DESC
),
blanked AS (
  SELECT id, payload->>'packageId' AS package_id
    FROM latest_site
   WHERE COALESCE(payload->>'ownerUserId', '') = ''
     AND COALESCE(payload->>'packageId', '') <> ''
     AND COALESCE(payload->>'systemOwned', 'false') <> 'true'
     AND COALESCE(payload->>'deleted', 'false') <> 'true'
),
latest_deployment AS (
  SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
   WHERE concept = '` + siteOwnerRestampDeploymentConcept + `'
   ORDER BY id, "createdAt" DESC
),
latest_package AS (
  SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
   WHERE concept = '` + siteOwnerRestampPackageConcept + `'
   ORDER BY id, "createdAt" DESC
)
SELECT b.id,
       b.package_id,
       (SELECT d.payload->>'requestedBy'
          FROM latest_deployment d
         WHERE d.payload->>'packageId' = b.package_id
           AND COALESCE(d.payload->>'requestedBy', '') <> ''
           AND EXISTS (
                 SELECT 1
                   FROM jsonb_array_elements(
                          CASE WHEN jsonb_typeof(d.payload->'deployables') = 'array'
                               THEN d.payload->'deployables'
                               ELSE '[]'::jsonb END) AS e
                  WHERE e->>'siteId' = b.id)
         ORDER BY d."createdAt" DESC
         LIMIT 1) AS deployment_owner,
       (SELECT NULLIF(p.payload->>'ownerUserId', '')
          FROM latest_package p
         WHERE p.id = b.package_id) AS package_owner
  FROM blanked b
 ORDER BY b.id`

// siteOwnerRestampInsertSQL appends the repaired version: the LATEST version
// of the id copied whole, with ownerUserId set. Strictly later than the
// version it copies, since (id, "createdAt") is the primary key. The owner
// guard in the subquery makes a concurrent engine write that already set an
// owner turn this into a no-op rather than a second version.
const siteOwnerRestampInsertSQL = `
INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
SELECT id,
       GREATEST(now(), "createdAt" + interval '1 millisecond'),
       "createdBy", concept, type, schema,
       jsonb_set(payload, '{ownerUserId}', to_jsonb(?::text), true),
       metadata,
       ?::jsonb
  FROM (SELECT * FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1) AS latest
 WHERE concept = ?
   AND COALESCE(payload->>'ownerUserId', '') = ''`

// RestampSiteOwners runs the repair once, in one transaction, and reports
// what it did. Safe to call any number of times.
func RestampSiteOwners(ctx context.Context, db *bun.DB, logger *slog.Logger) (SiteOwnerRestampReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var report SiteOwnerRestampReport
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		candidates, err := siteOwnerRestampCandidates(ctx, tx)
		if err != nil {
			return err
		}
		provenance := fmt.Sprintf(`{"kind":"migration","name":%q}`, siteOwnerRestampMigrationName)
		for _, c := range candidates {
			if c.OwnerUserId == "" {
				report.Unresolved = append(report.Unresolved, c)
				logger.Warn("site owner restamp: no owner resolvable, row left cluster-owned",
					"migration", siteOwnerRestampMigrationName, "siteId", c.SiteId, "packageId", c.PackageId)
				continue
			}
			res, err := tx.ExecContext(ctx, siteOwnerRestampInsertSQL,
				c.OwnerUserId, provenance, c.SiteId, siteOwnerRestampSiteConcept)
			if err != nil {
				return fmt.Errorf("site owner restamp: appending the repaired version of %s: %w", c.SiteId, err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				// The latest version gained an owner between the select and
				// the insert. Nothing to repair any more; say so and move on.
				logger.Info("site owner restamp: row already owned at write time, skipped",
					"migration", siteOwnerRestampMigrationName, "siteId", c.SiteId)
				continue
			}
			report.Restamped = append(report.Restamped, c)
			logger.Info("site owner restamp: owner re-stamped",
				"migration", siteOwnerRestampMigrationName, "siteId", c.SiteId,
				"packageId", c.PackageId, "ownerUserId", c.OwnerUserId, "source", c.Source)
		}
		return nil
	})
	if err != nil {
		return SiteOwnerRestampReport{}, err
	}
	logger.Info("site owner restamp: done",
		"migration", siteOwnerRestampMigrationName,
		"restamped", len(report.Restamped), "unresolved", len(report.Unresolved))
	return report, nil
}

// siteOwnerRestampCandidates reads every blanked site and decides its owner
// in the documented order: the deployment's requestedBy, else the package
// row's ownerUserId, else nothing.
func siteOwnerRestampCandidates(ctx context.Context, tx bun.Tx) ([]SiteOwnerRestamp, error) {
	rows, err := tx.QueryContext(ctx, siteOwnerRestampSelectSQL)
	if err != nil {
		return nil, fmt.Errorf("site owner restamp: selecting blanked sites: %w", err)
	}
	defer rows.Close()

	var out []SiteOwnerRestamp
	for rows.Next() {
		var (
			siteId, packageId             string
			deploymentOwner, packageOwner sql.NullString
		)
		if err := rows.Scan(&siteId, &packageId, &deploymentOwner, &packageOwner); err != nil {
			return nil, fmt.Errorf("site owner restamp: scanning: %w", err)
		}
		c := SiteOwnerRestamp{SiteId: siteId, PackageId: packageId}
		switch {
		case strings.TrimSpace(deploymentOwner.String) != "":
			c.OwnerUserId = strings.TrimSpace(deploymentOwner.String)
			c.Source = SiteOwnerRestampSourceDeployment
		case strings.TrimSpace(packageOwner.String) != "":
			c.OwnerUserId = strings.TrimSpace(packageOwner.String)
			c.Source = SiteOwnerRestampSourcePackage
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("site owner restamp: reading: %w", err)
	}
	return out, nil
}

// RevertSiteOwnerRestamp is the down direction: delete exactly the versions
// this migration appended, identified by their provenance. Every other
// version of every site is left as it was, so the rows read as they did
// before the up ran.
func RevertSiteOwnerRestamp(ctx context.Context, db *bun.DB, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes"
		  WHERE concept = ?
		    AND provenance->>'kind' = 'migration'
		    AND provenance->>'name' = ?`,
		siteOwnerRestampSiteConcept, siteOwnerRestampMigrationName)
	if err != nil {
		return fmt.Errorf("site owner restamp: reverting: %w", err)
	}
	n, _ := res.RowsAffected()
	logger.Info("site owner restamp: reverted", "migration", siteOwnerRestampMigrationName, "deleted", n)
	return nil
}

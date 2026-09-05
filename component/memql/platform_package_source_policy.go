package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// One source, once: the SOURCE UNIQUENESS guard for v1:platform:package
// (2026-09-05-deployables-states-activation-and-source-archive-design.md, D8).
// Sibling of platform_site_hostname_policy.go, wired into the same
// executeWrite path and for the same reason: "another active package already
// tracks this repository at this ref" is a read across rows, which no mutation
// body can make, and a UI-only check is not a check.
//
// # Why it is a rule and not a nicety
//
// The owner added the same repository at the same branch twice and got two
// sources on the list, each declaring the same apps. Deploying both is not
// merely confusing: a package's DSL domains land in the cluster's active set,
// so two packages shipping one domain would race at the roll, and its apps
// would compete for the same addresses. There is exactly one thing a second
// registration of one source at one ref can be, and that is a mistake.
//
// # What counts as the same source
//
// The repository URL as a person types it varies in ways that name the same
// tree -- scheme, case, a `.git` suffix, a trailing slash, the SSH form -- so
// the comparison is over normalizeRepoSource's answer, never the raw string.
// The ref is compared after trimming and EMPTY IS ITS OWN VALUE: a package
// tracking "" follows the default branch, and this guard cannot resolve what
// that branch is called, so `main` and "" are two refs here. The OS's own
// check on the Source stop knows the default branch from the probe and closes
// that gap before Analyze.
//
// # Archived packages do not count
//
// The whole point of archiving a source (D3) is that it can be added again,
// so only ACTIVE packages hold a source. Cluster-wide, exactly as hostnames
// are: a source another person tracks still collides, and the refusal names
// the package that holds it -- the same disclosure a hostname collision
// makes, and the one fact that helps.

// conceptPlatformPackage is v1:platform:package's canonical concept id.
const conceptPlatformPackage = "v1:platform:package"

// normalizeRepoSource reduces a repository URL and a ref to the pair two
// registrations are compared on: `host/owner/name` in lowercase with the
// scheme, `www.`, a `.git` suffix and any trailing slash removed, and the ref
// trimmed. An SSH form (`git@host:owner/name`) normalizes to the same answer
// as its https twin.
func normalizeRepoSource(repoUrl, repoRef string) (string, string) {
	url := strings.ToLower(strings.TrimSpace(repoUrl))
	if at := strings.Index(url, "@"); at >= 0 && !strings.Contains(url[:at], "/") {
		// git@github.com:owner/name -> github.com/owner/name
		url = strings.Replace(url[at+1:], ":", "/", 1)
	}
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	url = strings.TrimPrefix(url, "www.")
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimRight(url, "/")
	return url, strings.TrimSpace(repoRef)
}

// sourceHolder is what a collision names.
type sourceHolder struct {
	ID   string
	Name string
}

// validatePackageSourceUnique refuses a write that would make two ACTIVE
// packages track one repository at one ref.
//
// It runs only on a write that is CLAIMING a source -- a create, or an update
// that changes the URL or the ref -- because every other write inherits the
// stored pair through the read-merge, and re-judging an inherited value would
// refuse a rename or an auto-deploy flip on a package created before the rule.
// An archive write is not a claim either: it is the one write whose whole
// purpose is to stop holding the source.
func (e *MemQLEngine) validatePackageSourceUnique(
	ctx context.Context,
	payload map[string]any,
	mutationId, actor string,
	priorExisted bool,
	priorRepoUrl, priorRepoRef string,
) error {
	if e == nil || payload == nil {
		return nil
	}
	if strings.TrimSpace(stringFromAny(payload["sourceKind"])) != "repo" {
		return nil
	}
	if strings.TrimSpace(stringFromAny(payload["status"])) == "archived" {
		return nil
	}
	url, ref := normalizeRepoSource(stringFromAny(payload["repoUrl"]), stringFromAny(payload["repoRef"]))
	if url == "" {
		// `repoUrl` is optional on the concept; a repo-kind package with no
		// URL is somebody else's rule to refuse.
		return nil
	}
	if priorExisted {
		priorUrl, priorRef := normalizeRepoSource(priorRepoUrl, priorRepoRef)
		if priorUrl == url && priorRef == ref {
			return nil
		}
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	holders, err := e.activePackagesTrackingSource(ctx, url, ref)
	if err != nil {
		// Fail CLOSED, for the reason the hostname guard states: "we could
		// not check" and "there is nothing to find" are different answers.
		return fmt.Errorf("v1:platform:package: cannot verify that %q is not already tracked: %w", url, err)
	}
	self := canonicalPackageStorageId(mutationId)
	for _, h := range holders {
		if canonicalPackageStorageId(h.ID) == self {
			continue
		}
		which := "the default branch"
		if ref != "" {
			which = fmt.Sprintf("%q", ref)
		}
		name := h.Name
		if name == "" {
			name = h.ID
		}
		return fmt.Errorf(
			"v1:platform:package: %s at %s is already tracked by the source %q (%s). One source is added "+
				"once -- its apps and its MemQL would otherwise deploy twice from two records. Open that "+
				"source instead, or archive it first if you meant to start over.",
			url, which, name, h.ID)
	}
	return nil
}

// canonicalPackageStorageId converts either spelling of a package row id into
// the stored, concept-qualified form, so self-exclusion matches on an update
// that arrived with a bare id.
func canonicalPackageStorageId(rowId string) string {
	trimmed := strings.TrimSpace(rowId)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, conceptPlatformPackage+":") {
		return trimmed
	}
	return conceptPlatformPackage + ":" + trimmed
}

// activePackagesTrackingSource returns the ACTIVE packages whose LATEST version
// tracks this normalized repository at this ref.
//
// Two steps, the shape liveSiteIdsForHostname spells out: the store is
// time-series, so one predicate over every version would match rows since
// repointed or archived, and scanning the whole concept is unbounded. Step
// one narrows by the repository's `owner/name` tail -- a predicate the
// database answers, and a superset because it ignores scheme and suffix --
// and step two resolves only those rows to their latest version and compares
// the normalized pair exactly.
//
// Read WITHOUT row-authz narrowing, as the hostname probe is: a source another
// person tracks must still collide even though the caller may not read that
// row, which is why the refusal names the package and nothing else about it.
func (e *MemQLEngine) activePackagesTrackingSource(ctx context.Context, url, ref string) ([]sourceHolder, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}
	tail := url
	if i := strings.Index(url, "/"); i >= 0 {
		tail = url[i+1:]
	}
	if tail == "" {
		return nil, nil
	}

	var candidates []string
	err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Column("id").
		Where("concept = ?", conceptPlatformPackage).
		Where("lower(payload->>'repoUrl') LIKE ?", "%"+tail+"%").
		Distinct().
		Scan(ctx, &candidates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan package source candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	var nodes []memorynodes.MemoryNode
	err = db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptPlatformPackage).
		Where("id IN (?)", bun.In(candidates)).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan package source holders: %w", err)
	}

	seen := make(map[string]struct{}, len(candidates))
	var out []sourceHolder
	for _, node := range nodes {
		if _, dup := seen[node.ID]; dup {
			continue // an older version of a row already resolved
		}
		seen[node.ID] = struct{}{}

		var p map[string]any
		if jerr := json.Unmarshal(node.Payload, &p); jerr != nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(p["sourceKind"])) != "repo" {
			continue
		}
		if strings.TrimSpace(stringFromAny(p["status"])) != "active" {
			continue // an archived source holds nothing; that is what archiving is for
		}
		gotUrl, gotRef := normalizeRepoSource(stringFromAny(p["repoUrl"]), stringFromAny(p["repoRef"]))
		if gotUrl != url || gotRef != ref {
			continue
		}
		out = append(out, sourceHolder{ID: node.ID, Name: strings.TrimSpace(stringFromAny(p["name"]))})
	}
	return out, nil
}

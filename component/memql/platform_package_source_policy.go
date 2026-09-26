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

// Repository registrations are unique within their owning user, MemQL account,
// credential and installation binding. The same tree reached through another
// identity is an independent source. Hidden source entries continue holding
// their tuple so re-add restores the existing deployment history.

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
	priorRepoUrl, priorRepoRef, priorScope string,
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
		if priorUrl == url && priorRef == ref && priorScope == packageSourceScope(payload) {
			return nil
		}
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	holders, err := e.activePackagesTrackingSource(ctx, url, ref, packageSourceScope(payload))
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
				"once for this identity -- open or restore that source instead.",
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
// Reads bypass row-authz only to find candidates; the final exact comparison
// includes owner, account, credential and binding. A different person's source
// never collides and its identifier is never returned in a refusal.
//
// staged-data: MUST-NOT-GATE -- the gate CREATES the violation it would then be
// unable to detect (epic memql#3974, task memql#3984). This is a uniqueness
// probe of exactly liveSiteIdsForHostname's shape: it looks for an existing
// active package tracking the source so the write can be refused. Withhold a
// staged row and the probe finds nothing, the write is admitted, and the
// cluster now holds two packages on one source -- one of them the very row the
// probe could not see, so the duplicate is invisible to the next probe as
// well. The read discloses nothing but the holder's id; it is the thing
// keeping a second row out.
func (e *MemQLEngine) activePackagesTrackingSource(ctx context.Context, url, ref, scope string) ([]sourceHolder, error) {
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
		if gotUrl != url || gotRef != ref || packageSourceScope(p) != scope {
			continue
		}
		out = append(out, sourceHolder{ID: node.ID, Name: strings.TrimSpace(stringFromAny(p["name"]))})
	}
	return out, nil
}

// PackageSourceIdentity is shared by registration and the engine guard. IDs are
// normalized at this boundary; clients never compose this key.
func PackageSourceIdentity(payload map[string]any) string {
	url, ref := normalizeRepoSource(stringFromAny(payload["repoUrl"]), stringFromAny(payload["repoRef"]))
	encoded, _ := json.Marshal([]string{packageSourceScope(payload), url, ref})
	return string(encoded)
}

func packageSourceScope(payload map[string]any) string {
	var parts []string
	for _, field := range []string{"ownerUserId", "accountId", "credentialId", "sourceConnectionId"} {
		parts = append(parts, BareShortId(strings.TrimSpace(stringFromAny(payload[field]))))
	}
	encoded, _ := json.Marshal(parts)
	return string(encoded)
}

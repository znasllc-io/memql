package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// PackageDomainIntent names one domain effect selected before a package roll.
// Neither the selection nor its receipt contains the DNS verification token.
type PackageDomainIntent struct {
	Key            string `json:"key"`
	PublicationKey string `json:"publicationKey"`
	DomainID       string `json:"domainId"`
	SiteID         string `json:"siteId"`
	AccountID      string `json:"accountId"`
	Hostname       string `json:"hostname"`
}

// A receipt distinguishes creation, observation of an existing matching
// binding, and a definitive no-effect policy refusal. Transport/database errors
// never become refusal receipts: an unknown outcome must remain observable.
type PackageDomainReceipt struct {
	Key              string    `json:"key"`
	PublicationKey   string    `json:"publicationKey"`
	DomainID         string    `json:"domainId"`
	SiteID           string    `json:"siteId"`
	AccountID        string    `json:"accountId"`
	Hostname         string    `json:"hostname"`
	DomainVersion    time.Time `json:"domainVersion"`
	ExistingDomainID string    `json:"existingDomainId,omitempty"`
	Refusal          string    `json:"refusal,omitempty"`
}

func PackageDomainReceiptMatches(want PackageDomainIntent, got PackageDomainReceipt) bool {
	if !validPackageDomainIntent(want) || want.Key != got.Key || want.PublicationKey != got.PublicationKey || want.DomainID != got.DomainID || want.SiteID != got.SiteID || want.AccountID != got.AccountID || want.Hostname != got.Hostname {
		return false
	}
	if got.Refusal != "" {
		return len(got.Refusal) <= 2048 && got.DomainVersion.IsZero() && got.ExistingDomainID == ""
	}
	return !got.DomainVersion.IsZero() && (got.ExistingDomainID == "" || got.ExistingDomainID == BareShortId(got.ExistingDomainID))
}

func validPackageDomainIntent(p PackageDomainIntent) bool {
	return p.Key != "" && p.PublicationKey != "" && p.DomainID != "" && p.DomainID == BareShortId(p.DomainID) && p.SiteID != "" && p.SiteID == BareShortId(p.SiteID) && p.AccountID != "" && p.AccountID == BareShortId(p.AccountID) && p.Hostname != "" && p.Hostname == strings.ToLower(strings.TrimSpace(p.Hostname))
}

type packageDomainFence struct {
	deployment, selection, lease string
	intent                       PackageDomainIntent
	requester                    context.Context
}
type packageDomainFenceKey struct{}

// ContextWithPackageDomainFence adds restrictions, never authority. Retain the
// original request context before customDomainAdd's ordinary internal writer
// stamp, so the transaction can recheck that person rather than the service.
// A stable selected ID must remain absent: retries observe receipts and never
// replace a binding or mint a new token over an existing one.
func ContextWithPackageDomainFence(ctx context.Context, deployment, selection, lease string, intent PackageDomainIntent) context.Context {
	f := packageDomainFence{BareShortId(deployment), selection, lease, intent, ctx}
	ctx = ContextWithRowVersionFence(ctx, conceptPlatformCustomDomain, intent.DomainID, time.Time{})
	return context.WithValue(ctx, packageDomainFenceKey{}, f)
}

func HasPackageDomainFence(ctx context.Context) bool {
	_, ok := ctx.Value(packageDomainFenceKey{}).(packageDomainFence)
	return ok
}

func PackageDomainIntentFromContext(ctx context.Context) (PackageDomainIntent, bool) {
	f, ok := ctx.Value(packageDomainFenceKey{}).(packageDomainFence)
	return f.intent, ok
}

var ErrPackageDomainFence = errors.New("package domain selection, authority or live claim differs")
var ErrPackageDomainRecorded = errors.New("package domain result already recorded; observe its receipt")

// checkPackageDomain runs inside the graph/outbox transaction, after row
// witnesses lock the absent domain and the published site. Known no-effect
// outcomes commit only a receipt; nil means the ordinary creation guard and
// graph write must still succeed. The continuation row lock couples either
// outcome to the same live lease, preventing a stale replica from recording it.
// staged-data: MUST-NOT-GATE -- this is publication/ownership evidence, not a
// content read. A staged site still carries the authoritative account, package
// and published version; hiding it would falsely refuse that exact receipt or
// let an older visible version stand in for current authority. The original
// requester is separately checked against the actual row and current grants.
func (e *MemQLEngine) checkPackageDomain(ctx context.Context, db bun.IDB, concept, domainID string, payload map[string]any) (*PackageDomainReceipt, error) {
	f, ok := ctx.Value(packageDomainFenceKey{}).(packageDomainFence)
	if !ok {
		return nil, nil
	}
	p := f.intent
	if f.requester == nil || !validPackageDomainIntent(p) || concept != conceptPlatformCustomDomain || BareShortId(domainID) != p.DomainID || BareShortId(stringFromAny(payload["siteId"])) != p.SiteID || BareShortId(stringFromAny(payload["accountId"])) != p.AccountID || stringFromAny(payload["hostname"]) != p.Hostname || stringFromAny(payload["status"]) != "pending_dns" || stringFromAny(payload["token"]) == "" {
		return nil, ErrPackageDomainFence
	}
	var requester, packageID, selection string
	var body, publications, receipts []byte
	err := db.QueryRowContext(ctx, `SELECT requested_by, package_id, selection_sha256, body, publications, domain_receipts FROM package_deployment_continuations WHERE deployment_id = ? AND lease_id = ? AND lease_until > clock_timestamp() AND deadline > clock_timestamp() AND phase = 'publishing' AND NOT cancel_requested FOR UPDATE`, f.deployment, f.lease).Scan(&requester, &packageID, &selection, &body, &publications, &receipts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPackageDomainFence
	}
	if err != nil {
		return nil, err
	}
	access, present := auth.AccessFromContext(f.requester)
	if !present || access.UserId == "" || requester != BareShortId(access.UserId) || selection != f.selection {
		return nil, ErrPackageDomainFence
	}
	var selected struct {
		Domains      []PackageDomainIntent      `json:"domains"`
		Publications []PackagePublicationIntent `json:"publications"`
	}
	if json.Unmarshal(body, &selected) != nil || len(selected.Domains) == 0 {
		return nil, ErrPackageDomainFence
	}
	found, seen := false, map[string]bool{}
	for _, domain := range selected.Domains {
		if !validPackageDomainIntent(domain) || seen[domain.Key] {
			return nil, ErrPackageDomainFence
		}
		seen[domain.Key] = true
		if domain.Key == p.Key {
			found = domain == p
		}
	}
	var completed map[string]PackageDomainReceipt
	if !found || json.Unmarshal(receipts, &completed) != nil {
		return nil, ErrPackageDomainFence
	}
	if _, recorded := completed[p.Key]; recorded {
		return nil, ErrPackageDomainRecorded
	}
	var published map[string]PackagePublicationReceipt
	if json.Unmarshal(publications, &published) != nil {
		return nil, ErrPackageDomainFence
	}
	publication, publishedOK := published[p.PublicationKey]
	var selectedPublication *PackagePublicationIntent
	for index := range selected.Publications {
		if selected.Publications[index].Key == p.PublicationKey {
			if selectedPublication != nil {
				return nil, ErrPackageDomainFence
			}
			selectedPublication = &selected.Publications[index]
		}
	}
	if !publishedOK || selectedPublication == nil || publication.SiteID != p.SiteID || selectedPublication.SiteID != p.SiteID || selectedPublication.AccountID != p.AccountID || publication.Target != selectedPublication.Target || publication.BundleRef != selectedPublication.BundleRef || publication.SiteVersion.IsZero() || !reflect.DeepEqual(publication.Binding, selectedPublication.Binding) || !reflect.DeepEqual(publication.PreviewBinding, selectedPublication.PreviewBinding) {
		return nil, ErrPackageDomainFence
	}
	absent, siteWitness := false, false
	for _, witness := range rowVersionFences(ctx) {
		if witness.concept == conceptPlatformCustomDomain && BareShortId(witness.id) == p.DomainID && witness.version.IsZero() {
			absent = true
		}
		if witness.concept == conceptPlatformSite && BareShortId(witness.id) == p.SiteID && witness.version.Equal(publication.SiteVersion) {
			siteWitness = true
		}
	}
	if !absent || !siteWitness {
		return nil, ErrPackageDomainFence
	}
	var site memorynodes.MemoryNode
	err = db.NewSelect().Model(&site).ModelTableExpr(memorynodes.ObservedRowsSQL+" AS mn").Where("concept = ?", conceptPlatformSite).Where("id = ?", conceptPlatformSite+":"+p.SiteID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx)
	if err != nil {
		return nil, err
	}
	var sitePayload map[string]any
	if json.Unmarshal(site.Payload, &sitePayload) != nil || BareShortId(stringFromAny(sitePayload["accountId"])) != p.AccountID || BareShortId(stringFromAny(sitePayload["packageId"])) != packageID || !site.CreatedAt.Equal(publication.SiteVersion) {
		return nil, ErrPackageDomainFence
	}
	field := "bundleRef"
	if publication.Target == "candidate" {
		field = "candidateRef"
	} else if publication.Target != "serving" {
		return nil, ErrPackageDomainFence
	}
	if stringFromAny(sitePayload[field]) != publication.BundleRef {
		return nil, ErrPackageDomainFence
	}
	actorCtx := ContextWithFreshAuthorization(f.requester)
	role, roleOK := e.organizationUserRole(actorCtx, access.UserId)
	if !roleOK || !auth.CanAuthor(auth.UserContext{Role: role}) {
		return nil, ErrPackageDomainFence
	}
	current := *access
	current.Role = role
	actorCtx = auth.ContextWithAccess(auth.ContextWithClientOrigin(actorCtx), &current)
	actorCtx = contextWithRankScopeMemo(contextWithAccountScopeMemo(actorCtx, e), e)
	if !admitRowAuthzTraversal(actorCtx, site) || !e.OrganizationCapable(actorCtx, p.AccountID, auth.VerbExecute, "app:deployables/domains") {
		return nil, ErrPackageDomainFence
	}
	observation, err := e.observePackageDomain(ctx, db, p)
	if err != nil || observation == nil {
		return observation, err
	}
	if err := writePackageDomainReceipt(ctx, db, f, *observation); err != nil {
		return nil, err
	}
	return observation, nil
}

func domainReceipt(p PackageDomainIntent) PackageDomainReceipt {
	return PackageDomainReceipt{Key: p.Key, PublicationKey: p.PublicationKey, DomainID: p.DomainID, SiteID: p.SiteID, AccountID: p.AccountID, Hostname: p.Hostname}
}

// Observations are made in the transaction, never inferred from an arbitrary
// error string. Unknown reads propagate unchanged. The usual policy still runs
// for a new binding; this classification preserves its optional refusal result
// and recognizes an existing same-site binding without rotating its token.
// staged-data: MUST-NOT-GATE -- hostname exclusivity and the per-site domain
// cap include staged bindings and sites, just like the ordinary domain policy.
// Hiding them would admit duplicate hostnames or undercount the cap; hiding an
// already-created matching binding would also lose the evidence needed to
// recover its receipt without reminting a verification token. These reads
// classify durable effects and never return a domain payload or token.
func (e *MemQLEngine) observePackageDomain(ctx context.Context, db bun.IDB, p PackageDomainIntent) (*PackageDomainReceipt, error) {
	receipt := domainReceipt(p)
	if err := validateCustomDomainHostname(p.Hostname, customDomainPolicyDomain()); err != nil {
		receipt.Refusal = err.Error()
		return &receipt, nil
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1296387416, hashtext(?))`, p.Hostname); err != nil {
		return nil, err
	}
	// Resolve latest versions first, so a renamed or removed binding never
	// wins by an older version. Scope the candidate set to this host/site.
	var candidates []string
	err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("id").Where("concept = ?", conceptPlatformCustomDomain).Where("lower(payload->>'hostname') = ? OR payload->>'siteId' IN (?)", p.Hostname, bun.In([]string{p.SiteID, conceptPlatformSite + ":" + p.SiteID})).Distinct().Scan(ctx, &candidates)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	count := 0
	var matching *PackageDomainReceipt
	conflicting := false
	for _, candidate := range candidates {
		var row memorynodes.MemoryNode
		if err := db.NewSelect().Model(&row).ModelTableExpr(memorynodes.ObservedRowsSQL+" AS mn").Where("concept = ?", conceptPlatformCustomDomain).Where("id = ?", candidate).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx); err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(row.Payload, &fields); err != nil {
			return nil, err
		}
		status := stringFromAny(fields["status"])
		if customDomainTerminalStatus(status) {
			continue
		}
		sameSite := BareShortId(stringFromAny(fields["siteId"])) == p.SiteID
		if sameSite {
			count++
		}
		if strings.ToLower(stringFromAny(fields["hostname"])) != p.Hostname {
			continue
		}
		if sameSite && BareShortId(stringFromAny(fields["accountId"])) == p.AccountID && status != "removing" {
			if matching != nil {
				conflicting = true
			}
			observed := domainReceipt(p)
			observed.ExistingDomainID, observed.DomainVersion = BareShortId(row.ID), row.CreatedAt
			matching = &observed
		} else {
			conflicting = true
		}
	}
	if conflicting {
		receipt.Refusal = "The hostname is already bound or its existing binding is being removed."
		return &receipt, nil
	}
	// Match the ordinary policy's latest-version site check on this same
	// transaction connection. A failed read is never a collision or absence.
	var sites []string
	err = db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("id").Where("concept = ?", conceptPlatformSite).Where("lower(payload->>'hostname') = ?", p.Hostname).Distinct().Scan(ctx, &sites)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for _, siteID := range sites {
		var site memorynodes.MemoryNode
		if err := db.NewSelect().Model(&site).ModelTableExpr(memorynodes.ObservedRowsSQL+" AS mn").Where("concept = ?", conceptPlatformSite).Where("id = ?", siteID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx); err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(site.Payload, &fields); err != nil {
			return nil, err
		}
		if strings.ToLower(strings.TrimSpace(stringFromAny(fields["hostname"]))) == p.Hostname && !boolFromAny(fields["deleted"]) {
			receipt.Refusal = "The hostname is already served by a deployable."
			return &receipt, nil
		}
	}
	if matching != nil {
		return matching, nil
	}
	if count >= customDomainMaxPerSite() {
		receipt.Refusal = "The deployable already has the configured maximum number of custom domains."
		return &receipt, nil
	}
	return nil, nil
}

func recordPackageDomain(ctx context.Context, db bun.IDB, created memorynodes.Node) error {
	f, ok := ctx.Value(packageDomainFenceKey{}).(packageDomainFence)
	if !ok {
		return nil
	}
	if created.Concept != conceptPlatformCustomDomain || BareShortId(created.ID) != f.intent.DomainID || created.CreatedAt.IsZero() {
		return ErrPackageDomainFence
	}
	receipt := domainReceipt(f.intent)
	receipt.DomainVersion = created.CreatedAt
	return writePackageDomainReceipt(ctx, db, f, receipt)
}

func writePackageDomainReceipt(ctx context.Context, db bun.IDB, f packageDomainFence, receipt PackageDomainReceipt) error {
	if !PackageDomainReceiptMatches(f.intent, receipt) {
		return ErrPackageDomainFence
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE package_deployment_continuations SET domain_receipts = domain_receipts || jsonb_build_object(?, ?::jsonb), updated_at = clock_timestamp() WHERE deployment_id = ? AND selection_sha256 = ? AND lease_id = ? AND lease_until > clock_timestamp() AND deadline > clock_timestamp() AND phase = 'publishing' AND NOT cancel_requested AND NOT jsonb_exists(domain_receipts, ?)`, f.intent.Key, string(encoded), f.deployment, f.selection, f.lease, f.intent.Key)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: receipt was not recorded", ErrPackageDomainFence)
	}
	return nil
}

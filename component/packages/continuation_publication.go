package packages

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PreparedSiteBundle contains only immutable object-version receipts, never
// executable build instructions. It is saved before the package can roll its
// own process. No continuation path rebuilds or repeats a site preparation.
type PreparedSiteBundle struct {
	SiteID         string                    `json:"siteId"`
	Target         edge.Target               `json:"target"`
	Version        string                    `json:"version"`
	BundleRef      string                    `json:"bundleRef"`
	Files          []PreparedSiteFile        `json:"files"`
	Binding        *memql.PackageSiteBinding `json:"binding,omitempty"`
	PreviewBinding *memql.PackageSiteBinding `json:"previewBinding,omitempty"`
}
type PreparedSiteFile struct {
	Name    string                 `json:"name"`
	Receipt azureblob.VerifiedBlob `json:"receipt"`
}
type durableSitePublisher interface {
	PrepareBundle(context.Context, string, edge.Bundle, edge.Target) (PreparedSiteBundle, error)
	PublishPrepared(context.Context, PreparedSiteBundle, ActiveSetVersion) (PublishResult, error)
}

// Preparation may create an empty draft to obtain its immutable asset path.
// Existing sites are only read: bindings and domains remain selected intent
// until rollout readiness, never side effects of an upload or checkpoint failure.
func (d *Deps) preparePublications(ctx context.Context, req DeployRequest, pkg map[string]any, rep *Report, bundles map[string]edge.Bundle) ([]DeployableOutcome, []PreparedSiteBundle, []memql.PackagePublicationIntent, []memql.PackageDomainIntent, error) {
	durable, ok := d.Publisher.(durableSitePublisher)
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("package publisher has no durable output support")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	existing, err := d.Store.sitesForPackage(ctx, req.PackageId)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byName := map[string]map[string]any{}
	for _, row := range existing {
		byName[rowString(row, "packageDeployableName")] = row
	}
	outcomes := []DeployableOutcome{}
	prepared := []PreparedSiteBundle{}
	intents := []memql.PackagePublicationIntent{}
	domains := []memql.PackageDomainIntent{}
	seen := map[string]bool{}
	for _, dep := range rep.Deployables {
		if req.Placements[dep.Name].Skip {
			outcomes = append(outcomes, DeployableOutcome{Name: dep.Name, Refusal: &Problem{Code: CodeDeployableSkipped, Message: "You chose not to deploy this one. Nothing was built for it, and anything it already serves is untouched.", Scope: dep.Name}})
			continue
		}
		bundle, ok := bundles[dep.Name]
		if !ok {
			if dep.Problem != nil && !dep.Problem.Fatal {
				note := *dep.Problem
				outcomes = append(outcomes, DeployableOutcome{Name: dep.Name, Refusal: &note})
			}
			continue
		}
		placement := manifestPlacement(dep.Deployment, req.Placements[dep.Name])
		siteID := rowString(byName[dep.Name], "id")
		created := false
		if siteID == "" {
			if strings.TrimSpace(placement.Hostname) == "" {
				return nil, nil, nil, nil, refuseScoped(CodeDeployableHostnameUnchosen, dep.Name, "deployable %q has never been deployed and no hostname was chosen for it. The first deploy picks a hostname; later ones remember it.", dep.Name)
			}
			account := firstNonEmpty(strings.TrimSpace(req.Placements[dep.Name].AccountId), rowString(pkg, "accountId"))
			siteID, _, created, err = d.Publisher.EnsureSite(ctx, EnsureSiteRequest{PackageId: req.PackageId, DeployableName: dep.Name, Kind: dep.Kind, Hostname: strings.TrimSpace(placement.Hostname), AccountId: account, ResolutionTail: dep.ResolutionTail, OwnerUserId: rowString(pkg, "ownerUserId")})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			// EnsureSite may discover a concurrently created existing site. Never
			// mutate it as if it were our draft, or overwrite its version witness.
			if created {
				if err := d.Store.bindSiteToPackage(ctx, siteID, req.PackageId, dep.Name); err != nil {
					return nil, nil, nil, nil, err
				}
			}
		}
		row, err := d.Store.siteById(ctx, siteID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		at, versionErr := continuationRowVersion(row)
		if versionErr != nil || row == nil || !sameShortId(rowString(row, "packageId"), req.PackageId) || rowString(row, "packageDeployableName") != dep.Name || rowString(row, "accountId") == "" {
			return nil, nil, nil, nil, fmt.Errorf("prepared site identity or version differs")
		}
		if created && (rowString(row, "status") != siteStatusDraft || rowString(row, "bundleRef") != "" || boundStoreId(row) != "" || boundStoreId(map[string]any{"binding": row["previewBinding"]}) != "") {
			return nil, nil, nil, nil, fmt.Errorf("new package site is not an empty draft")
		}
		storeID, previewID, note, err := d.resolvePublicationStores(ctx, dep, row, rowString(row, "accountId"))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		target := placement.Target
		if target == "" {
			target = edge.TargetServing
		}
		b, err := durable.PrepareBundle(ctx, siteID, bundle, target)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if storeID != "" && !sameShortId(storeID, boundStoreId(row)) {
			b.Binding = &memql.PackageSiteBinding{StoreID: memql.BareShortId(storeID)}
		}
		if previewID != "" {
			b.PreviewBinding = &memql.PackageSiteBinding{StoreID: memql.BareShortId(previewID)}
		}
		key := b.SiteID + "/" + string(b.Target)
		if seen[key] {
			return nil, nil, nil, nil, fmt.Errorf("package selects the same publication target twice")
		}
		seen[key] = true
		accountID := memql.BareShortId(rowString(row, "accountId"))
		intents = append(intents, memql.PackagePublicationIntent{Key: key, SiteID: b.SiteID, AccountID: accountID, Target: string(b.Target), BundleRef: b.BundleRef, SiteVersion: at, Binding: b.Binding, PreviewBinding: b.PreviewBinding})
		prepared = append(prepared, b)
		outcome := DeployableOutcome{Name: dep.Name, SiteId: b.SiteID, Hostname: rowString(row, "hostname"), AccountId: accountID, Version: b.Version, Created: created, Refusal: note}
		if target == edge.TargetCandidate {
			outcome.CandidateRef = b.BundleRef
		} else {
			outcome.BundleRef = b.BundleRef
		}
		for _, hostname := range placementDomainNames(placement) {
			// Match customDomainAdd's canonical spelling before selecting a
			// stable identity. Invalid manual placements remain nonfatal notes,
			// not checkpoints waiting for a DSL/schema error to become evidence.
			hostname = strings.TrimSuffix(hostname, ".")
			if err := validateDeployment(&ManifestDeployment{Domains: []string{hostname}}); err != nil {
				if outcome.DomainRefusal == nil {
					outcome.DomainRefusal = &Problem{Code: CodeDeployableDomainRefused, Scope: dep.Name, Message: err.Error()}
				} else {
					outcome.DomainRefusal.Message += "; " + err.Error()
				}
				continue
			}
			domainKey := b.SiteID + "/" + hostname
			if seen["domain/"+domainKey] {
				continue
			}
			seen["domain/"+domainKey] = true
			domains = append(domains, memql.PackageDomainIntent{Key: domainKey, PublicationKey: key, DomainID: rand.Text(), SiteID: b.SiteID, AccountID: accountID, Hostname: hostname})
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, prepared, intents, domains, nil
}

// A native shaped query retains its protobuf timestamp; only a wire round trip
// turns it into RFC3339. Decode that metadata without weakening the exact
// row-version fence or accepting missing/malformed versions.
func continuationRowVersion(row map[string]any) (time.Time, error) {
	var at time.Time
	switch value := row["createdAt"].(type) {
	case *timestamppb.Timestamp:
		if value == nil || value.CheckValid() != nil {
			return at, fmt.Errorf("invalid native row version")
		}
		at = value.AsTime()
	case time.Time:
		at = value
	case string:
		var err error
		at, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid row version")
		}
	default:
		return at, fmt.Errorf("missing row version")
	}
	if at.IsZero() {
		return time.Time{}, fmt.Errorf("empty row version")
	}
	if err := timestamppb.New(at).CheckValid(); err != nil {
		return time.Time{}, fmt.Errorf("invalid row version")
	}
	return at.UTC(), nil
}

func (p *enginePublisher) PrepareBundle(ctx context.Context, siteID string, bundle edge.Bundle, target edge.Target) (PreparedSiteBundle, error) {
	siteID = memql.BareShortId(siteID)
	if target != edge.TargetServing && target != edge.TargetCandidate {
		return PreparedSiteBundle{}, fmt.Errorf("unknown package publication target")
	}
	if len(bundle) == 0 || bundle["index.html"] == nil || len(bundle) > DefaultLimits().MaxFileCount {
		return PreparedSiteBundle{}, fmt.Errorf("invalid prepared bundle inventory")
	}
	if err := p.requireSiteAction(ctx, siteID, "deploy"); err != nil {
		return PreparedSiteBundle{}, err
	}
	if target == edge.TargetCandidate {
		if err := p.requireSiteAction(ctx, siteID, "preview"); err != nil {
			return PreparedSiteBundle{}, err
		}
	}
	if err := p.resolve(ctx); err != nil {
		return PreparedSiteBundle{}, err
	}
	names := make([]string, 0, len(bundle))
	for name := range bundle {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	var total int64
	for _, name := range names {
		if !validBundlePath(name) {
			return PreparedSiteBundle{}, fmt.Errorf("invalid prepared bundle path")
		}
		data := bundle[name]
		total += int64(len(data))
		if int64(len(data)) > DefaultLimits().MaxFileBytes || total > DefaultLimits().MaxSourceBytes {
			return PreparedSiteBundle{}, fmt.Errorf("prepared bundle exceeds native limits")
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(data))
		h.Write(data)
	}
	version := "v" + hex.EncodeToString(h.Sum(nil))[:12]
	prefix := "sites/" + siteID + "/" + version + "/"
	result := PreparedSiteBundle{SiteID: siteID, Target: target, Version: version, BundleRef: "blob://" + prefix}
	for _, name := range names {
		data := bundle[name]
		digest := sha256.Sum256(data)
		receipt, err := p.uploader.ClusterAssetsCreateVerifiedStream(ctx, p.container, prefix+name, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(digest[:]), "application/octet-stream")
		if err != nil {
			return PreparedSiteBundle{}, err
		}
		// Provider URLs are not authority and need not be retained in the journal.
		receipt.URL = ""
		result.Files = append(result.Files, PreparedSiteFile{Name: name, Receipt: receipt})
	}
	return result, nil
}

func validBundlePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (p *enginePublisher) PublishPrepared(ctx context.Context, b PreparedSiteBundle, expectedActive ActiveSetVersion) (PublishResult, error) {
	if !memql.HasPackagePublicationFence(ctx) {
		return PublishResult{}, fmt.Errorf("prepared package publish requires its native claim")
	}
	if b.SiteID == "" || b.SiteID != memql.BareShortId(b.SiteID) || b.BundleRef != "blob://sites/"+b.SiteID+"/"+b.Version+"/" || len(b.Files) == 0 || len(b.Files) > DefaultLimits().MaxFileCount {
		return PublishResult{}, fmt.Errorf("prepared bundle identity differs")
	}
	if err := p.requireSiteAction(ctx, b.SiteID, "deploy"); err != nil {
		return PublishResult{}, err
	}
	if err := p.resolve(ctx); err != nil {
		return PublishResult{}, err
	}
	seen := map[string]bool{}
	prefix := strings.TrimPrefix(b.BundleRef, "blob://")
	for _, f := range b.Files {
		if !validBundlePath(f.Name) || seen[f.Name] {
			return PublishResult{}, fmt.Errorf("prepared bundle inventory differs")
		}
		seen[f.Name] = true
		stream, err := p.uploader.OpenReceiptStream(ctx, p.container, prefix+f.Name, f.Receipt)
		if err != nil {
			return PublishResult{}, err
		}
		_, err = io.Copy(io.Discard, stream)
		closeErr := stream.Close()
		if err != nil {
			return PublishResult{}, err
		}
		if closeErr != nil {
			return PublishResult{}, closeErr
		}
	}
	if !seen["index.html"] {
		return PublishResult{}, fmt.Errorf("prepared bundle has no index")
	}
	// Verification can stream a large retained bundle. Re-read the fixed active
	// pointer only AFTER every EOF, immediately before the visible site write.
	observed, err := p.uploader.ReadPackageActiveSet(ctx, p.container)
	if err != nil {
		return PublishResult{}, err
	}
	active, err := activeSetVersion(observed)
	if err != nil {
		return PublishResult{}, err
	}
	if !sameActiveVersion(active, expectedActive) {
		return PublishResult{}, fmt.Errorf("active DSL pointer changed during output verification")
	}
	// A ranked context is a snapshot, not a lasting grant. Resolve the exact
	// original requester again after streaming, before normal account/site
	// guards. Keep the native claim and row-version witnesses on that context.
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access.UserId == "" {
		return PublishResult{}, fmt.Errorf("prepared publication has no original requester")
	}
	ctx = memql.ContextWithFreshAuthorization(ctx)
	role, err := p.store.resolveRole(ctx, access.UserId)
	if err != nil {
		return PublishResult{}, err
	}
	if !auth.CanAuthor(auth.UserContext{Role: role}) {
		return PublishResult{}, refuse(CodeDslRequiresAuthoring, "The original requester can no longer publish this DSL deployment.")
	}
	current := *access
	current.Role = role
	ctx = auth.ContextWithAccess(auth.ContextWithClientOrigin(ctx), &current)
	// The normal package writer repeats original actor/site/account guards after
	// verification. The mutation retains ordinary client-origin authorization.
	writer := packageSiteWriter{publisher: p}
	if err := writer.PointPrepared(ctx, b); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{SiteId: b.SiteID, BundleRef: b.BundleRef, Version: b.Version}, nil
}

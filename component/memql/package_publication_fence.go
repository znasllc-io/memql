package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// PackagePublicationIntent is an immutable native continuation selection. It is
// never accepted from a DSL argument or an editable graph row. The package
// runtime saves it only after retaining the exact built output and site witness.
type PackagePublicationIntent struct {
	Key            string              `json:"key"`
	SiteID         string              `json:"siteId"`
	AccountID      string              `json:"accountId"`
	Target         string              `json:"target"`
	BundleRef      string              `json:"bundleRef"`
	SiteVersion    time.Time           `json:"siteVersion"`
	Binding        *PackageSiteBinding `json:"binding,omitempty"`
	PreviewBinding *PackageSiteBinding `json:"previewBinding,omitempty"`
}

// A nil delta preserves the existing binding. A selected value replaces the
// whole binding under the ordinary store-read and organization store guards.
type PackageSiteBinding struct {
	StoreID string `json:"storeId"`
}

type PackagePublicationReceipt struct {
	SiteID         string              `json:"siteId"`
	Target         string              `json:"target"`
	BundleRef      string              `json:"bundleRef"`
	SiteVersion    time.Time           `json:"siteVersion"`
	Binding        *PackageSiteBinding `json:"binding,omitempty"`
	PreviewBinding *PackageSiteBinding `json:"previewBinding,omitempty"`
}

type packagePublicationFence struct{ deployment, selection, lease, key string }
type packagePublicationFenceKey struct{}

// ContextWithPackagePublicationFence restricts one ordinary authorized site
// mutation to a native continuation's live lease and immutable selection. It
// grants no role, source, account or site authority and carries no callback.
func ContextWithPackagePublicationFence(ctx context.Context, deployment, selection, lease, key string) context.Context {
	return context.WithValue(ContextWithFreshRead(ctx), packagePublicationFenceKey{}, packagePublicationFence{BareShortId(deployment), selection, lease, key})
}

func packagePublicationFenceFrom(ctx context.Context) (packagePublicationFence, bool) {
	f, ok := ctx.Value(packagePublicationFenceKey{}).(packagePublicationFence)
	return f, ok
}

func HasPackagePublicationFence(ctx context.Context) bool {
	_, ok := packagePublicationFenceFrom(ctx)
	return ok
}

var ErrPackagePublicationFence = errors.New("package publication selection or live claim differs")
var ErrPackagePublicationRecorded = errors.New("package publication already recorded; observe its receipt")

// checkPackagePublication runs AFTER normal mutation authorization, in the same
// transaction as the site version and outbox. Locking this native row prevents
// lease replacement or cancellation until the coupled write commits/rolls back.
func checkPackagePublication(ctx context.Context, db bun.IDB, concept, siteID string, prior, payload map[string]any) error {
	f, ok := packagePublicationFenceFrom(ctx)
	if !ok {
		return nil
	}
	ac, present := auth.AccessFromContext(ctx)
	if !present || ac.UserId == "" || concept != conceptPlatformSite || f.deployment == "" || len(f.selection) != 64 || f.lease == "" || f.key == "" {
		return ErrPackagePublicationFence
	}
	var requester, packageID, selection string
	var body, receipts []byte
	err := db.QueryRowContext(ctx, `SELECT requested_by, package_id, selection_sha256, body, publications FROM package_deployment_continuations WHERE deployment_id = ? AND lease_id = ? AND lease_until > clock_timestamp() AND deadline > clock_timestamp() AND phase = 'publishing' AND NOT cancel_requested FOR UPDATE`, f.deployment, f.lease).Scan(&requester, &packageID, &selection, &body, &receipts)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPackagePublicationFence
	}
	if err != nil {
		return err
	}
	if requester != BareShortId(ac.UserId) || selection != f.selection {
		return ErrPackagePublicationFence
	}
	var selected struct {
		Publications []PackagePublicationIntent `json:"publications"`
	}
	if err := json.Unmarshal(body, &selected); err != nil || len(selected.Publications) == 0 || len(selected.Publications) > 128 {
		return ErrPackagePublicationFence
	}
	var intent *PackagePublicationIntent
	seen := make(map[string]bool, len(selected.Publications))
	for n := range selected.Publications {
		p := &selected.Publications[n]
		if p.Key == "" || seen[p.Key] {
			return ErrPackagePublicationFence
		}
		seen[p.Key] = true
		if p.Key == f.key {
			intent = p
		}
	}
	if intent == nil || intent.SiteID != BareShortId(siteID) || intent.AccountID == "" || intent.SiteVersion.IsZero() || BareShortId(stringFromAny(payload["accountId"])) != intent.AccountID || BareShortId(stringFromAny(payload["packageId"])) != packageID {
		return ErrPackagePublicationFence
	}
	field := ""
	switch intent.Target {
	case "serving":
		field = "bundleRef"
	case "candidate":
		field = "candidateRef"
	default:
		return ErrPackagePublicationFence
	}
	if !strings.HasPrefix(intent.BundleRef, "blob://sites/"+intent.SiteID+"/") || stringFromAny(payload[field]) != intent.BundleRef {
		return ErrPackagePublicationFence
	}
	// The version witness protects the prior row; the immutable selection
	// protects its exact delta. Omitted bindings must remain unchanged, and a
	// native fence cannot be attached to a broader raw site edit.
	expected := maps.Clone(prior)
	if expected == nil {
		return ErrPackagePublicationFence
	}
	expected[field] = intent.BundleRef
	for name, delta := range map[string]*PackageSiteBinding{"binding": intent.Binding, "previewBinding": intent.PreviewBinding} {
		if delta != nil {
			if delta.StoreID == "" || delta.StoreID != BareShortId(delta.StoreID) {
				return ErrPackagePublicationFence
			}
			expected[name] = map[string]any{"storeId": delta.StoreID}
		}
	}
	if !reflect.DeepEqual(expected, payload) {
		return ErrPackagePublicationFence
	}
	witness := false
	for _, v := range rowVersionFences(ctx) {
		if v.concept == conceptPlatformSite && BareShortId(v.id) == intent.SiteID && v.version.Equal(intent.SiteVersion) {
			witness = true
		}
	}
	if !witness {
		return ErrPackagePublicationFence
	}
	var completed map[string]PackagePublicationReceipt
	if json.Unmarshal(receipts, &completed) != nil {
		return ErrPackagePublicationFence
	}
	if _, exists := completed[f.key]; exists {
		return ErrPackagePublicationRecorded
	}
	return nil
}

func recordPackagePublication(ctx context.Context, db bun.IDB, created memorynodes.Node) error {
	f, ok := packagePublicationFenceFrom(ctx)
	if !ok {
		return nil
	}
	var body struct {
		Publications []PackagePublicationIntent `json:"publications"`
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT body FROM package_deployment_continuations WHERE deployment_id = ? AND lease_id = ? FOR UPDATE`, f.deployment, f.lease).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal(raw, &body) != nil {
		return ErrPackagePublicationFence
	}
	for _, p := range body.Publications {
		if p.Key != f.key {
			continue
		}
		encoded, err := json.Marshal(PackagePublicationReceipt{SiteID: p.SiteID, Target: p.Target, BundleRef: p.BundleRef, SiteVersion: created.CreatedAt, Binding: p.Binding, PreviewBinding: p.PreviewBinding})
		if err != nil {
			return err
		}
		result, err := db.ExecContext(ctx, `UPDATE package_deployment_continuations SET publications = publications || jsonb_build_object(?, ?::jsonb), updated_at = clock_timestamp() WHERE deployment_id = ? AND lease_id = ? AND lease_until > clock_timestamp() AND deadline > clock_timestamp() AND phase = 'publishing' AND NOT cancel_requested AND NOT jsonb_exists(publications, ?)`, f.key, string(encoded), f.deployment, f.lease, f.key)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrPackagePublicationFence
		}
		return nil
	}
	return fmt.Errorf("package publication receipt has no selected target")
}

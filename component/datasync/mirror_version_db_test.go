package datasync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror_version_db_test.go -- the version guard against what the engine
// really stores (D6).
//
// Every guard test in inbound_test.go answers StoredVersion from fakeWriter's
// table, so none of them ever read a row. The real reader asks the engine a
// raw concept read, and that answers RAW BUNDLE ROWS: the intrinsics at the
// top level (`created_at` as a protobuf timestamp) and the concept's fields
// under `payload`. StoredVersion read `updatedAt` off the top level, found
// nothing on every row, reported every row as "exists, no version", and the
// guard applied every late delivery it was built to refuse. This writes the
// row the way the Applier does and asks the question the way the Applier
// does.

const versionGuardConcept = "v1:shopify:product"

// versionGuardSpecs is the DomainSpec the Shopify connector declares for every
// mirrored concept: the origin's own version, kept in the payload's updatedAt.
func versionGuardSpecs() map[string]memqlsync.DomainSpec {
	return map[string]memqlsync.DomainSpec{
		versionGuardConcept: {Concept: versionGuardConcept, VersionField: "updatedAt", Direction: memqlsync.DirectionInbound},
	}
}

func versionGuardWrite(rowID, version, title string) memqlsync.MirrorWrite {
	return memqlsync.MirrorWrite{
		Concept: versionGuardConcept,
		RowId:   rowID,
		Version: version,
		Payload: map[string]any{
			"storeId":   "dbtest-version-guard",
			"gid":       "gid://shopify/Product/" + rowID,
			"updatedAt": version,
			"syncedAt":  time.Now().UTC().Format(time.RFC3339),
			"deleted":   false,
			"title":     title,
		},
	}
}

func TestTheVersionGuardReadsTheOriginsVersionOffTheStoredRow(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	rowID := fmt.Sprintf("shpdbtestversion%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2`,
			versionGuardConcept, "gid://shopify/Product/"+rowID)
	})
	writer := NewEngineMirrorWriter(engineSeam{eng})
	applier := NewApplier(NewStore(engineSeam{eng}), writer)
	// The Dispatcher's own stamp: the connector actor, the only identity a
	// mirror admits a write or a read from.
	ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
	specs := versionGuardSpecs()

	// The origin's version is in August; MemQL writes the row today. The two
	// clocks are months apart on purpose, so a guard reading the row's own
	// createdAt (MemQL's clock) would refuse the newer August delivery below.
	const stored, older, newer = "2026-08-23T12:00:00Z", "2026-08-23T11:00:00Z", "2026-08-23T13:00:00Z"
	if res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, stored, "As stored")}); err != nil || res.Applied != 1 {
		t.Fatalf("seed the mirror row: applied=%d err=%v", res.Applied, err)
	}

	got, exists, err := writer.StoredVersion(ctx, specs[versionGuardConcept], rowID)
	if err != nil || !exists || got != stored {
		t.Fatalf("StoredVersion = (%q, %v, %v), want (%q, true, nil) -- the origin's version from the payload", got, exists, err, stored)
	}

	res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, older, "A late delivery")})
	if err != nil {
		t.Fatalf("Apply the older delivery: %v", err)
	}
	if res.Applied != 0 || res.Stale != 1 {
		t.Fatalf("older delivery: applied=%d stale=%d, want 0/1 -- it would overwrite newer data", res.Applied, res.Stale)
	}
	if got, _, _ := writer.StoredVersion(ctx, specs[versionGuardConcept], rowID); got != stored {
		t.Fatalf("after the refused delivery the stored version is %q, want it untouched at %q", got, stored)
	}

	// EQUAL is not older: a redelivery of the version MemQL holds applies,
	// which is what keeps an identical re-fetch harmless.
	if res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, stored, "The same version again")}); err != nil || res.Applied != 1 || res.Stale != 0 {
		t.Fatalf("equal version: applied=%d stale=%d err=%v, want 1/0", res.Applied, res.Stale, err)
	}
	if res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, newer, "A newer delivery")}); err != nil || res.Applied != 1 || res.Stale != 0 {
		t.Fatalf("newer version: applied=%d stale=%d err=%v, want 1/0 -- compared on the origin's clock, never the row's createdAt", res.Applied, res.Stale, err)
	}
	if got, _, _ := writer.StoredVersion(ctx, specs[versionGuardConcept], rowID); got != newer {
		t.Fatalf("after the newer delivery the stored version is %q, want %q", got, newer)
	}

	// A write that carries no version is last-write-wins, as it was before the
	// guard existed.
	noVersion := versionGuardWrite(rowID, newer, "No version on the write")
	noVersion.Version = ""
	if res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{noVersion}); err != nil || res.Applied != 1 {
		t.Fatalf("a write with no version: applied=%d err=%v, want it applied", res.Applied, err)
	}

	// A domain that declares no version field cannot be ordered against this
	// row, so an older delivery for it still applies.
	unversioned := map[string]memqlsync.DomainSpec{versionGuardConcept: {Concept: versionGuardConcept, Direction: memqlsync.DirectionInbound}}
	if res, err := applier.Apply(ctx, "shopify", unversioned, []memqlsync.MirrorWrite{versionGuardWrite(rowID, older, "No version field")}); err != nil || res.Applied != 1 {
		t.Fatalf("a domain with no version field: applied=%d err=%v, want it applied", res.Applied, err)
	}
}

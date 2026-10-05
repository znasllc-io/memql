package datasync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// THE STALENESS READ NAMES ITS ROW AS A LITERAL (memql#5625's sweep).
//
// StoredVersion read the row it guards with `concept==C && id==<rowId>`, the
// id pasted in BARE, while WriteMirror two functions above quotes the same id.
// A row id is the connector's to mint and the contract types it as a string:
// the Shopify connector happens to mint `shp<hash>`, which survives the bare
// form, but an id carrying a quote or a space does not. Such a row WRITES
// fine and then cannot be read back -- and the read runs before every write
// that carries a version (isStale), so the error fails the whole apply at
// that row, every time it is delivered.
//
// Through the real engine for the reason syncstate_write_db_test.go gives:
// the defect is in the statement the engine is handed, which a fake writer
// would accept.
func TestStoredVersionReadsBackARowWhateverItsIdSpells(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	writer := NewEngineMirrorWriter(engineSeam{eng})
	ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
	spec := memqlsync.DomainSpec{Concept: "v1:shopify:product", VersionField: "updatedAt"}
	run := time.Now().UnixNano()

	for _, rowID := range []string{
		fmt.Sprintf(`dbtest%dquote"d`, run),
		fmt.Sprintf(`dbtest%d spaced`, run),
	} {
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(),
				`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:product' AND id = $1`, "v1:shopify:product:"+rowID)
		})
		version := "2026-09-27T12:00:00Z"
		err := writer.WriteMirror(ctx, "shopify", memqlsync.MirrorWrite{
			Concept: spec.Concept,
			RowId:   rowID,
			Payload: map[string]any{
				"storeId":   "dbtest-store",
				"gid":       "gid://shopify/Product/" + rowID,
				"updatedAt": version,
				"syncedAt":  version,
				"deleted":   false,
			},
		})
		if err != nil {
			t.Fatalf("WriteMirror(%q): %v", rowID, err)
		}

		_, found, err := writer.StoredVersion(ctx, spec, rowID)
		if err != nil {
			t.Fatalf("StoredVersion(%q) failed: %v -- the row exists, so the read statement itself is broken", rowID, err)
		}
		if !found {
			t.Fatalf("StoredVersion(%q) found nothing for a row it has just written", rowID)
		}
	}
}

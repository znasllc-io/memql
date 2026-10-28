package packages

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestFetchArtifactRetainsAuthorizedSnapshot(t *testing.T) {
	raw := zipOf(t, validPackage())
	const artifactID = "v1:library:artifact:uploaded-source"
	ctx := callerCtx("v1:identity:user:artifact-reader")
	denied := errors.New("artifact is not readable by this caller")
	for _, allowed := range []bool{true, false} {
		name := "allowed"
		if !allowed {
			name = "denied"
		}
		t.Run(name, func(t *testing.T) {
			reads := 0
			fetcher := &githubFetcher{artifactBytes: func(readCtx context.Context, id string) ([]byte, string, error) {
				reads++
				if id != artifactID || actorFromContext(readCtx).UserId != actorFromContext(ctx).UserId {
					t.Fatal("artifact read changed the selected identity or caller")
				}
				if !allowed {
					return nil, "", denied
				}
				return raw, "application/zip", nil
			}}
			snapshot, err := fetcher.FetchArtifact(ctx, artifactID, DefaultLimits())
			if reads != 1 {
				t.Fatalf("artifact reads = %d, want 1", reads)
			}
			if !allowed {
				if snapshot != nil || !errors.Is(err, denied) {
					t.Fatalf("unreadable source produced a snapshot: %+v %v", snapshot, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			publisher := &fakePublisher{}
			deps := &Deps{Publisher: publisher}
			if snapshot.ArtifactId != artifactID || snapshot.Version != contentHash(raw) || len(snapshot.Bytes) != 0 {
				t.Fatalf("uploaded snapshot identity changed: %+v", snapshot)
			}
			if id := deps.storeSnapshot(ctx, DeployRequest{PackageId: "package"}, "attempt", snapshot); id != artifactID || publisher.snapshots != 0 {
				t.Fatalf("stored artifact was copied or lost: id=%q uploads=%d", id, publisher.snapshots)
			}
		})
	}
}

func TestPackageArtifactContinuationUsesUploadedSnapshot(t *testing.T) {
	for _, replaceSource := range []bool{false, true} {
		name := "continue-on-another-replica"
		if replaceSource {
			name = "refuse-replaced-artifact"
		}
		t.Run(name, func(t *testing.T) {
			c := newContinuationFixture(t)
			f := c.base
			db, err := f.deps[0].Store.continuationDB()
			if err != nil {
				t.Fatal(err)
			}
			artifactID := "v1:library:artifact:" + memql.BareShortId(f.pkg)
			// Keep the real account/site/attempt fixture, but make its selected
			// source an uploaded ZIP before confirmation. The actual fetcher,
			// report write and checkpoint run below; no repo snapshot is minted.
			if _, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=payload || jsonb_build_object('sourceKind','artifact','artifactId',$2::text) WHERE id=$1`, f.pkg, artifactID); err != nil {
				t.Fatal(err)
			}
			f.ctx = memql.ContextWithFreshRead(f.ctx)
			raw := zipOf(t, validPackage())
			reads := 0
			fetcher := &githubFetcher{artifactBytes: func(ctx context.Context, id string) ([]byte, string, error) {
				reads++
				if !sameShortId(id, artifactID) || actorFromContext(ctx).UserId != actorFromContext(f.ctx).UserId {
					t.Fatal("artifact fetch changed the source or requester")
				}
				return raw, "application/zip", nil
			}}
			for _, deps := range f.deps {
				deps.Fetcher = fetcher
			}
			uploadsBefore := f.h.publisher.snapshots
			c.start(t)
			record, err := f.deps[1].Store.readContinuation(f.ctx, f.run)
			if err != nil || record == nil {
				t.Fatalf("artifact checkpoint: %+v %v", record, err)
			}
			var selectedArtifact string
			if err := json.Unmarshal([]byte(record.Selection.Source["artifactId"]), &selectedArtifact); err != nil {
				t.Fatalf("checkpoint omitted artifact source identity: %v", err)
			}
			if !sameShortId(selectedArtifact, artifactID) || !sameShortId(record.Selection.SnapshotID, artifactID) || record.Selection.SourceVersion != contentHash(raw) {
				t.Fatalf("checkpoint did not bind the selected ZIP and bytes: %+v", record.Selection)
			}
			if reads != 1 || f.h.publisher.snapshots != uploadsBefore || len(f.h.publisher.snapshotReads) != 0 {
				t.Fatal("artifact checkpoint refetched, uploaded a copy or used the repository snapshot reader")
			}
			if replaceSource {
				if _, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{artifactId}',to_jsonb($2::text)) WHERE id=$1`, f.pkg, artifactID+"-replacement"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.deps[1].continuationAuthority(f.ctx, record); err == nil || !strings.Contains(err.Error(), "source or owner changed") {
					t.Fatalf("replacement artifact retained continuation authority: %v", err)
				}
			}
			c.roller.ready = true
			c.resume(t)
			if reads != 1 || c.roller.calls != 1 || len(f.h.builder.built) != 1 {
				t.Fatal("artifact recovery fetched, rebuilt or restarted the selected deployment again")
			}
			if replaceSource {
				if c.publishers[1].published.Load() != 0 || rowString(c.row(t), "status") == StatusSucceeded {
					t.Fatal("replaced artifact source published the old selection")
				}
			} else if c.publishers[1].published.Load() != 1 || rowString(c.row(t), "status") != StatusSucceeded {
				t.Fatal("uploaded artifact did not complete on the second replica")
			}
		})
	}
}

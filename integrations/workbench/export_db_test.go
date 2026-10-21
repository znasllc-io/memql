package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

func exportDBEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "Workbench export", dbtest.DSN(), err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e
}

type exportTestStorage struct {
	objects map[string][]byte
	writes  int
}

func (s *exportTestStorage) Upload(context.Context, string, string, []byte, string) (string, error) {
	panic("binary export must stream")
}
func (s *exportTestStorage) CreateVerifiedStream(_ context.Context, _, key string, r io.Reader, size int64, digest, _ string) (azureblob.VerifiedBlob, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return azureblob.VerifiedBlob{}, err
	}
	h := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(h[:]) != digest {
		return azureblob.VerifiedBlob{}, fmt.Errorf("invalid bytes")
	}
	if old, ok := s.objects[key]; ok && !bytes.Equal(old, data) {
		return azureblob.VerifiedBlob{}, fmt.Errorf("immutable blob changed")
	}
	s.objects[key] = data
	s.writes++
	return azureblob.VerifiedBlob{URL: key, Size: size, SHA256: digest, ETag: `"test"`}, nil
}

type exportTestFleet struct {
	ws    *workspace
	reads int
}

func (f *exportTestFleet) DispatchFleetWorkbench(ctx context.Context, req FleetRequest) (FleetResult, bool) {
	f.reads++
	if req.Action != "fs_read" {
		return FleetResult{ErrorMessage: "only reads are allowed"}, true
	}
	r := (&Integration{}).handleFSRead(ctx, f.ws, req.Args)
	return FleetResult{OK: r.OK, Payload: r.Payload, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMsg}, true
}

func TestExportDBReplicaRecoveryAndOwnerAdmission(t *testing.T) {
	producer, indexer := exportDBEngine(t), exportDBEngine(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, run := "export-owner-"+suffix, "export-run-"+suffix
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	seed := auth.ContextWithInternalOrigin(ctx)
	payload, _ := json.Marshal(map[string]any{"ownerUserId": owner, "status": "running", "automationName": "export-test", "templateFingerprint": "export-test-v1", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339), "routing": map[string]any{"destination": "auto"}})
	if _, err := producer.Execute(seed, fmt.Sprintf(`insert("v1:work:run", id=%q, payload=%s)`, run, payload)); err != nil {
		t.Fatal(err)
	}
	fleet := &exportTestFleet{ws: &workspace{rootPath: t.TempDir()}}
	data := bytes.Repeat([]byte{0, 255, 13, 10, 128}, 150000)
	if err := os.WriteFile(filepath.Join(fleet.ws.rootPath, "test.tar.gz"), data, 0600); err != nil {
		t.Fatal(err)
	}
	blob := &exportTestStorage{objects: map[string][]byte{}}
	makeIntegration := func(e *memql.MemQLEngine) *Integration {
		i := NewIntegration(nil)
		i.SetEngine(e)
		i.SetFleetDispatcher(fleet)
		i.SetAttachmentUploader(blob, "library")
		return i
	}
	i := makeIntegration(producer)
	// Exercise promotion on a different engine/connection, as the installed
	// indexFileOnCreate automation does. It can see only a committed file row.
	indexCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	indexed := make(chan error, 1)
	go func() {
		for {
			res, err := indexer.Execute(ctx, fmt.Sprintf(`query libraryFilesForOwner(runId:%q)`, run))
			if err != nil {
				indexed <- err
				return
			}
			rows := memql.MaterializeRows(res)
			if len(rows) > 0 {
				file := rows[0]
				id := fmt.Sprint(file["id"])
				if !strings.HasPrefix(id, "v1:library:file:") {
					id = "v1:library:file:" + id
				}
				_, err = indexer.Execute(seed, fmt.Sprintf(`mutation createArtifact(sourceConceptRef:%q, ownerUserId:%q, lens:"artifact", kind:"file", source:"agent_generated", title:"test.tar.gz", format:"other", producedByRunId:%q)`, id, owner, run))
				indexed <- err
				return
			}
			select {
			case <-indexCtx.Done():
				indexed <- indexCtx.Err()
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	args := map[string]any{"runId": run, "action": "fs_export", "args": map[string]any{"path": "test.tar.gz"}}
	first, err := i.exportFile(ctx, args)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = <-indexed; err != nil {
		t.Fatal(err)
	}
	second, err := makeIntegration(indexer).exportFile(ctx, args)
	if err != nil || first["fileId"] != second["fileId"] || first["downloadUrl"] != second["downloadUrl"] || len(blob.objects) != 1 {
		t.Fatalf("recovery: %v %v %v", first, second, err)
	}
	for _, saved := range blob.objects {
		if !bytes.Equal(saved, data) {
			t.Fatal("export changed bytes")
		}
	}
	reads := fleet.reads
	if _, err = i.exportFile(auth.ContextWithUserActor(context.Background(), owner+"-stranger"), args); err == nil {
		t.Fatal("export accepted another owner")
	}
	if fleet.reads != reads {
		t.Fatal("read private bytes before admission")
	}
}

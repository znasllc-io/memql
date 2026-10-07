package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

func TestPipelineScopeDBFenceBeforeFirstUploadSurvivesReplacement(t *testing.T) {
	u := &pipelineStreamFake{}
	one, _, _ := pipelineStreamDBStore(t, u)
	two, _, db := pipelineStreamDBStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "data")
	ctx, scope := pipelineLifecycleScope(f)
	if _, err := two.ReadFencedRunFileIntents(ctx, scope, ""); err == nil {
		t.Fatal("unfenced inventory could be mistaken for complete cleanup")
	}
	if err := one.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	// Simulate a lost successful response and retry on an independent engine.
	if err := two.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal("replacement fence", err)
	}
	if got, err := two.ReadFencedRunFileIntents(ctx, scope, ""); err != nil || len(got) != 0 {
		t.Fatalf("empty closed scope: %v %v", got, err)
	}
	for _, writer := range []*pipelinesLibraryStore{one, two} {
		f.Body = strings.NewReader("data")
		if _, err := writer.StoreRunFileStream(context.Background(), f); !errors.Is(err, errPipelineScopeFenced) {
			t.Fatalf("late writer crossed producer fence: %v", err)
		}
	}
	if u.writes != 0 {
		t.Fatal("rejected writer contacted provider")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM pipeline_artifact_scope_fences WHERE owner_user_id=$1", scope.OwnerUserID).Scan(&count); err != nil || count != 1 {
		t.Fatal("lost-response retry changed fence cardinality", count, err)
	}
	// An explicit different scope is independent; no prefix matching or owner-
	// wide cancellation is permitted when closing one producing step/attempt.
	for _, change := range []string{"owner", "run", "step", "attempt"} {
		g := f
		g.Body = strings.NewReader("data")
		switch change {
		case "owner":
			g.OwnerUserID = id.NewShortId()
		case "run":
			g.WorkRunID = id.NewShortId()
		case "step":
			g.StepKey += "/other"
		case "attempt":
			g.Attempt++
		}
		if _, err := two.StoreRunFileStream(context.Background(), g); err != nil {
			t.Fatalf("unrelated %s scope rejected: %v", change, err)
		}
	}
}

type pipelineScopeLostUploadReply struct{ pipelineRetirementFake }

func (u *pipelineScopeLostUploadReply) CreateVerifiedStream(ctx context.Context, bucket, object string, body io.Reader, size int64, digest, mime string) (azureblob.VerifiedBlob, error) {
	if _, err := u.pipelineStreamFake.CreateVerifiedStream(ctx, bucket, object, body, size, digest, mime); err != nil {
		return azureblob.VerifiedBlob{}, err
	}
	return azureblob.VerifiedBlob{}, errors.New("lost response after provider commit")
}

func TestPipelineScopeDBDiscoversLostUploadAndRetiresWithoutReceipt(t *testing.T) {
	u := &pipelineScopeLostUploadReply{}
	one, _, _ := pipelineStreamDBStore(t, u)
	two, _, db := pipelineStreamDBStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "data")
	if got, err := one.StoreRunFileStream(context.Background(), f); err == nil || got.Receipt != nil {
		t.Fatal("injected lost upload response returned a receipt", got, err)
	}
	ctx, scope := pipelineLifecycleScope(f)
	if err := two.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	ids, err := two.ReadFencedRunFileIntents(ctx, scope, "")
	if err != nil || len(ids) != 1 {
		t.Fatal("lost upload absent from inventory", ids, err)
	}
	var state string
	if err := db.QueryRow("SELECT state FROM pipeline_artifact_uploads WHERE intent_id=$1", ids[0]).Scan(&state); err != nil || state != "reserved" {
		t.Fatal("fixture did not lose receipt before verification journal", state, err)
	}
	if _, err := two.RetireRunFile(ctx, scope, ids[0]); err != nil {
		t.Fatal("retire discovered intent", err)
	}
	f.Body = strings.NewReader("data")
	if _, err := one.StoreRunFileStream(context.Background(), f); !errors.Is(err, errPipelineScopeFenced) || u.writes != 1 {
		t.Fatal("old producer resumed after retirement", err, u.writes)
	}
	if again, err := one.ReadFencedRunFileIntents(ctx, scope, ""); err != nil || !slices.Equal(ids, again) {
		t.Fatal("retirement removed evidence from cleanup recovery", again, err)
	}
}

func TestPipelineScopeDBFenceSerializesConcurrentReservationsAcrossReplicas(t *testing.T) {
	one, _, db1 := pipelineStreamDBStore(t, &pipelineStreamFake{})
	two, _, db2 := pipelineStreamDBStore(t, &pipelineStreamFake{})
	for range 4 {
		f := pipelineStreamFile(id.NewShortId(), "data")
		ctx, scope := pipelineLifecycleScope(f)
		identity, err := one.streamIdentity(f)
		if err != nil {
			t.Fatal(err)
		}
		first, err := one.reserveStream(ctx, db1, identity)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{first.ID}
		type admission struct {
			key string
			err error
		}
		start := make(chan struct{})
		results := make(chan admission, 12)
		stopped := make(chan error, 1)
		for n := range 12 {
			go func() {
				<-start
				x := identity
				x.Path = fmt.Sprintf("output/racing-%d", n)
				writer, db := one, db1
				if n%2 == 0 {
					writer, db = two, db2
				}
				intent, err := writer.reserveStream(ctx, db, x)
				results <- admission{intent.ID, err}
			}()
		}
		go func() { <-start; stopped <- two.FenceRunFileScope(ctx, scope) }()
		close(start)
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		for range 12 {
			r := <-results
			if r.err == nil {
				want = append(want, r.key)
			} else if !errors.Is(r.err, errPipelineScopeFenced) {
				t.Fatal(r.err)
			}
		}
		slices.Sort(want)
		got, err := one.ReadFencedRunFileIntents(ctx, scope, "")
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("inventory lost an admitted producer: got=%v want=%v err=%v", got, want, err)
		}
		// Both recovery of an earlier reservation and a fresh path must refuse.
		for _, path := range []string{identity.Path, "output/after-fence"} {
			identity.Path = path
			if _, err := one.reserveStream(ctx, db1, identity); !errors.Is(err, errPipelineScopeFenced) {
				t.Fatal("post-fence admission", err)
			}
		}
	}
}

func TestPipelineScopeDBInventoryPagesAndAuthorization(t *testing.T) {
	s, _, db := pipelineStreamDBStore(t, &pipelineStreamFake{})
	f := pipelineStreamFile(id.NewShortId(), "data")
	ctx, scope := pipelineLifecycleScope(f)
	identity, err := s.streamIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for n := range pipelineScopeIntentPageSize + 3 {
		identity.Path = fmt.Sprintf("output/%d", n)
		intent, err := s.reserveStream(ctx, db, identity)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, intent.ID)
	}
	for _, bad := range []context.Context{context.Background(), auth.ContextWithUserActor(context.Background(), scope.OwnerUserID), auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), id.NewShortId()))} {
		if err := s.FenceRunFileScope(bad, scope); err == nil {
			t.Fatal("unauthorized fence")
		}
		if _, err := s.ReadFencedRunFileIntents(bad, scope, ""); err == nil {
			t.Fatal("unauthorized inventory")
		}
	}
	if err := s.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	var got []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 3 {
			t.Fatal("inventory never ended")
		}
		ids, err := s.ReadFencedRunFileIntents(ctx, scope, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > pipelineScopeIntentPageSize {
			t.Fatal("unbounded inventory")
		}
		if len(ids) == 0 {
			break
		}
		got = append(got, ids...)
		cursor = ids[len(ids)-1]
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatal("paginated inventory omitted or repeated intents")
	}
	for _, bad := range []string{"bad", strings.Repeat("A", 64)} {
		if _, err := s.ReadFencedRunFileIntents(ctx, scope, bad); err == nil {
			t.Fatal("invalid cursor admitted")
		}
	}
	key, _ := pipelineScopeKey(scope)
	if _, err := db.Exec("UPDATE pipeline_artifact_scope_fences SET scope=jsonb_set(scope,'{Attempt}','2') WHERE scope_key=$1", key); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceRunFileScope(ctx, scope); err == nil {
		t.Fatal("corrupt scope silently repaired")
	}
	if _, err := s.ReadFencedRunFileIntents(ctx, scope, ""); err == nil {
		t.Fatal("corrupt scope returned inventory")
	}
	if _, err := s.reserveStream(ctx, db, identity); err == nil {
		t.Fatal("corrupt scope admitted writer")
	}
}

func TestPipelineScopeDBDownMigrationRefusesDurableFences(t *testing.T) {
	s, _, db := pipelineStreamDBStore(t, &pipelineStreamFake{})
	f := pipelineStreamFile(id.NewShortId(), "data")
	ctx, scope := pipelineLifecycleScope(f)
	if err := s.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("../component/database/memory-nodes/migrations/20261007130000_pipeline_artifact_scope_fences.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, string(body)); err == nil || !strings.Contains(err.Error(), "cannot remove artifact producer fences") {
		t.Fatal("down migration forgot producer history", err)
	}
}

func TestPipelineScopeDBRejectsOlderWriterAdmissionAndRecovery(t *testing.T) {
	s, _, db := pipelineStreamDBStore(t, &pipelineRetirementFake{})
	f := pipelineStreamFile(id.NewShortId(), "data")
	ctx, scope := pipelineLifecycleScope(f)
	identity, err := s.streamIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := s.reserveStream(ctx, db, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FenceRunFileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	// These are the former adapter's actual admission/recovery statements,
	// deliberately bypassing the new Go fence check. Rolling replicas must
	// still be unable to add an artifact after cleanup's inventory is sealed.
	identity.Path = "output/legacy-delayed"
	payload, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO pipeline_artifact_uploads
(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES ($1,$2,$3::jsonb,$4,$5,$6,1,'reserved')`, pipelineIntentID(identity), identity.OwnerUserID, string(payload), id.NewShortId(), "legacy/"+id.NewShortId(), identity.Size)
	if err == nil || !strings.Contains(err.Error(), "permanently fenced") {
		t.Fatal("older writer created a post-fence intent", err)
	}
	_, err = db.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET generation=generation+1 WHERE intent_id=$1`, reserved.ID)
	if err == nil || !strings.Contains(err.Error(), "permanently fenced") {
		t.Fatal("older writer recovered a fenced producer", err)
	}
	if _, err := s.RetireRunFile(ctx, scope, reserved.ID); err != nil {
		t.Fatal("provider retirement blocked by admission guard", err)
	}
}

func TestPipelineScopeDBLegacyInsertWaitsForNativeFenceCommit(t *testing.T) {
	s, _, db := pipelineStreamDBStore(t, &pipelineStreamFake{})
	f := pipelineStreamFile(id.NewShortId(), "data")
	owner, scope := pipelineLifecycleScope(f)
	ctx, cancel := context.WithTimeout(owner, 10*time.Second)
	defer cancel()
	identity, err := s.streamIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Rollback()
	if err := lockPipelineUploadOwner(ctx, fence, scope.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	legacy, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Rollback()
	var pid int
	if err := legacy.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(identity)
	done := make(chan error, 1)
	go func() {
		_, err := legacy.ExecContext(ctx, `INSERT INTO pipeline_artifact_uploads
(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES ($1,$2,$3::jsonb,$4,$5,$6,1,'reserved')`, pipelineIntentID(identity), identity.OwnerUserID, string(body), id.NewShortId(), "legacy/"+id.NewShortId(), identity.Size)
		done <- err
	}()
	// Observe the actual database wait, not elapsed time. This proves that the
	// SQL trigger uses the same cross-replica lock as the native admission path.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("legacy admission did not wait for the native owner lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	key, payload := pipelineScopeKey(scope)
	if _, err := fence.ExecContext(ctx, "INSERT INTO pipeline_artifact_scope_fences(scope_key,owner_user_id,scope) VALUES($1,$2,$3::jsonb)", key, scope.OwnerUserID, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := fence.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "permanently fenced") {
		t.Fatal("legacy statement missed fence committed while waiting", err)
	}
}

package identity

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

type concurrentGithubGrantEngine struct {
	mu      sync.Mutex
	ids     map[string]string
	creates int
}

var grantIDArg = regexp.MustCompile(`credentialId: "([^"]+)"`)
var grantExternalArg = regexp.MustCompile(`externalId: "([^"]+)"`)

func (e *concurrentGithubGrantEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	owner := ""
	if ac != nil {
		owner = ac.UserId
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if match := grantExternalArg.FindStringSubmatch(q); len(match) > 1 {
		key := owner + "/" + match[1]
		if id := grantIDArg.FindStringSubmatch(q); len(id) > 1 {
			e.ids[key] = id[1]
			e.creates++
			return &memqlengine.ExecuteResult{}, nil
		}
		existing := e.ids[key]
		if existing == "" {
			time.Sleep(20 * time.Millisecond)
			return &memqlengine.ExecuteResult{}, nil
		}
		payload, _ := structpb.NewStruct(map[string]any{"id": existing})
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: existing, Payload: payload}}}}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func TestConcurrentGitHubGrantCallbacksOnTwoReplicasKeepOneRow(t *testing.T) {
	db := openDirectDBForGithubConnect(t)
	if db == nil {
		return
	}
	engine := &concurrentGithubGrantEngine{ids: map[string]string{}}
	replicas := []*Store{{Engine: engine, DirectDB: func() *sql.DB { return db }}, {Engine: engine, DirectDB: func() *sql.DB { return db }}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var created atomic.Int32
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			id, newRow, err := replicas[n%2].UpsertGithubAppGrant(context.Background(), GithubAppGrant{OwnerUserId: "same-owner", ExternalId: "5150", Login: "account"})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
			if newRow {
				created.Add(1)
			}
		}(n)
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 || created.Load() != 1 || engine.creates != 1 {
		t.Fatalf("rows=%d initial creates=%d writes=%d", len(unique), created.Load(), engine.creates)
	}
	// Another GitHub identity is additive; reconnect never occupies another key.
	for n, grant := range []GithubAppGrant{{OwnerUserId: "same-owner", ExternalId: "9001"}, {OwnerUserId: "another-owner", ExternalId: "5150"}} {
		_, made, err := replicas[n].UpsertGithubAppGrant(context.Background(), grant)
		if err != nil || !made {
			t.Fatal(fmt.Sprintf("independent grant: created=%v error=%v", made, err))
		}
	}
	if engine.creates != 3 {
		t.Fatalf("distinct owner/provider pairs collapsed: %d", engine.creates)
	}
}

func TestGitHubLifecycleFailsClosedWithoutSharedGate(t *testing.T) {
	engine := &concurrentGithubGrantEngine{ids: map[string]string{}}
	store := &Store{Engine: engine}
	if _, _, err := store.UpsertGithubAppGrant(context.Background(), GithubAppGrant{OwnerUserId: "owner", ExternalId: "5150"}); err == nil {
		t.Fatal("grant written without shared gate")
	}
	if _, err := store.ConsumeGithubConnectState(context.Background(), "state", ""); err == nil {
		t.Fatal("state consumed without shared gate")
	}
	if engine.creates != 0 {
		t.Fatal("unlocked grant write occurred")
	}
}

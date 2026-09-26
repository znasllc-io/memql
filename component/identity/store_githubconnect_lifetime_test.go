package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// A STATE PLANTED BEFORE THE WRITE GUARD SHIPPED (memql#5623, review of
// Connect Shopify PR 6).
//
// The executeWrite guard stops a signed-in caller writing a connect state
// from now on. It cannot reach back: a row planted while the hole was open is
// still in the table, unspent, and its expiresAt is whatever the planter
// chose -- the forgery that proved the hole used the year 2099. Consume
// checked only that expiresAt, so such a row would still finish a Connect or a
// GitHub App setup after the fix deployed.
//
// Every server writer sets expiresAt to createdAt plus ten minutes, so a row
// whose lifetime is longer than that, or that has no expiry at all, is one the
// server never wrote. Consume answers it exactly as it answers an unknown
// digest, and leaves it unspent.

// lifetimeFakeEngine answers the state lookup with one row and records whether
// the consume write was reached.
type lifetimeFakeEngine struct {
	mu       sync.Mutex
	row      map[string]string
	consumed bool
}

func (f *lifetimeFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(q, "consumeGithubConnectState(") {
		f.consumed = true
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	if strings.Contains(q, "githubConnectStateByHash(") {
		fields := map[string]*structpb.Value{}
		for k, v := range f.row {
			fields[k] = structpb.NewStringValue(v)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: f.row["id"], CreatedAt: fixtureCreatedAt(f.row["createdAt"]), Payload: &structpb.Struct{Fields: fields}}},
		}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func TestConsumeRefusesAStateTheServerCouldNotHaveWritten(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	cases := []struct {
		name      string
		expiresAt string
		wantSpent bool
	}{
		{"the server's own ten-minute state", created.Add(10 * time.Minute).Format(time.RFC3339Nano), true},
		{"a state that expires in 2099", "2099-01-01T00:00:00Z", false},
		{"a state with no expiry at all", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &lifetimeFakeEngine{row: map[string]string{
				"id":        "v1:identity:githubConnectState:s1",
				"userId":    "owner-1",
				"stateHash": "h",
				"purpose":   githubconnect.PurposeAppSetup,
				"createdAt": created.Format(time.RFC3339Nano),
				"expiresAt": tc.expiresAt,
			}}
			store := &Store{Engine: eng, GithubGate: githubUnitGate}

			row, err := store.ConsumeGithubConnectStateFor(context.Background(), "h", "", githubconnect.PurposeAppSetup)

			if tc.wantSpent {
				if err != nil || row == nil {
					t.Fatalf("the server's own state was refused: row=%v err=%v", row, err)
				}
				if !eng.consumed {
					t.Fatal("the server's own state was answered but never spent")
				}
				return
			}
			if !errors.Is(err, ErrGithubConnectStateNotFound) {
				t.Fatalf("a state whose lifetime no server writer sets was answered %v, want "+
					"ErrGithubConnectStateNotFound. A row planted before the write guard shipped "+
					"would still finish a GitHub App setup", err)
			}
			if eng.consumed {
				t.Fatal("the refused state was spent anyway")
			}
		})
	}
}

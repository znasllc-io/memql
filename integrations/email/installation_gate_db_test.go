package email

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestEmailSetupGatesOutliveCancellationAtCommit(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	for _, kind := range []string{"account", "connection"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(emailStateContext(context.Background()))
			defer cancel()
			witness := replicaGatePool(t)()
			key := "email-installation:self-account"
			logical := "cancellation:" + w.account
			if kind == "connection" {
				key = "email-connection:" + emailStateID(logical)
			}
			called := false
			base := w.instances[0].azure.store.engine
			if kind == "account" {
				base = &installationAccountCache{ConfigWriter: base, accountID: w.account + "-cancel"}
			}
			guarded := setupCommitTestStore{ConfigWriter: base, before: func(commit context.Context) {
				called = true
				cancel()
				// Give database/sql the opportunity to roll back a transaction
				// whose lifetime incorrectly follows the canceled request.
				time.Sleep(10 * time.Millisecond)
				if commit.Err() != nil {
					t.Error("commit inherited request cancellation")
				}
				hold, err := memql.AcquireGateHold(context.Background(), witness, 30*time.Millisecond, key)
				if err == nil {
					hold.Release()
					t.Error("peer acquired the gate before the commit finished")
				} else if !memql.IsWriteGateBusy(err) {
					t.Errorf("gate witness: %v", err)
				}
			}}
			store := &connectionStore{engine: guarded, gateDB: replicaGatePool(t)}
			if kind == "account" {
				if _, err := (&installationScope{a: &azureSetup{store: store}}).account(ctx, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				var value map[string]string
				if err := store.change(ctx, logical, &value, func(bool) error { value = map[string]string{"fixture": "synthetic"}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if !called {
				t.Fatal("test did not reach a commit")
			}
			if ctx.Err() == nil {
				t.Fatal("positive control: request was not canceled")
			}
		})
	}
}

type setupCommitTestStore struct {
	ConfigWriter
	before func(context.Context)
}

func (s setupCommitTestStore) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation createClientAccount(") || strings.HasPrefix(query, "mutation storeEmailConnectionState(") {
		if s.before == nil {
			return nil, fmt.Errorf("missing commit observation")
		}
		s.before(ctx)
	}
	return s.ConfigWriter.Execute(ctx, query)
}

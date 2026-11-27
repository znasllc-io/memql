package calls

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// Reproduce the actual two-BFF Questions/media cycle deterministically. Replica B
// already holds the account prefix of its complete media authority set; replica A
// must wait there without taking the room first. The borrowed transaction is a
// test scheduling barrier, not a new production transaction or a retry on deadlock.
func TestQuestionsMediaSignalGateOrdering(t *testing.T) {
	for _, action := range []string{"share", "refresh", "exact-replay"} {
		t.Run(action, func(t *testing.T) {
			cw, key, commerce, start, run := questionFixture(t)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			t.Cleanup(func() {
				_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", mediaSignalConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
			})
			host := *key.result.Session
			_, guest, err := cw.join(start.Call.GuestToken, "Concurrent question author")
			require.NoError(t, err)
			submit := questionRequest(t, cw, guest, run, "question_submit", "gate-submit", "Benign lock-order regression")
			submitted, err := cw.svc.Workflow(ctx, cw.ref, guest.Token, submit)
			require.NoError(t, err)
			selectQuestion := questionRequest(t, cw, host, run, "question_select", "gate-select", "")
			selectQuestion.Question.ID = submitted.QuestionReceipt.ID
			selectQuestion.Question.Revision = 1
			selectQuestion.Question.ExpectedEpoch = run.ControlEpoch
			if action != "share" {
				_, err = cw.svc.Workflow(ctx, cw.ref, host.Token, selectQuestion)
				require.NoError(t, err)
			}
			request := selectQuestion
			if action == "refresh" {
				request = questionRequest(t, cw, host, run, "questions", "", "")
			}

			replica := allowanceReplica(t, cw) // Distinct BFF-shaped gate pool.
			replica.commerce = func() CommerceConfig { return *commerce }
			lease, err := replica.BeginMediaSignal(ctx, start.Join.Pass)
			require.NoError(t, err)
			cw.media.setPresent(start.Call.ID, person(lease.Identity))
			require.NoError(t, lease.Confirm(ctx))
			t.Cleanup(func() { _ = lease.Close(context.Background(), true) })
			room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, start.Call.ID)
			require.NoError(t, err)
			all, err := replica.mediaSignalGateKeys(ctx, *room, mediaSignalBinding{})
			require.NoError(t, err)
			accounts := slices.DeleteFunc(slices.Clone(all), func(key string) bool { return !strings.HasPrefix(key, "calls.account.") })
			require.Len(t, accounts, 2)
			held, err := memql.AcquireGateHold(ctx, replica.gateDB(), 3*time.Second, accounts...)
			require.NoError(t, err)
			defer held.Release()
			type result struct {
				value callrules.WorkflowResult
				err   error
			}
			done := make(chan result, 1)
			go func() { value, err := cw.svc.Workflow(ctx, cw.ref, host.Token, request); done <- result{value, err} }()
			// Wait for PostgreSQL itself to confirm A is waiting at an account;
			// a sleep or goroutine-start signal would not force the failing order.
			require.Eventually(t, func() bool {
				for _, account := range accounts {
					var waiting bool
					err := cw.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=1296387409::oid AND objid=hashtext(?)::oid AND objsubid=2 AND NOT granted)`, account).Scan(&waiting)
					if err == nil && waiting {
						return true
					}
				}
				return false
			}, 3*time.Second, 10*time.Millisecond, "Questions must reach the controlled account gate wait")
			forwarded := false
			forwardErr := lease.Forward(held.Context(ctx), func(context.Context) error { forwarded = true; return nil })
			held.Release()
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("Questions did not finish after the media authority gate was released")
			}
			require.NoError(t, forwardErr, "native media forwarding must not deadlock with the Questions room gate")
			require.True(t, forwarded)
			require.NoError(t, got.err, "Questions must not deadlock waiting for media account authority")
			require.NotNil(t, got.value.Questions)
			require.NotNil(t, got.value.Questions.Selected)
			require.Equal(t, submitted.QuestionReceipt.ID, got.value.Questions.Selected.ID)
			require.Equal(t, int64(2), got.value.Questions.Selected.Revision)
			var versions int
			require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND id=?`, questionConcept, questionConcept+":"+submitted.QuestionReceipt.ID).Scan(&versions))
			require.Equal(t, 2, versions, "refresh and exact replay must retain one submit and one share version")
		})
	}
}

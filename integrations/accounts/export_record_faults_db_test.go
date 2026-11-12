package accounts

// export_record_faults_db_test.go -- three faults around the export's record:
// a panic in the owners' email, a renewal that never returns, and a lookup
// that fails while a start is refused. Fixture: export_db_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/integrations/email"
)

type panickingSender struct{}

func (panickingSender) Send(context.Context, email.Message, email.SendAs) error {
	panic(errors.New("the template blew up"))
}

// 1. A panic in the notice is recovered and logged; Start answers and the
// process carries on.
func TestExportOwnersEmailPanicIsRecovered(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	f.x.notifyOwners = newOwnerNotice(func(context.Context) ([]string, error) {
		return []string{"one@example.test"}, nil
	}, func() email.Sender { return panickingSender{} }, f.x.logger)
	id := f.prepare(t, acme, false)["id"].(string)
	started := f.start(t, id)
	require.Equal(t, "queued", started["state"])
	f.x.notices.Wait()
	logs := f.logs.String()
	require.Contains(t, logs, "level=ERROR")
	require.Contains(t, logs, id)
	require.Contains(t, logs, "the template blew up")
	require.Contains(t, logs, "*errors.errorString")
}

// 2. A renewal that ignores its context and never returns cannot stop the
// fence: the work is cancelled before the stale window ends.
func TestExportFenceFiresWhileARenewalNeverReturns(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	forever := make(chan struct{})
	t.Cleanup(func() { close(forever) })
	cancelled := make(chan time.Time, 1)
	r := f.runner(t, "agent-a", func(ctx context.Context, _ Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		<-ctx.Done()
		cancelled <- time.Now()
		return ExportResult{}, ctx.Err()
	})
	r.heartbeatEvery = 10 * time.Millisecond
	r.fenceAfter = 300 * time.Millisecond
	r.renew = func(context.Context, Export, string) (bool, bool, error) {
		<-forever // ignores its context
		return false, false, nil
	}
	began := time.Now()
	r.tick(context.Background())
	select {
	case at := <-cancelled:
		require.GreaterOrEqual(t, at.Sub(began), r.fenceAfter)
		require.Less(t, at.Sub(began), exportStaleAfter)
	case <-time.After(10 * time.Second):
		t.Fatal("the fence never fired while the renewal was stuck")
	}
	r.wait()
	x, _ := f.row(t, id)
	require.Equal(t, "running", x.State, "a fenced node writes nothing")
}

// 4. A refused start whose own lookup failed says so, in words, and is not
// export_draft_expired; a draft known to be gone still is.
func TestExportStartLookupFailureIsNotAnExpiredDraft(t *testing.T) {
	f := newExportFixture(t)
	f.x.getHook = func(context.Context, string) (Export, bool, error) {
		return Export{}, false, errors.New("connection reset")
	}
	_, err := f.x.handleStart(person(exportPerson), map[string]any{"exportId": "aex-whatever"}, 0)
	require.Error(t, err)
	require.False(t, strings.HasPrefix(err.Error(), "export_draft_expired"), err.Error())
	require.Contains(t, err.Error(), "try again")

	f.x.getHook = nil
	_, err = f.x.handleStart(person(exportPerson), map[string]any{"exportId": "aex-whatever"}, 0)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "export_draft_expired: "), err.Error())
}

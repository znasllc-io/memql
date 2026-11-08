package email

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// azure_feedback_schedule_db_test.go -- how often a ready organization's sealed
// row is rewritten by the feedback indexer, counted in stored versions.
//
// Every look at an organization's delivery feedback reschedules its row
// (indexEmailConnectionState), a new version each time. Until 2026-11-08 the
// next look was always five minutes away, so a subscription Azure had already
// confirmed cost 288 versions and 288 management calls a day (about 451
// versions a day on production on 2026-11-06). A confirmed subscription is now
// checked hourly (feedbackRecheck), and a look another replica already took is
// neither written nor sent to Azure again.
func TestAConfirmedFeedbackSubscriptionIsRecheckedHourlyOverADay(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	if w == nil {
		return
	}
	ctx := context.Background()
	var calls atomic.Int32
	transport := acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		endpoint, _ := azureFeedbackURL()
		body := fmt.Sprintf(`{"properties":{"provisioningState":"Succeeded","destination":{"endpointType":"WebHook","properties":{"endpointBaseUrl":%q}}}}`, endpoint)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	for _, instance := range w.instances {
		instance.azure.protocol.client.Transport = transport
	}
	// The fixture's management token lasts an hour; this test's day must not
	// spend its looks refreshing it.
	var cluster azureClusterConnection
	if err := w.instances[0].azure.store.change(ctx, azureClusterKey, &cluster, func(bool) error {
		cluster.Session.TokenExpiresAt = w.now.Add(72 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rowID := emailStateID("organization:" + w.account)
	versions := func() int {
		t.Helper()
		var n int
		if err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, "v1:email:connectionState:"+rowID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := versions()

	// A day, both replicas polling every five minutes: often enough to take
	// every look the old five-minute schedule made.
	for range 24 * 12 {
		w.now = w.now.Add(5 * time.Minute)
		for _, instance := range w.instances {
			if err := instance.PollFeedbackConnections(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	written, sent := versions()-start, int(calls.Load())
	t.Logf("one day of a confirmed subscription: %d versions of the organization row and %d management calls (288 and 288 on the five-minute schedule)", written, sent)
	// The look that confirms the credential, the one five minutes after it,
	// then one an hour: 25 or 26 depending on where the day's edge falls.
	if written < 20 || written > 27 {
		t.Fatalf("a confirmed subscription's row was rewritten %d times in a day, want about 25 (hourly)", written)
	}
	// Every look reads the subscription (GET); the first also installs the
	// credential it minted (PUT), since the fixture's saved configuration names
	// no endpoint. A look not taken sends nothing.
	if sent != written+1 {
		t.Fatalf("%d management calls for %d looks, want one GET a look and the first look's PUT", sent, written)
	}

	// The race the list leaves open: a replica that listed the row before
	// another took the look must not write it or call Azure again.
	w.now = w.now.Add(2 * time.Hour)
	before, sentBefore := versions(), calls.Load()
	if claimed, err := w.instances[0].azure.indexFeedbackConnection(ctx, rowID, true); err != nil || !claimed {
		t.Fatalf("the first replica's due look: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := w.instances[1].azure.indexFeedbackConnection(ctx, rowID, true); err != nil || claimed {
		t.Fatalf("a look another replica already took was taken again: claimed=%v err=%v", claimed, err)
	}
	if got := versions(); got != before+1 || calls.Load() != sentBefore {
		t.Fatalf("two replicas racing for one look wrote %d version(s) and sent %d call(s), want 1 and 0", got-before, calls.Load()-sentBefore)
	}
}

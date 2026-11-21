package calls

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"sync"
	"testing"
	"time"
)

type messageTestMedia struct {
	MediaBackend
	mu         sync.Mutex
	packets    [][]byte
	recipients [][]string
	fail       bool
	entered    chan struct{}
	unblock    chan struct{}
}

func (m *messageTestMedia) PublishApplicationMessage(ctx context.Context, _ string, b []byte, to []string) error {
	m.mu.Lock()
	m.packets = append(m.packets, append([]byte(nil), b...))
	m.recipients = append(m.recipients, append([]string(nil), to...))
	fail := m.fail
	entered, unblock := m.entered, m.unblock
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errors.New("synthetic uncertain media response")
	}
	return nil
}
func messageFixture(t *testing.T) (*callWorld, callrules.Session, callrules.StartResult, *messageTestMedia) {
	cw, _, host, start, base := processingFixture(t, false)
	if cw == nil {
		return nil, host, start, nil
	}
	media := &messageTestMedia{MediaBackend: base.MediaBackend}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	return cw, host, start, media
}
func messageAsk(t *testing.T, cw *callWorld, sess callrules.Session, seq int64, text string) callrules.MessageRequest {
	t.Helper()
	state, e := cw.svc.SessionCall(context.Background(), sess)
	require.NoError(t, e)
	require.NotNil(t, state.Messages)
	require.True(t, state.Messages.Available)
	return callrules.MessageRequest{RoomID: state.RoomID, Identity: state.Identity, Sequence: seq, Kind: "chat", Text: &text}
}
func TestMessageRelayUnknownAttemptNeverRedispatchesOrStoresContent(t *testing.T) {
	cw, host, _, media := messageFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	q := messageAsk(t, cw, host, 1, "benign private relay fixture secret sentence")
	media.fail = true
	r, e := cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "delivery_uncertain", r.Status)
	retry, e := cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "duplicate", retry.Status)
	require.Equal(t, r.EventID, retry.EventID)
	require.Len(t, media.packets, 1)
	changed := q
	newtext := "different benign text"
	changed.Text = &newtext
	_, e = cw.svc.Message(ctx, host, changed)
	require.ErrorContains(t, e, "message_request_changed")
	var packet callrules.MessagePacket
	require.NoError(t, json.Unmarshal(media.packets[0], &packet))
	require.Equal(t, q.Identity, packet.Identity)
	require.Equal(t, "person", packet.ParticipantKind)
	require.Equal(t, "benign private relay fixture secret sentence", *packet.Text)
	require.Equal(t, []string{q.Identity}, media.recipients[0])
	var count int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE payload::text LIKE ?`, "%benign private relay fixture secret sentence%").Scan(&count))
	require.Zero(t, count)
	receipt, e := cw.svc.store.roomByID(ctx, cw.owner, q.RoomID)
	require.NoError(t, e)
	seat, e := cw.svc.store.seatByID(ctx, cw.owner, packet.SeatID)
	require.NoError(t, e)
	stored, e := cw.svc.store.messageReceipt(ctx, *receipt, *seat)
	require.NoError(t, e)
	raw, _ := json.Marshal(stored.State)
	require.Less(t, len(raw), 1024)
	require.NotContains(t, string(raw), "benign")
}
func TestMessageRelayTwoReplicasOneAttemptAndBoundAdmission(t *testing.T) {
	cw, host, start, media := messageFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	copyService := *cw.svc
	q := messageAsk(t, cw, host, 1, "one shared operation")
	results := make(chan callrules.MessageResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, svc := range []*Service{cw.svc, &copyService} {
		wg.Add(1)
		go func(s *Service) { defer wg.Done(); r, e := s.Message(ctx, host, q); results <- r; errs <- e }(svc)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	counts := map[string]int{}
	for r := range results {
		counts[r.Status]++
	}
	require.Equal(t, map[string]int{"published": 1, "duplicate": 1}, counts)
	require.Len(t, media.packets, 1)
	q.Sequence = 2
	q.RoomID = "another-room"
	_, e := cw.svc.Message(ctx, host, q)
	require.Error(t, e)
	q.RoomID = start.Call.ID
	q.Identity = "another-admission"
	_, e = cw.svc.Message(ctx, host, q)
	require.Error(t, e)
	q = messageAsk(t, cw, host, 2, "after departure")
	require.NoError(t, cw.svc.Leave(ctx, host))
	_, e = cw.svc.Message(ctx, host, q)
	require.Error(t, e)
	require.Len(t, media.packets, 1)
}
func TestMessageRelayRateAndHandSnapshot(t *testing.T) {
	cw, host, _, media := messageFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	q := messageAsk(t, cw, host, 1, "rate fixture")
	var beforeSeats int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND payload->>'roomId'=?`, seatConcept, roomConcept+":"+q.RoomID).Scan(&beforeSeats))
	r, e := cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "published", r.Status)
	q.Sequence = 2
	r, e = cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "rate_limited", r.Status)
	require.EqualValues(t, 1000, r.RetryAfterMS)
	raised := true
	q.Kind = "hand"
	q.Text = nil
	q.Raised = &raised
	r, e = cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "published", r.Status)
	state, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.Contains(t, state.Messages.Hands, q.Identity)
	require.EqualValues(t, 3, state.Messages.NextSequence)
	require.EqualValues(t, 2, state.Messages.HandSequences[q.Identity])
	cw.clock.Advance(time.Second)
	raised = false
	q.Sequence = 3
	r, e = cw.svc.Message(ctx, host, q)
	require.NoError(t, e)
	require.Equal(t, "published", r.Status)
	state, e = cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.Empty(t, state.Messages.Hands)
	require.EqualValues(t, 3, state.Messages.HandSequences[q.Identity])
	require.Len(t, media.packets, 3)
	var afterSeats int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND payload->>'roomId'=?`, seatConcept, roomConcept+":"+q.RoomID).Scan(&afterSeats))
	require.Equal(t, beforeSeats, afterSeats, "hand changes must not create persistent seat versions")
	// Payloads are never stored; rate state does not depend on room roster size.
	var bytes int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT COALESCE(sum(octet_length(payload::text)),0) FROM "MemoryNodes" WHERE concept=? AND payload->>'scope'='room' AND payload->>'targetId'=?`, messageRateConcept, q.RoomID).Scan(&bytes))
	require.Less(t, bytes, 16000)
	var receiptMax, rateMax int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT COALESCE(max(octet_length(payload::text)),0) FROM "MemoryNodes" WHERE concept=?`, messageReceiptConcept).Scan(&receiptMax))
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT COALESCE(max(octet_length(payload::text)),0) FROM "MemoryNodes" WHERE concept=?`, messageRateConcept).Scan(&rateMax))
	t.Logf("Stored JSON max bytes: receipt=%d, counter=%d; conservative accepted-attempt sum=%d (one receipt, two counters), excludes indexes/WAL", receiptMax, rateMax, receiptMax+2*rateMax)

}

package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type continuityFixture struct {
	t           *testing.T
	delegate    API
	application *apiFixture
	events      []map[string]any
	watchBody   []byte
	watchError  bool
	watches     int
	cursor      string
}

func (f *continuityFixture) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	u, err := url.Parse(path)
	require.NoError(f.t, err)
	collection, _ := testTarget.collectionPath()
	if u.Path != collection {
		return f.delegate.Do(ctx, method, path, contentType, body)
	}
	require.Equal(f.t, http.MethodGet, method)
	require.Empty(f.t, contentType)
	require.Empty(f.t, body)
	q := u.Query()
	require.Equal(f.t, "metadata.name="+testTarget.Name, q.Get("fieldSelector"))
	if q.Get("watch") == "true" {
		f.watches++
		require.Equal(f.t, f.cursor, q.Get("resourceVersion"))
		require.Equal(f.t, "true", q.Get("allowWatchBookmarks"))
		require.Equal(f.t, "1", q.Get("timeoutSeconds"))
		require.Len(f.t, q, 5)
		if f.watchError {
			return nil, errors.New("history compacted")
		}
		if f.watchBody != nil {
			return f.watchBody, nil
		}
		var out []byte
		for _, event := range f.events {
			raw, _ := json.Marshal(event)
			out = append(out, raw...)
			out = append(out, '\n')
		}
		return out, nil
	}
	require.Len(f.t, q, 2)
	require.Equal(f.t, "2", q.Get("limit"))
	return json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList", "metadata": map[string]any{"resourceVersion": "100"}, "items": []any{f.application.snapshot()}})
}

func newContinuityFixture(t *testing.T) (*continuityFixture, *Client, Intent, ContinuityState) {
	t.Helper()
	app := newFixture(t)
	f := &continuityFixture{t: t, delegate: app.client(t).api, application: app, cursor: "100"}
	c, err := New(f)
	require.NoError(t, err)
	snapshot, err := c.Read(t.Context(), testTarget)
	require.NoError(t, err)
	intent, err := c.PlanObservedRevision(t.Context(), snapshot, "continuity-request", testRevision, false)
	require.NoError(t, err)
	require.Equal(t, 2, intent.FormatVersion)
	require.Equal(t, "41", intent.BeforeResourceVersion)
	require.Equal(t, "100", intent.WatchResourceVersion)
	state, err := NewContinuityState(intent)
	require.NoError(t, err)
	return f, c, intent, state
}

func (f *continuityFixture) modify(version string, generation int64, change func(map[string]any)) {
	f.application.change(f.t, func(o map[string]any) {
		m := object(o, "metadata")
		m["resourceVersion"] = version
		m["generation"] = generation
		if change != nil {
			change(o)
		}
	})
	f.events = append(f.events, map[string]any{"type": "MODIFIED", "object": f.application.snapshot()})
}

func TestApplicationStatusGenerationCanAdvanceWithCompleteHistory(t *testing.T) {
	f, c, intent, state := newContinuityFixture(t)
	f.modify("42", 8, func(o map[string]any) { object(o, "status")["reconciledAt"] = "2026-11-08T17:30:00Z" })
	f.modify("43", 9, func(o map[string]any) { object(o, "status")["reconciledAt"] = "2026-11-08T17:33:00Z" })
	_, err := c.Apply(t.Context(), intent)
	require.ErrorContains(t, err, "fresh complete Application history")
	require.Zero(t, f.application.patchCount())
	next, proof, err := c.CheckContinuity(t.Context(), intent, state)
	require.NoError(t, err)
	require.Equal(t, int64(9), next.Generation)
	require.Equal(t, "43", next.ObjectVersion)
	require.Equal(t, 1, f.watches)
	_, err = c.ApplyWithContinuity(t.Context(), intent, proof)
	require.NoError(t, err)
	require.Equal(t, 1, f.application.writeCount())
	// The original operation marker still makes a lost reply idempotent.
	_, err = c.Apply(t.Context(), intent)
	require.NoError(t, err)
	require.Equal(t, 1, f.application.writeCount())
}

func TestApplicationHistoryRejectsAwayAndBackBeforeWriting(t *testing.T) {
	for _, fault := range []string{"spec", "marker", "operation", "replacement", "deletion"} {
		t.Run(fault, func(t *testing.T) {
			f, c, intent, state := newContinuityFixture(t)
			original := f.application.snapshot()
			f.modify("42", 8, func(o map[string]any) {
				switch fault {
				case "spec":
					object(object(o, "spec"), "source")["targetRevision"] = strings.Repeat("b", 40)
				case "marker":
					object(object(o, "metadata"), "annotations")[intentAnnotation] = "another-intent"
				case "operation":
					o["operation"] = map[string]any{"sync": map[string]any{}}
				case "replacement":
					object(o, "metadata")["uid"] = "replacement-uid"
				case "deletion":
					object(o, "metadata")["deletionTimestamp"] = "2026-11-08T17:30:00Z"
				}
			})
			f.modify("43", 9, func(o map[string]any) {
				o["spec"] = original["spec"]
				delete(o, "operation")
				m := object(o, "metadata")
				m["uid"] = testTarget.UID
				m["annotations"] = object(original, "metadata")["annotations"]
				delete(m, "deletionTimestamp")
			})
			next, proof, err := c.CheckContinuity(t.Context(), intent, state)
			require.ErrorIs(t, err, ErrChanged)
			require.True(t, next.Failed)
			require.Nil(t, proof)
			require.Equal(t, state.Cursor, next.Cursor)
			_, _, err = c.CheckContinuity(t.Context(), intent, next)
			require.ErrorIs(t, err, ErrChanged)
			require.Zero(t, f.application.patchCount())
		})
	}
}

func TestApplicationHistoryMissingOrMalformedCannotMintProof(t *testing.T) {
	for _, fault := range []string{"expired", "empty", "truncated", "missing-target", "error-event", "duplicate-key", "extra-event-field"} {
		t.Run(fault, func(t *testing.T) {
			f, c, intent, state := newContinuityFixture(t)
			f.modify("42", 8, nil)
			switch fault {
			case "expired":
				f.watchError = true
			case "empty":
				f.watchBody = []byte{}
			case "truncated":
				f.watchBody = []byte(`{"type":"MODIFIED","object":`)
			case "missing-target":
				f.events = []map[string]any{{"type": "BOOKMARK", "object": map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"resourceVersion": "100"}}}}
			case "error-event":
				f.events = []map[string]any{{"type": "ERROR", "object": map[string]any{"kind": "Status", "code": 410}}}
			case "duplicate-key":
				f.watchBody = []byte(`{"type":"BOOKMARK","type":"MODIFIED","object":{}}`)
			case "extra-event-field":
				f.events[0]["approved"] = true
			}
			next, proof, err := c.CheckContinuity(t.Context(), intent, state)
			require.Error(t, err)
			require.Nil(t, proof)
			require.Equal(t, state, next)
			require.Zero(t, f.application.patchCount())
		})
	}
}

func TestApplicationHistoryRefreshesUnchangedOldObjectBoundary(t *testing.T) {
	f, c, intent, state := newContinuityFixture(t)
	state.Cursor = "old-boundary"
	next, proof, err := c.CheckContinuity(t.Context(), intent, state)
	require.NoError(t, err)
	require.NotNil(t, proof)
	require.Equal(t, "100", next.Cursor)
	require.Equal(t, "41", next.ObjectVersion)
	require.Zero(t, f.watches)
}

func TestApplicationHistoryProofIsExactFreshAndCASBound(t *testing.T) {
	for _, fault := range []string{"expired", "foreign-intent", "changed-after-proof", "concurrent-patch"} {
		t.Run(fault, func(t *testing.T) {
			f, c, intent, state := newContinuityFixture(t)
			_, proof, err := c.CheckContinuity(t.Context(), intent, state)
			require.NoError(t, err)
			switch fault {
			case "expired":
				proof.observed = time.Now().Add(-time.Minute)
			case "foreign-intent":
				intent.RequestID = "foreign-request"
			case "changed-after-proof":
				f.modify("42", 8, nil)
			case "concurrent-patch":
				f.application.configure(false, true)
			}
			_, err = c.ApplyWithContinuity(t.Context(), intent, proof)
			require.Error(t, err)
			require.Zero(t, f.application.writeCount())
		})
	}
}

func TestApplicationHistoryIntentRoundTripAndOldJournalFence(t *testing.T) {
	f, c, intent, _ := newContinuityFixture(t)
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	restored, err := DecodeIntent(raw)
	require.NoError(t, err)
	require.Equal(t, intent, restored)
	old := fixtureIntent(t, c)
	require.Equal(t, 1, old.FormatVersion)
	f.modify("42", 8, nil)
	_, err = c.Apply(t.Context(), old)
	require.ErrorIs(t, err, ErrChanged)
	old.BeforeResourceVersion = "41"
	_, err = old.Digest()
	require.Error(t, err)
	require.Zero(t, f.application.patchCount())
}

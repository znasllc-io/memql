package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

func republishBudget(t *testing.T, full bool) (*mediaSignalBudget, *livekit.SignalRequest) {
	t.Helper()
	now := time.Now()
	b := newMediaSignalBudget(now)
	b.tracks["track0"] = mediaSignalTrack{source: livekit.TrackSource_CAMERA, sid: "old-camera", mid: "0"}
	offer := budgetSDP(0, 1, 0)
	offer.Id = 19
	offer.Sdp = strings.ReplaceAll(offer.Sdp, "sendonly", "inactive")
	if !b.publisherOffer(offer) {
		t.Fatal("fixture offer refused")
	}
	if full {
		for _, source := range []livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO} {
			b.tracks[source.String()] = mediaSignalTrack{source: source, sid: source.String()}
		}
	}
	return b, budgetAdd(budgetTrack("replacement", livekit.TrackSource_CAMERA))
}

func republishPending(t *testing.T, b *mediaSignalBudget) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		b.mu.Lock()
		pending := b.replacementPending != nil
		b.mu.Unlock()
		if pending {
			return
		}
		select {
		case <-deadline:
			t.Fatal("replacement did not wait")
		case <-tick.C:
		}
	}
}

func republishAnswer(b *mediaSignalBudget) *livekit.SessionDescription {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := proto.Clone(b.offer).(*livekit.SessionDescription)
	a.Type = "answer"
	return a
}

func TestMediaSignalRepublishReleaseRequiresExactProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		alter              func(*livekit.SessionDescription)
		unpublish, allowed bool
	}{
		{name: "matched_answer", allowed: true},
		{name: "provider_unpublish", unpublish: true, allowed: true},
		{name: "mismatched_id", alter: func(a *livekit.SessionDescription) { a.Id++ }},
		{name: "active_answer", alter: func(a *livekit.SessionDescription) { a.Sdp = strings.ReplaceAll(a.Sdp, "inactive", "recvonly") }},
		{name: "wrong_mid", alter: func(a *livekit.SessionDescription) { a.Sdp = strings.ReplaceAll(a.Sdp, "a=mid:0", "a=mid:other") }},
		{name: "invalid_answer", alter: func(a *livekit.SessionDescription) { a.Sdp = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, req := republishBudget(t, true)
			now := time.Now()
			if !b.frame(now, 123) {
				t.Fatal("frame refused")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			result := make(chan string, 1)
			go func() { result <- b.requestAwaitRelease(ctx, now, req) }()
			republishPending(t, b)
			b.mu.Lock()
			if len(b.tracks) != 4 || b.additions.amount != 7 || b.messages.amount != 127 || b.bytes.amount != mediaSignalByteBurst-123 {
				t.Error("early release or wrong spending")
			}
			b.mu.Unlock()
			a := republishAnswer(b)
			if tc.alter != nil {
				tc.alter(a)
			}
			response := &livekit.SignalResponse{Message: &livekit.SignalResponse_Answer{Answer: a}}
			if tc.unpublish {
				response = &livekit.SignalResponse{Message: &livekit.SignalResponse_TrackUnpublished{TrackUnpublished: &livekit.TrackUnpublishedResponse{TrackSid: "old-camera"}}}
			}
			if !b.provider(response, "native-identity") {
				t.Fatal("provider response refused")
			}
			got := <-result
			b.mu.Lock()
			defer b.mu.Unlock()
			if (got == "") != tc.allowed {
				t.Fatalf("allowed=%v reason=%s", tc.allowed, got)
			}
			if b.additions.amount != 7 || b.messages.amount != 127 || b.bytes.amount != mediaSignalByteBurst-123 || len(b.tracks) != 4 {
				t.Fatal("wait changed spending or track bound")
			}
			_, replacement := b.tracks["replacement"]
			if replacement != tc.allowed || b.replacementPending != nil {
				t.Fatal("wrong reservation or retained waiter")
			}
		})
	}
}

func TestMediaSignalRepublishWaitRefusesUnknownAndEarlyDuplicates(t *testing.T) {
	for _, name := range []string{"no_offer", "active_mid", "unknown_mid", "unknown_sid", "unknown_source_mid", "caller_sid", "already_waiting"} {
		t.Run(name, func(t *testing.T) {
			b, req := republishBudget(t, false)
			old := b.tracks["track0"]
			switch name {
			case "no_offer":
				b.offer = nil
			case "active_mid":
				b.offer.Sdp = strings.ReplaceAll(b.offer.Sdp, "inactive", "sendonly")
			case "unknown_mid":
				old.mid = ""
				b.tracks["track0"] = old
			case "unknown_sid":
				old.sid = ""
				b.tracks["track0"] = old
			case "unknown_source_mid":
				old.mid = "other"
				b.tracks["track0"] = old
			case "caller_sid":
				req.GetAddTrack().Sid = "unknown"
			case "already_waiting":
				b.replacementPending = &mediaSignalReplacement{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			started := time.Now()
			if got := b.requestAwaitRelease(ctx, started, req); got == "" {
				t.Fatal("unsupported duplicate accepted")
			}
			if time.Since(started) > 500*time.Millisecond {
				t.Fatal("unsupported duplicate waited")
			}
			if len(b.tracks) != 1 || b.additions.amount != 7 {
				t.Fatal("reservation/spending changed")
			}
		})
	}
}

func TestMediaSignalRepublishWaitCancellationAndLateAnswer(t *testing.T) {
	b, req := republishBudget(t, false)
	answer := republishAnswer(b)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan string, 1)
	go func() { result <- b.requestAwaitRelease(ctx, time.Now(), req) }()
	republishPending(t, b)
	cancel()
	if got := <-result; got != "budget_add_track_source" {
		t.Fatal(got)
	}
	if !b.provider(&livekit.SignalResponse{Message: &livekit.SignalResponse_Answer{Answer: answer}}, "native-identity") {
		t.Fatal("late provider response refused")
	}
	if _, exists := b.tracks["replacement"]; exists || b.replacementPending != nil {
		t.Fatal("late acknowledgement resurrected cancelled request")
	}
}

func TestMediaSignalRepublishWaitHasFixedDeadline(t *testing.T) {
	b, req := republishBudget(t, false)
	started := time.Now()
	if got := b.requestAwaitRelease(context.Background(), started, req); got != "budget_add_track_source" {
		t.Fatal(got)
	}
	elapsed := time.Since(started)
	if elapsed < mediaSignalReleaseWait || elapsed > mediaSignalReleaseWait+time.Second {
		t.Fatal("wait escaped fixed bound", elapsed)
	}
	if len(b.tracks) != 1 || b.replacementPending != nil || b.additions.amount != 7 {
		t.Fatal("timeout released reservation or recharged")
	}
}

func TestMediaSignalRepublishWaitIgnoresUnrelatedRelease(t *testing.T) {
	b, req := republishBudget(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan string, 1)
	go func() { result <- b.requestAwaitRelease(ctx, time.Now(), req) }()
	republishPending(t, b)
	b.provider(&livekit.SignalResponse{Message: &livekit.SignalResponse_TrackUnpublished{TrackUnpublished: &livekit.TrackUnpublishedResponse{TrackSid: livekit.TrackSource_MICROPHONE.String()}}}, "native-identity")
	if got := <-result; got != "budget_add_track_count" {
		t.Fatal("unrelated source released waiting camera", got)
	}
	if _, exists := b.tracks["track0"]; !exists {
		t.Fatal("old camera removed")
	}
	if _, exists := b.tracks["replacement"]; exists {
		t.Fatal("unrelated release admitted camera")
	}
}

func TestMediaSignalRepublishWaitRequiresCapturedProviderTuple(t *testing.T) {
	b, req := republishBudget(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan string, 1)
	go func() { result <- b.requestAwaitRelease(ctx, time.Now(), req) }()
	republishPending(t, b)
	b.provider(&livekit.SignalResponse{Message: &livekit.SignalResponse_TrackPublished{TrackPublished: &livekit.TrackPublishedResponse{Cid: "track0", Track: &livekit.TrackInfo{Sid: "changed-camera", Source: livekit.TrackSource_CAMERA, Type: livekit.TrackType_VIDEO}}}}, "native-identity")
	b.provider(&livekit.SignalResponse{Message: &livekit.SignalResponse_TrackUnpublished{TrackUnpublished: &livekit.TrackUnpublishedResponse{TrackSid: "changed-camera"}}}, "native-identity")
	if got := <-result; got != "budget_add_track_source" {
		t.Fatal("changed provider tuple released waiting request", got)
	}
	if _, exists := b.tracks["replacement"]; exists {
		t.Fatal("changed tuple admitted replacement")
	}
}

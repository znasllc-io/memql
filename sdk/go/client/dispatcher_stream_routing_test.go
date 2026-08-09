package client

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// dispatcher_stream_routing_test.go is the whole of memql#3429: the routing
// table in streamRequestId decides whether a multi-frame server message
// reaches its caller or is quietly delivered to an event channel nobody is
// reading, and until now a family missing from it failed in total silence on
// both sides of the wire.
//
// Three things are pinned here.
//
//  1. The COVERAGE LEDGER below classifies every member of the
//     MemqlServerMessage payload oneof as routed or deliberately unrouted, and
//     the classification is checked against the proto by reflection. A family
//     added to the proto tomorrow fails this file by name instead of shipping
//     as a silent omission -- which converts memql#3414's failure mode from an
//     investigation cycle into a red test.
//
//  2. The streaming-chat pair (AiStreamChunk + AiChatResult) routes, closing
//     the divergence with the TS SDK, WITHOUT disturbing the non-streaming
//     chat path that resolves on correlate_to.
//
//  3. An unrouted request-scoped frame is loud exactly once per family, and
//     counted every time.
//
// No cluster, no token, no database: the wire was never the problem.

// routedFamilies is the intended coverage -- every payload oneof member
// streamRequestId keys by request_id. Read the rule at streamRequestId; in
// short, these are the families whose frames a caller can be parked on via
// RegisterStream, because the correlate_to tier structurally cannot serve
// them.
var routedFamilies = map[string]string{
	"ai_transcribe_stream_delta":    "streaming transcription: repeats with the accumulated text",
	"ai_transcribe_stream_complete": "streaming transcription: the terminal",
	"automation_run_event":          "automation run trace: accepted, N steps, one complete (memql#3310/#3414)",
	"ai_chunk":                      "streaming chat: N token deltas",
	"ai_chat_result":                "streaming chat: the terminal carrying the assembled text",
	"agent_generate_turn_delta":     "agent turn streaming: deltas",
	"agent_generate_turn_complete":  "agent turn streaming: the terminal",
	"voice_agent_turn_delta":        "voice-agent turn streaming: deltas",
	"voice_agent_turn_complete":     "voice-agent turn streaming: the terminal",
	"query_result":                  "QueryResultChunk carries `done`; a chunked contract even where the engine emits one",
	"query_error":                   "the error terminal that can end any of the above in place of its normal terminal",
}

// unroutedFamilies is the other half of the ledger, each with the reason it
// stays off the routing table. Two reasons only:
//
//	"single-reply"  -- SendAndWait resolves it on correlate_to before
//	                   streamRequestId is consulted. Routing it would assert a
//	                   streaming contract that does not exist. 55 of these.
//	"not request-scoped" -- no request_id at all, or a server-minted one for an
//	                   exchange no caller opened. Belongs on Events().
//
// If you are here because this test failed on a family you just added: pick
// the side it belongs on. If the server can emit it MORE THAN ONCE for one
// request, or it terminates such an exchange, it is routed -- add a case to
// streamRequestId, add it to routedFamilies, and mirror both into
// sdk/ts/src/client/wire.ts. Otherwise add it here.
var unroutedFamilies = map[string]string{
	"server_hello":                           "not request-scoped",
	"event":                                  "not request-scoped",
	"heartbeat":                              "not request-scoped",
	"client_tool_call":                       "not request-scoped: handled by its own tier in Run()",
	"rotate_auth_result":                     "not request-scoped",
	"voice_agent_speak":                      "not request-scoped: server-minted id for an exchange no caller opened",
	"list_tools_result":                      "single-reply",
	"call_tool_result":                       "single-reply",
	"ai_speech_result":                       "single-reply",
	"ai_transcribe_result":                   "single-reply",
	"ai_suggest_result":                      "single-reply",
	"identity_result":                        "single-reply",
	"delegation_result":                      "single-reply",
	"sense_tokenize_result":                  "single-reply",
	"sense_complete_result":                  "single-reply",
	"sense_diagnose_result":                  "single-reply",
	"sense_hover_result":                     "single-reply",
	"sense_signature_help_result":            "single-reply",
	"sense_definition_result":                "single-reply",
	"polyphon_room_token_result":             "single-reply",
	"polyphon_status_result":                 "single-reply",
	"polyphon_utterance_result":              "single-reply",
	"concepts_list_result":                   "single-reply",
	"concepts_subscribe_result":              "single-reply",
	"my_access_result":                       "single-reply",
	"send_guest_invite_result":               "single-reply",
	"resolve_guest_invite_result":            "single-reply",
	"join_space_as_guest_result":             "single-reply",
	"cancel_guest_invite_result":             "single-reply",
	"resend_guest_invite_email_result":       "single-reply",
	"revoke_current_session_result":          "single-reply",
	"revoke_all_sessions_result":             "single-reply",
	"create_worker_token_result":             "single-reply",
	"revoke_worker_token_result":             "single-reply",
	"voice_agent_partial_ack":                "single-reply",
	"voice_agent_final_ack":                  "single-reply",
	"voice_agent_session_ack":                "single-reply",
	"voice_agent_realtime_output_ack":        "single-reply",
	"list_pack_domains_result":               "single-reply",
	"list_pack_files_result":                 "single-reply",
	"read_pack_file_result":                  "single-reply",
	"node_maintenance_result":                "single-reply",
	"authoring_validate_bundle_result":       "single-reply",
	"authoring_session_define_bundle_result": "single-reply",
	"dsl_spec_result":                        "single-reply",
	"durable_promote_bundle_result":          "single-reply",
	"durable_demote_bundle_result":           "single-reply",
	"create_badge_result":                    "single-reply",
	"revoke_badge_result":                    "single-reply",
	"deploy_control_result":                  "single-reply",
	"create_account_token_result":            "single-reply",
	"revoke_account_token_result":            "single-reply",
	"identity_admin_result":                  "single-reply",
}

// serverPayloadOneof returns the MemqlServerMessage payload oneof descriptor.
func serverPayloadOneof(t *testing.T) protoreflect.OneofDescriptor {
	t.Helper()
	od := (&memqlv1.MemqlServerMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")
	if od == nil {
		t.Fatal("MemqlServerMessage has no `payload` oneof -- the routing table's whole premise")
	}
	return od
}

// serverMessageWithRequestId builds a MemqlServerMessage carrying the named
// payload family, with request_id set when the family has that field.
//
// Built by reflection so the gate below covers families no Go code in this
// repo constructs by hand -- including the next one somebody adds.
func serverMessageWithRequestId(fd protoreflect.FieldDescriptor, requestId string) *memqlv1.MemqlServerMessage {
	msg := &memqlv1.MemqlServerMessage{}
	m := msg.ProtoReflect()
	if fd.Kind() != protoreflect.MessageKind {
		return msg
	}
	inner := m.NewField(fd)
	if rf := inner.Message().Descriptor().Fields().ByName("request_id"); rf != nil && rf.Kind() == protoreflect.StringKind {
		inner.Message().Set(rf, protoreflect.ValueOfString(requestId))
	}
	m.Set(fd, inner)
	return msg
}

// TestRoutingLedgerCoversEveryServerPayload is the build break that replaces
// silence (memql#3429). Every member of the payload oneof must be classified,
// and the classification must be exhaustive and disjoint.
//
// This is what makes the next omission cost seconds. memql#3414 shipped
// because nothing anywhere related "the proto grew a family" to "the SDK
// routes a family"; the two facts lived in different files in different
// languages with no assertion between them. Here they are one assertion.
func TestRoutingLedgerCoversEveryServerPayload(t *testing.T) {
	od := serverPayloadOneof(t)

	var unclassified, both []string
	inProto := map[string]bool{}
	for i := 0; i < od.Fields().Len(); i++ {
		name := string(od.Fields().Get(i).Name())
		inProto[name] = true
		_, routed := routedFamilies[name]
		_, unrouted := unroutedFamilies[name]
		switch {
		case routed && unrouted:
			both = append(both, name)
		case !routed && !unrouted:
			unclassified = append(unclassified, name)
		}
	}

	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("MemqlServerMessage.payload has %d member(s) this SDK has not classified: %v\n"+
			"  Every family must be either ROUTED by streamRequestId (multi-frame, or the terminal\n"+
			"  of a multi-frame exchange -- add a case there AND to routedFamilies, AND mirror both\n"+
			"  into sdk/ts/src/client/wire.ts) or listed in unroutedFamilies with its reason.\n"+
			"  This test exists because an unclassified family is delivered to the event channel in\n"+
			"  silence and the caller parks forever on a frame the server already sent (memql#3414).",
			len(unclassified), unclassified)
	}
	if len(both) > 0 {
		sort.Strings(both)
		t.Errorf("classified as BOTH routed and unrouted: %v", both)
	}

	var stale []string
	for name := range routedFamilies {
		if !inProto[name] {
			stale = append(stale, name+" (routedFamilies)")
		}
	}
	for name := range unroutedFamilies {
		if !inProto[name] {
			stale = append(stale, name+" (unroutedFamilies)")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("the ledger names %d family/families the proto no longer has: %v\n"+
			"  A renamed or removed payload silently un-routes itself; drop or rename the entry.", len(stale), stale)
	}
}

// TestStreamRequestIdMatchesTheLedger asserts the switch and the ledger agree,
// family by family, by constructing each payload through reflection.
//
// The ledger alone would be a comment. This is what makes it a contract: a
// family listed as routed whose case somebody deleted fails here, and so does
// a family listed as unrouted that acquired a case.
func TestStreamRequestIdMatchesTheLedger(t *testing.T) {
	od := serverPayloadOneof(t)
	const rid = "req-ledger-1"

	for i := 0; i < od.Fields().Len(); i++ {
		fd := od.Fields().Get(i)
		name := string(fd.Name())
		msg := serverMessageWithRequestId(fd, rid)
		got := streamRequestId(msg)

		if _, routed := routedFamilies[name]; routed {
			if got != rid {
				t.Errorf("%s is listed as routed (%s) but streamRequestId returned %q, want %q.\n"+
					"  Its frames go to the uncorrelated event channel and no RegisterStream listener\n"+
					"  ever sees them -- memql#3414 verbatim.", name, routedFamilies[name], got, rid)
			}
			continue
		}
		if got != "" {
			t.Errorf("%s is listed as unrouted (%s) but streamRequestId returned %q.\n"+
				"  Routing a single-reply result moves it off the correlate_to path for anyone who\n"+
				"  sends with Send instead of SendAndWait. Either fix the case or move the entry\n"+
				"  into routedFamilies with a reason.", name, unroutedFamilies[name], got)
		}
	}
}

// aiChatChunk builds one streaming-chat token delta.
//
// CorrelateTo is set exactly as the server sets it (ai_handlers.go
// handleAiChatStream passes `correlate` to every frame) -- and it must not be
// what routes these, because a streaming caller uses Send, so nothing is
// registered under the message_id.
func aiChatChunk(requestId string, index int64, text string, done bool) *memqlv1.MemqlServerMessage {
	return &memqlv1.MemqlServerMessage{
		CorrelateTo: "envelope-message-id",
		Payload: &memqlv1.MemqlServerMessage_AiChunk{
			AiChunk: &memqlv1.AiStreamChunk{
				StreamId:  requestId,
				RequestId: requestId,
				Index:     index,
				Chunk:     &memqlv1.AiStreamChunk_TextDelta{TextDelta: text},
				Done:      done,
			},
		},
	}
}

func aiChatTerminal(requestId, content string) *memqlv1.MemqlServerMessage {
	return &memqlv1.MemqlServerMessage{
		CorrelateTo: "envelope-message-id",
		Payload: &memqlv1.MemqlServerMessage_AiChatResult{
			AiChatResult: &memqlv1.AiChatResult{
				RequestId: requestId,
				Message:   &memqlv1.AiChatMessage{Role: "assistant", Content: content},
			},
		},
	}
}

// TestDispatcherRoutesStreamingChat is the behavioural half of the Go/TS
// divergence (memql#3429). The TS SDK has routed aiChunk + aiChatResult since
// it shipped; the Go SDK dropped both. Remove either case from streamRequestId
// and this fails the way a live consumer would: no terminal, ever.
func TestDispatcherRoutesStreamingChat(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	const requestId = "chat-req-1"
	frames, release := d.RegisterStream(requestId)
	defer release()

	stream.recvCh <- aiChatChunk(requestId, 0, "hello", false)
	stream.recvCh <- aiChatChunk(requestId, 1, " world", false)
	stream.recvCh <- aiChatTerminal(requestId, "hello world")

	var text strings.Builder
	var terminal *memqlv1.AiChatResult
	deadline := time.After(2 * time.Second)
	for terminal == nil {
		select {
		case <-deadline:
			t.Fatalf("no AiChatResult routed to the chat session's listener (deltas seen: %q). "+
				"streamRequestId must key ai_chunk and ai_chat_result by request_id, as the TS SDK "+
				"already does -- otherwise a streaming-chat caller parks forever (memql#3429).", text.String())
		case msg, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed before the chat completed")
			}
			if c := msg.GetAiChunk(); c != nil {
				text.WriteString(c.GetTextDelta())
				continue
			}
			if r := msg.GetAiChatResult(); r != nil {
				terminal = r
			}
		}
	}

	if text.String() != "hello world" {
		t.Errorf("both token deltas must route; assembled %q, want %q", text.String(), "hello world")
	}
	if got := terminal.GetMessage().GetContent(); got != "hello world" {
		t.Errorf("the terminal must survive routing intact; got %q", got)
	}
}

// TestNonStreamingChatStillResolvesOnCorrelateTo is the regression guard for
// the behaviour change, and the reason routing ai_chat_result is safe.
//
// The non-streaming aiChat path uses SendAndWait, which registers under the
// envelope's message_id and is served by the correlate_to tier BEFORE
// streamRequestId is consulted. This test makes that ordering explicit under
// the hostile arrangement: a stream is registered for the very same
// request_id, so if the tiers were reordered -- or if someone "simplified"
// Run() by consulting streamRequestId first -- the caller would hang while the
// stream listener silently ate its reply.
func TestNonStreamingChatStillResolvesOnCorrelateTo(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	const requestId = "chat-req-2"
	frames, release := d.RegisterStream(requestId)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	respCh := make(chan *memqlv1.MemqlServerMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := d.SendAndWait(ctx, &memqlv1.MemqlClientMessage{
			MessageId: "chat-envelope-1",
			Payload: &memqlv1.MemqlClientMessage_AiChat{
				AiChat: &memqlv1.AiChatMsg{RequestId: requestId},
			},
		})
		respCh <- resp
		errCh <- err
	}()

	// Wait for the request to be registered before answering it.
	select {
	case <-stream.sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("SendAndWait never wrote the request to the stream")
	}

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: "chat-envelope-1",
		Payload: &memqlv1.MemqlServerMessage_AiChatResult{
			AiChatResult: &memqlv1.AiChatResult{
				RequestId: requestId,
				Message:   &memqlv1.AiChatMessage{Role: "assistant", Content: "one-shot"},
			},
		},
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("SendAndWait failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendAndWait never returned. Routing ai_chat_result by request_id must NOT " +
			"pre-empt the correlate_to tier -- a one-shot chat caller would be starved by a " +
			"stream listener registered under the same request_id (memql#3429).")
	}

	resp := <-respCh
	if got := resp.GetAiChatResult().GetMessage().GetContent(); got != "one-shot" {
		t.Errorf("correlated reply content = %q, want %q", got, "one-shot")
	}
	select {
	case msg := <-frames:
		t.Fatalf("the correlated reply must NOT also reach the stream listener; got %v", msg)
	default:
	}
}

// TestUnregisteredStreamFramesStillReachEvents pins the other half of the
// safety argument: routing a family by request_id does not remove it from the
// event channel for a consumer that never registered a stream.
//
// This is the specific starvation the issue warned about, and it does not
// exist. A frame only leaves Events() when there is a registered listener for
// its exact request_id -- i.e. when somebody is waiting for it.
func TestUnregisteredStreamFramesStillReachEvents(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	stream.recvCh <- aiChatTerminal("chat-req-3", "nobody registered")

	select {
	case msg, ok := <-d.Events():
		if !ok {
			t.Fatal("the event channel closed")
		}
		if got := msg.GetAiChatResult().GetMessage().GetContent(); got != "nobody registered" {
			t.Fatalf("event-channel delivery = %q, want %q", got, "nobody registered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a chat frame with no registered listener must still fall through to Events()")
	}
}

// TestQueryErrorReachesTheStreamListener pins the one addition beyond the
// families memql#3429 enumerates.
//
// A streaming request that fails server-side is answered with QueryErrorMsg,
// not with its normal terminal -- component/grpc/ai_transcribe_stream.go calls
// sendQueryError on every refusal, keyed by the same request_id. Unrouted,
// that error is delivered to the event channel and the caller waits out its
// own deadline: memql#3414's outcome by a different route, and reachable today
// by the shipped voice.PushToTalk helper.
func TestQueryErrorReachesTheStreamListener(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	const requestId = "transcribe-req-1"
	frames, release := d.RegisterStream(requestId)
	defer release()

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: "envelope-message-id",
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				RequestId: requestId,
				Error:     &memqlv1.QueryError{Message: "no streaming transcription provider available"},
			},
		},
	}

	select {
	case msg, ok := <-frames:
		if !ok {
			t.Fatal("the stream closed before the error arrived")
		}
		if got := msg.GetQueryError().GetError().GetMessage(); got != "no streaming transcription provider available" {
			t.Fatalf("routed error message = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a QueryErrorMsg carrying a registered stream's request_id must reach that " +
			"listener -- otherwise a failed streaming request is indistinguishable from a hung one.")
	}
}

// TestUnroutedRequestScopedFrameIsLoudExactlyOnce pins the anti-silence
// mechanism AND its rate limit.
//
// The requirement pulls both ways: an unrouted family must surface in seconds
// rather than an investigation cycle, and it must not become log spam when a
// server pushes thousands of frames of it. One Warn per payload family per
// dispatcher satisfies both -- bounded by the oneof's member count for the
// life of a connection -- with the true volume kept on the counter.
func TestUnroutedRequestScopedFrameIsLoudExactlyOnce(t *testing.T) {
	if _, ok := unroutedFamilies["ai_speech_result"]; !ok {
		t.Fatal("this test needs a family that is NOT routed; ai_speech_result has moved -- pick another")
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stream := newMockStream()
	d := NewDispatcher(stream, logger)
	go d.Run()
	defer d.Stop()

	for i := 0; i < 3; i++ {
		stream.recvCh <- &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_AiSpeechResult{
				AiSpeechResult: &memqlv1.AiSpeechResult{RequestId: "speech-req-1"},
			},
		}
	}
	// Drain them off the event channel so we know Run() processed all three.
	for i := 0; i < 3; i++ {
		select {
		case <-d.Events():
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 3 frames reached the event channel", i)
		}
	}

	if n := strings.Count(logs.String(), "the SDK routing table does not route its family"); n != 1 {
		t.Errorf("want exactly 1 warning for 3 frames of one unrouted family, got %d.\n"+
			"  0 means the omission is still silent (memql#3414); >1 means it can become log spam.\n"+
			"  Log was:\n%s", n, logs.String())
	}
	if got := d.UnroutedFrames()["ai_speech_result"]; got != 3 {
		t.Errorf("UnroutedFrames counts every frame even though the log speaks once; got %d, want 3", got)
	}
}

// TestLateStreamFrameIsQuiet is the other side of the rate-limit requirement.
//
// A frame arriving for a ROUTED family after its listener unregistered is
// ordinary -- a cancelled PushToTalk still gets its terminal -- so it must not
// produce a warning. It is still counted, so "my stream missed frames" remains
// answerable without turning the log up.
func TestLateStreamFrameIsQuiet(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stream := newMockStream()
	d := NewDispatcher(stream, logger)
	go d.Run()
	defer d.Stop()

	// No RegisterStream: the family is routed, the listener is simply absent.
	stream.recvCh <- aiChatTerminal("chat-req-late", "after unregister")

	select {
	case <-d.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("the late frame never reached the event channel")
	}

	if strings.Contains(logs.String(), "the SDK routing table does not route its family") {
		t.Errorf("a late frame of a ROUTED family must not warn -- that is normal traffic and "+
			"warning on it is how the loud signal gets tuned out. Log was:\n%s", logs.String())
	}
	if got := d.UnroutedFrames()["ai_chat_result"]; got != 1 {
		t.Errorf("a late frame is still counted; got %d, want 1", got)
	}
}

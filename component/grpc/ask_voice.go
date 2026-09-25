package memql

import (
	"context"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/grpc/codes"
)

func (s *streamSession) handleAskVoiceStart(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AskVoiceStartMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "voice request is missing")
	}
	requestID := s.normalizeRequestId(envelope, msg.GetRequestId())
	if s.shouldProxyAI(nodeTargetForChat()) {
		return s.proxyAI(envelope, requestID, nodeTargetForChat())
	}
	if s.service.engine == nil {
		return s.sendQueryError(requestID, envelope.GetMessageId(), codes.Unavailable, "MemQL is unavailable")
	}
	ctx, release, err := s.beginAskRequest(s.stream.Context(), requestID)
	if err != nil {
		return s.sendQueryError(requestID, envelope.GetMessageId(), codes.AlreadyExists, err.Error())
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		credentials, err := s.service.engine.StartAskVoice(ctx, engine.AskVoiceOptions{ConversationID: msg.GetConversationId(), PageContext: msg.GetPageContext(), Voice: msg.GetVoice(), ChatProvider: msg.GetChatProvider(), TranscriptionProvider: msg.GetTranscriptionProvider(), SpeechProvider: msg.GetSpeechProvider()})
		if err != nil {
			_ = s.sendQueryError(requestID, envelope.GetMessageId(), codes.FailedPrecondition, err.Error())
			return
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AskVoiceStartResult{AskVoiceStartResult: &memqlv1.AskVoiceStartResult{RequestId: requestID, Url: credentials.URL, Token: credentials.Token, Room: credentials.Room}}})
	}()
	return nil
}

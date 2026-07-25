package memql

import (
	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// handleSenseTokenize handles SenseTokenizeMsg requests.
func (s *streamSession) handleSenseTokenize(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SenseTokenizeMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "sense_tokenize: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	svc := s.service.sense
	if svc == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL Sense is not configured")
	}

	go func() {
		tokens := svc.Tokenize(msg.GetSource())
		protoTokens := make([]*memqlv1.SenseToken, len(tokens))
		for i, t := range tokens {
			protoTokens[i] = &memqlv1.SenseToken{
				Type:    t.Type,
				Literal: t.Literal,
				Range:   senseRangeToProto(t.Range),
			}
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SenseTokenizeResult{
				SenseTokenizeResult: &memqlv1.SenseTokenizeResult{
					RequestId: requestId,
					Tokens:    protoTokens,
				},
			},
		})
	}()
	return nil
}

// handleSenseComplete handles SenseCompleteMsg requests.
func (s *streamSession) handleSenseComplete(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SenseCompleteMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "sense_complete: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	svc := s.service.sense
	if svc == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL Sense is not configured")
	}

	go func() {
		cursor := msg.GetCursor()
		line, col := int(cursor.GetLine()), int(cursor.GetColumn())
		items := svc.Complete(msg.GetSource(), line, col, msg.GetFilePath())
		protoItems := make([]*memqlv1.SenseCompletionItem, len(items))
		for i, item := range items {
			protoItems[i] = &memqlv1.SenseCompletionItem{
				Label:         item.Label,
				Kind:          item.Kind,
				Detail:        item.Detail,
				Documentation: item.Documentation,
				InsertText:    item.InsertText,
				SortPriority:  int32(item.SortPriority),
				IsSnippet:     item.IsSnippet,
			}
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SenseCompleteResult{
				SenseCompleteResult: &memqlv1.SenseCompleteResult{
					RequestId: requestId,
					Items:     protoItems,
				},
			},
		})
	}()
	return nil
}

// handleSenseDiagnose handles SenseDiagnoseMsg requests.
func (s *streamSession) handleSenseDiagnose(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SenseDiagnoseMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "sense_diagnose: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	svc := s.service.sense
	if svc == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL Sense is not configured")
	}

	go func() {
		diags := svc.Diagnose(msg.GetSource(), msg.GetFilePath())
		protoDiags := make([]*memqlv1.SenseDiagnostic, len(diags))
		for i, d := range diags {
			protoDiags[i] = &memqlv1.SenseDiagnostic{
				Range:    senseRangeToProto(d.Range),
				Severity: memqlv1.SenseSeverity(d.Severity),
				Message:  d.Message,
				Code:     d.Code,
			}
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SenseDiagnoseResult{
				SenseDiagnoseResult: &memqlv1.SenseDiagnoseResult{
					RequestId:   requestId,
					Diagnostics: protoDiags,
				},
			},
		})
	}()
	return nil
}

// handleSenseHover handles SenseHoverMsg requests.
func (s *streamSession) handleSenseHover(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SenseHoverMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "sense_hover: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	svc := s.service.sense
	if svc == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL Sense is not configured")
	}

	go func() {
		pos := msg.GetPosition()
		// SenseHoverMsg carries no file_path (SenseCompleteMsg and
		// SenseDiagnoseMsg do). Without it a bare concept name whose
		// trailing segment collides across namespaces stays unresolved
		// here rather than resolving wrongly; every unambiguous name is
		// unaffected. Adding the field is a proto change (#2753).
		result := svc.Hover(msg.GetSource(), int(pos.GetLine()), int(pos.GetColumn()), "")
		resp := &memqlv1.SenseHoverResult{RequestId: requestId}
		if result != nil {
			resp.Contents = result.Contents
			resp.Range = senseRangeToProto(result.Range)
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SenseHoverResult{
				SenseHoverResult: resp,
			},
		})
	}()
	return nil
}

// handleSenseSignatureHelp handles SenseSignatureHelpMsg requests.
func (s *streamSession) handleSenseSignatureHelp(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SenseSignatureHelpMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "sense_signature_help: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	svc := s.service.sense
	if svc == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL Sense is not configured")
	}

	go func() {
		pos := msg.GetPosition()
		result := svc.SignatureHelp(msg.GetSource(), int(pos.GetLine()), int(pos.GetColumn()))
		resp := &memqlv1.SenseSignatureHelpResult{RequestId: requestId}
		if result != nil {
			resp.ActiveSignature = int32(result.ActiveSignature)
			resp.ActiveParameter = int32(result.ActiveParameter)
			for _, sig := range result.Signatures {
				protoParams := make([]*memqlv1.SenseParameter, len(sig.Parameters))
				for j, p := range sig.Parameters {
					protoParams[j] = &memqlv1.SenseParameter{
						Label:         p.Label,
						Documentation: p.Documentation,
					}
				}
				resp.Signatures = append(resp.Signatures, &memqlv1.SenseSignature{
					Label:         sig.Label,
					Documentation: sig.Documentation,
					Parameters:    protoParams,
				})
			}
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SenseSignatureHelpResult{
				SenseSignatureHelpResult: resp,
			},
		})
	}()
	return nil
}

// senseRangeToProto converts a sense.Range to a proto SenseRange.
func senseRangeToProto(r sense.Range) *memqlv1.SenseRange {
	return &memqlv1.SenseRange{
		Start: &memqlv1.SensePosition{
			Line:   int32(r.Start.Line),
			Column: int32(r.Start.Column),
		},
		End: &memqlv1.SensePosition{
			Line:   int32(r.End.Line),
			Column: int32(r.End.Column),
		},
	}
}

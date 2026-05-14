package memql

import (
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	memqlengine "github.com/visionarys-io/memql/component/memql"
)

// handleEvaluatePolicy answers an EvaluatePolicyMsg by dispatching
// through engine.EvaluatePolicy with RequiredTier="bff". The handler
// is the only place the @frontend_visible gate is checked: every
// frontend-bound policy invocation goes through here, so callers
// cannot reach into core policies via the gRPC surface.
//
// Error contract (see EvaluatePolicyResult.error_code in
// memql.proto):
//
//   POLICY_UNKNOWN              -- policy_name does not resolve
//   POLICY_NOT_FRONTEND_VISIBLE -- policy lacks @frontend_visible
//   POLICY_TIER_MISMATCH        -- policy is not tier=bff
//   POLICY_RUNTIME_ERROR        -- evaluation errored at runtime
func (s *streamSession) handleEvaluatePolicy(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.EvaluatePolicyMsg) error {
	requestId := ""
	if msg != nil {
		requestId = msg.GetRequestId()
	}
	if s.service == nil || s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "policy engine is not available")
	}
	name := msg.GetPolicyName()
	if name == "" {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_UNKNOWN", "policy_name is required")
	}

	registry := s.service.engine.PolicyFunctions()
	if registry == nil {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_UNKNOWN", "policy registry is not initialised")
	}
	policy, ok := registry.Lookup(name)
	if !ok {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_UNKNOWN", fmt.Sprintf("policy %q is not registered", name))
	}
	if !policy.FrontendVisible {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_NOT_FRONTEND_VISIBLE", fmt.Sprintf("policy %q is not @frontend_visible", name))
	}
	if policy.Tier != "bff" {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_TIER_MISMATCH", fmt.Sprintf("policy %q tier=%q, only bff policies are callable from the frontend", name, policy.Tier))
	}

	var args map[string]any
	if raw := msg.GetArgsJson(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_RUNTIME_ERROR", fmt.Sprintf("decode args_json: %v", err))
		}
	}

	ctx := s.stream.Context()
	result, trace, err := s.service.engine.EvaluatePolicy(ctx, name, args, memqlengine.PolicyEvalOptions{
		RequiredTier: "bff",
		ReturnTrace:  msg.GetReturnTrace(),
	})
	if err != nil {
		return s.sendEvaluatePolicyError(requestId, envelope.GetMessageId(), "POLICY_RUNTIME_ERROR", err.Error())
	}

	resultJSON, _ := json.Marshal(result)
	traceJSON := ""
	if (msg.GetReturnTrace() || policy.ReturnsTrace) && trace != nil {
		b, _ := json.Marshal(trace)
		traceJSON = string(b)
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_EvaluatePolicyResult{
			EvaluatePolicyResult: &memqlv1.EvaluatePolicyResult{
				RequestId:  requestId,
				ResultJson: string(resultJSON),
				TraceJson:  traceJSON,
			},
		},
	})
}

func (s *streamSession) sendEvaluatePolicyError(requestId, messageId, code, message string) error {
	return s.sendServerMessage(messageId, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_EvaluatePolicyResult{
			EvaluatePolicyResult: &memqlv1.EvaluatePolicyResult{
				RequestId:    requestId,
				ErrorCode:    code,
				ErrorMessage: message,
			},
		},
	})
}

package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/envregistry"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// module_handlers.go is the stream landing for the module registry (epic
// memql#4183; design docs/superpowers/specs/2026-08-20-module-registry-
// design.md sections 5-6).
//
// The handlers are thin the way identity_admin_handlers.go is thin, and
// for the same reason: the policy -- read app:cluster/modules for reads; the
// owner, or a developer on a storefront pack, for the flip -- lives in ONE
// place (component/memql's AuthorizeModuleRead / AuthorizeSetPackEnabled, and
// SetPackEnabled for the write itself), so a future second transport cannot
// disagree with this one. What lives here
// is the plumbing that stamps the session's resolved AccessContext onto
// the context those gates read, the proto mapping, and -- for the one
// write -- the audit emission, because this package is the one with the
// identity dependency. Exactly one audit event per SetPackEnabled call,
// INCLUDING refusals, mirroring adminops.
//
// Failures never return a Go error: an error out of a handler tears down
// the multiplexed stream. The gRPC status rides inside each result
// (error_code / error_message).

// SetModuleAuditLogger installs the audit sink the SetPackEnabled write
// emits through. Mirrors SetIdentityAdminHandler's double write so a
// post-Run install still takes effect. Nil leaves the write surface
// refusing with FAILED_PRECONDITION rather than flipping packs unaudited.
func (s *Server) SetModuleAuditLogger(l identity.AuditLogger) {
	if s == nil {
		return
	}
	s.moduleAudit = l
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.moduleAudit = l
	}
}

// moduleCtx stamps the stream's resolved AccessContext onto the context,
// exactly as handleIdentityAdmin does. A nil access context is left alone
// on purpose: the policy gates then fail closed with UNAUTHENTICATED.
func (s *streamSession) moduleCtx() context.Context {
	c := s.stream.Context()
	if ac := s.ensureAccess(c); ac != nil {
		c = auth.ContextWithAccess(c, ac)
	}
	return c
}

func (s *streamSession) reportingNode() (nodeID, nodeType string) {
	return s.service.streamNodeID, envregistry.ResolveNodeType()
}

func moduleInfoToProto(row memqlengine.ModuleRow) *memqlv1.ModuleInfo {
	return &memqlv1.ModuleInfo{
		Kind:          row.Kind,
		Name:          row.Name,
		Description:   row.Description,
		State:         row.State,
		StateDetail:   row.StateDetail,
		Scope:         row.Scope,
		EnvComponents: row.EnvComponents,
		FqnPrefixes:   row.FqnPrefixes,
		CodeReference: row.CodeReference,
		MayFlip:       row.MayFlip,
	}
}

func (s *streamSession) handleModulesList(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ModulesListMsg) error {
	out := &memqlv1.ModulesListResult{RequestId: msg.GetRequestId()}
	out.ReportingNodeId, out.ReportingNodeType = s.reportingNode()

	ctx := s.moduleCtx()
	if refusal := memqlengine.AuthorizeModuleRead(ctx); refusal != nil {
		out.ErrorCode = int32(refusal.Code)
		out.ErrorMessage = refusal.Message
		return s.replyModulesList(envelope, out)
	}

	rows, err := s.service.engine.ListModules(ctx)
	if err != nil {
		out.ErrorCode = 13 // INTERNAL
		out.ErrorMessage = err.Error()
		return s.replyModulesList(envelope, out)
	}
	for _, row := range rows {
		out.Modules = append(out.Modules, moduleInfoToProto(row))
	}
	return s.replyModulesList(envelope, out)
}

func (s *streamSession) handleModuleDetail(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ModuleDetailMsg) error {
	out := &memqlv1.ModuleDetailResult{RequestId: msg.GetRequestId()}
	out.ReportingNodeId, out.ReportingNodeType = s.reportingNode()

	ctx := s.moduleCtx()
	if refusal := memqlengine.AuthorizeModuleRead(ctx); refusal != nil {
		out.ErrorCode = int32(refusal.Code)
		out.ErrorMessage = refusal.Message
		return s.replyModuleDetail(envelope, out)
	}

	detail, err := s.service.engine.ModuleDetail(ctx, msg.GetKind(), msg.GetName())
	if err != nil {
		out.ErrorCode = 13 // INTERNAL
		out.ErrorMessage = err.Error()
		return s.replyModuleDetail(envelope, out)
	}
	if detail == nil {
		out.ErrorCode = 5 // NOT_FOUND
		out.ErrorMessage = fmt.Sprintf("module inventory: no %s module named %q", msg.GetKind(), msg.GetName())
		return s.replyModuleDetail(envelope, out)
	}
	out.Module = moduleInfoToProto(detail.Row)
	for _, v := range detail.EnvVars {
		out.EnvVars = append(out.EnvVars, &memqlv1.ModuleEnvVar{
			Name:         v.Name,
			Description:  v.Description,
			Secret:       v.Secret,
			Scope:        v.Scope,
			RequiredFor:  v.RequiredFor,
			Set:          v.Set,
			Value:        v.Value,
			DefaultValue: v.DefaultValue,
		})
	}
	return s.replyModuleDetail(envelope, out)
}

// handleSetPackEnabled performs the one module-registry write. The policy
// AND the write are component/memql's SetPackEnabled -- an owner flips any
// pack, a developer holding execute app:cluster/modules flips a storefront
// pack -- because the write runs under internal origin and this package may
// not stamp it. What stays here is the audit event, emitted for EVERY outcome
// -- refusal, validation failure, mutation failure, success -- before the
// reply goes out.
func (s *streamSession) handleSetPackEnabled(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SetPackEnabledMsg) error {
	out := &memqlv1.SetPackEnabledResult{
		RequestId:       msg.GetRequestId(),
		PackDomain:      strings.TrimSpace(msg.GetPackDomain()),
		Enabled:         msg.GetEnabled(),
		RestartRequired: true,
	}

	audit := s.service.moduleAudit
	if audit == nil {
		// Refuse rather than flip a pack unaudited: the audit trail is part
		// of the write's contract (design section 5), not a nice-to-have.
		out.ErrorCode = 9 // FAILED_PRECONDITION
		out.ErrorMessage = "set pack enabled: this node has no audit sink wired"
		return s.replySetPackEnabled(envelope, out)
	}

	ctx := s.moduleCtx()
	actor, prior, refusal, err := s.service.engine.SetPackEnabled(ctx, out.PackDomain, msg.GetEnabled(), msg.GetReason())

	emit := func(outcome identity.AuditOutcome, failureReason string) {
		audit.Log(ctx, identity.AuditEvent{
			Category:      identity.AuditCategoryAdmin,
			Action:        "module_set_pack_enabled",
			ActorUserId:   actor.ID,
			ActorEmail:    actor.Email,
			ActorRole:     string(actor.Role),
			TargetType:    "pack",
			TargetId:      out.PackDomain,
			Detail:        map[string]any{"enabled": msg.GetEnabled(), "reason": msg.GetReason()},
			Outcome:       outcome,
			FailureReason: failureReason,
		})
	}

	switch {
	case refusal != nil:
		emit(identity.AuditOutcomeBlocked, refusal.Message)
		out.ErrorCode = int32(refusal.Code)
		out.ErrorMessage = refusal.Message
	case err != nil:
		emit(identity.AuditOutcomeFailure, err.Error())
		out.PriorEnabled = prior
		out.ErrorCode = 13 // INTERNAL
		out.ErrorMessage = fmt.Sprintf("set pack enabled: %v", err)
	default:
		emit(identity.AuditOutcomeSuccess, "")
		out.PriorEnabled = prior
	}
	return s.replySetPackEnabled(envelope, out)
}

func (s *streamSession) replyModulesList(envelope *memqlv1.MemqlClientMessage, out *memqlv1.ModulesListResult) error {
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ModulesListResult{ModulesListResult: out},
	})
}

func (s *streamSession) replyModuleDetail(envelope *memqlv1.MemqlClientMessage, out *memqlv1.ModuleDetailResult) error {
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ModuleDetailResult{ModuleDetailResult: out},
	})
}

func (s *streamSession) replySetPackEnabled(envelope *memqlv1.MemqlClientMessage, out *memqlv1.SetPackEnabledResult) error {
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_SetPackEnabledResult{SetPackEnabledResult: out},
	})
}

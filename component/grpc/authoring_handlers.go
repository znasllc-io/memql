package memql

// authoring_handlers.go -- the cockpit-facing gRPC surface over the engine's
// existing authoring machinery (issue memql#2128 / C1). Two operations:
//
//   - AuthoringValidateBundle: run the Gate-1 isolated compile-and-bind sandbox
//     (memql.ValidateBundle -> SandboxCompileBundle) over a .memql bundle and
//     return per-construct diagnostics + an overall ok. NO engine mutation: the
//     sandbox compiles against a READ-ONLY clone of the live registry.
//
//   - AuthoringSessionDefineBundle: validate, then session-define the bundle via
//     memql.AuthorSessionBundle into this stream's OWNER-scoped authored
//     registry (stream-scoped, non-durable). Defined function-family constructs
//     become callable BY NAME within the session, never shadowing core, and are
//     dropped when the stream ends.
//
// This MIRRORS the MCP `define` wiring (component/mcp/tool_surface.go
// handleDefine): same gate (owner/developer via auth.CanAuthor, the
// authoring-tier bar), same AuthorSessionBundle call, same owner-scoped
// AuthoredRuntimeRegistry. The difference is the surface (gRPC stream vs MCP
// tool) and where the session registry lives (per-streamSession here vs
// per-Server there). Owner-gated DURABLE activation/promotion is out of scope
// (issue #232 / ActivateApprovedBundle); this surface only validates + injects.

import (
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// handleAuthoringValidateBundle runs the Gate-1 sandbox over the bundle and
// returns the diagnostics. Read-only: it never mutates engine state, so it is
// safe against a running engine. Still gated to the authoring roles
// (owner/developer) -- validation reveals the live concept surface, and the
// cockpit only exposes the authoring panel to those roles anyway.
func (s *streamSession) handleAuthoringValidateBundle(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AuthoringValidateBundleMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "authoring_validate_bundle: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if allowed, err := s.requireAuthoringRole(requestId, envelope.GetMessageId()); !allowed {
		return err
	}

	report := memqlengine.ValidateBundle(msg.GetSources())
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AuthoringValidateBundleResult{
			AuthoringValidateBundleResult: &memqlv1.AuthoringValidateBundleResult{
				RequestId:   requestId,
				Ok:          report.OK,
				Diagnostics: authoringDiagnosticsToProto(report.Diagnostics),
			},
		},
	})
}

// handleAuthoringSessionDefineBundle validates + session-defines the bundle into
// this stream's owner-scoped registry. On validation failure it returns the
// diagnostics with ok=false and a populated error; on success it returns the
// defined constructs (now callable by name for the session).
func (s *streamSession) handleAuthoringSessionDefineBundle(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AuthoringSessionDefineBundleMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "authoring_session_define_bundle: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if allowed, err := s.requireAuthoringRole(requestId, envelope.GetMessageId()); !allowed {
		return err
	}

	// Owner is the authenticated caller; every construct is keyed to it so one
	// session can never resolve another's. ensureAccess always returns non-nil
	// here because requireAuthoringRole already resolved + gated it.
	ac := s.ensureAccess(s.stream.Context())
	owner := ""
	if ac != nil {
		owner = strings.TrimSpace(ac.UserId)
	}
	if owner == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unauthenticated,
			"authoring_session_define_bundle: an authenticated owner is required to session-define a bundle")
	}

	res, err := memqlengine.AuthorSessionBundle(s.authoredSessionRegistry(), owner, msg.GetSources())

	result := &memqlv1.AuthoringSessionDefineBundleResult{
		RequestId:   requestId,
		Ok:          res.OK,
		Defined:     authoringConstructsToProto(res.Defined),
		Diagnostics: authoringDiagnosticsToProto(res.Diagnostics),
	}
	if err != nil {
		// A validation/register failure registers nothing; surface the reason
		// alongside the per-construct diagnostics so the cockpit can render them.
		result.Error = err.Error()
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AuthoringSessionDefineBundleResult{
			AuthoringSessionDefineBundleResult: result,
		},
	})
}

// handleDurablePromoteBundle validates the bundle via the Gate-1 sandbox, then
// durably promotes every plain construct into the SHARED engine registry (issue
// znasllc-io/memql-cockpit#232, engine half). It is the owner-gated, restart-
// durable, LIVE-cross-node-propagating counterpart to the session-define
// surface above. OWNER-only: it matches the MCP `promote` gate (roleCanPromote /
// promoteGate -- owner role, strictly higher than the owner-or-developer
// CanAuthor bar the define/validate surface uses).
//
// On success the promoted constructs are persisted (reviewable + restart-
// durable), registered into the shared registry (callable by every session on
// this node, core-first never-shadow), and a broadcast event makes them callable
// on EVERY node within seconds. On a validation failure it returns ok=false with
// the diagnostics + a populated error and promotes nothing.
func (s *streamSession) handleDurablePromoteBundle(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DurablePromoteBundleMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "durable_promote_bundle: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if allowed, err := s.requireOwnerRole(requestId, envelope.GetMessageId()); !allowed {
		return err
	}

	// Owner is the authenticated caller; the persisted rows are stamped to it
	// and PromoteConstructDurable runs the writes under its envelope.
	ac := s.ensureAccess(s.stream.Context())
	owner := ""
	if ac != nil {
		owner = strings.TrimSpace(ac.UserId)
	}
	if owner == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unauthenticated,
			"durable_promote_bundle: an authenticated owner is required to promote a bundle")
	}

	if s.service == nil || s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable,
			"durable_promote_bundle: engine unavailable on this node")
	}
	res, err := s.service.engine.PromoteBundleDurable(s.stream.Context(), owner, msg.GetSources())

	result := &memqlv1.DurablePromoteBundleResult{
		RequestId:   requestId,
		Ok:          res.OK,
		Promoted:    durablePromotedConstructsToProto(res.Promoted),
		Diagnostics: authoringDiagnosticsToProto(res.Diagnostics),
	}
	if err != nil {
		result.Ok = false
		result.Error = err.Error()
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DurablePromoteBundleResult{
			DurablePromoteBundleResult: result,
		},
	})
}

// handleDurableDemoteBundle is the inverse of handleDurablePromoteBundle
// (memql#2163): it durably DEMOTES (retires) every previously promoted plain
// construct named in the bundle source out of the SHARED engine registry. OWNER-
// only, same gate as the promote handler. There is no Gate-1 compile -- a demote
// only needs the names + kinds, so a construct whose source no longer compiles
// can still be demoted.
//
// On success the named constructs are removed from the shared registry on this
// node (author-promoted-only safety gate -- a core construct can never be
// removed), the persisted bundle/construct rows are flipped to "retired" (so a
// restart never re-hydrates them), and a broadcast event removes them on EVERY
// node within seconds. On failure (no demotable constructs, or a per-construct
// demote error) it returns ok=false with a populated error.
func (s *streamSession) handleDurableDemoteBundle(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DurableDemoteBundleMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "durable_demote_bundle: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if allowed, err := s.requireOwnerRole(requestId, envelope.GetMessageId()); !allowed {
		return err
	}

	// Owner is the authenticated caller; the persisted retire writes run under
	// its envelope, mirroring the promote handler.
	ac := s.ensureAccess(s.stream.Context())
	owner := ""
	if ac != nil {
		owner = strings.TrimSpace(ac.UserId)
	}
	if owner == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unauthenticated,
			"durable_demote_bundle: an authenticated owner is required to demote a bundle")
	}

	if s.service == nil || s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable,
			"durable_demote_bundle: engine unavailable on this node")
	}
	res, err := s.service.engine.DemoteBundleDurable(s.stream.Context(), owner, msg.GetSources())

	result := &memqlv1.DurableDemoteBundleResult{
		RequestId:   requestId,
		Ok:          res.OK,
		Demoted:     durableDemotedConstructsToProto(res.Demoted),
		Diagnostics: authoringDiagnosticsToProto(res.Diagnostics),
	}
	if err != nil {
		result.Ok = false
		result.Error = err.Error()
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DurableDemoteBundleResult{
			DurableDemoteBundleResult: result,
		},
	})
}

// durableDemotedConstructsToProto maps the engine-side DefinedConstructs (the
// durably-demoted ones) onto the wire form.
func durableDemotedConstructsToProto(demoted []memqlengine.DefinedConstruct) []*memqlv1.AuthoringConstruct {
	out := make([]*memqlv1.AuthoringConstruct, 0, len(demoted))
	for _, c := range demoted {
		out = append(out, &memqlv1.AuthoringConstruct{Kind: c.Kind, Name: c.Name})
	}
	return out
}

// requireOwnerRole resolves the caller's access and enforces the OWNER-only
// gate the durable promote needs (issue #232). This is the higher bar than
// requireAuthoringRole (owner OR developer): promotion makes a construct durable
// + cluster-wide, so it matches the MCP `promote` op's owner-only gate
// (roleCanPromote). Returns allowed=true to proceed; when allowed=false the
// denial has already been sent and the returned error is the send error.
func (s *streamSession) requireOwnerRole(requestId, correlate string) (bool, error) {
	ac := s.ensureAccess(s.stream.Context())
	if ac == nil || ac.Role != auth.RoleOwner {
		return false, s.sendQueryError(requestId, correlate, codes.PermissionDenied,
			"durable promotion requires the owner role")
	}
	return true, nil
}

// durablePromotedConstructsToProto maps the engine-side DefinedConstructs (the
// durably-promoted ones) onto the wire form.
func durablePromotedConstructsToProto(promoted []memqlengine.DefinedConstruct) []*memqlv1.AuthoringConstruct {
	out := make([]*memqlv1.AuthoringConstruct, 0, len(promoted))
	for _, c := range promoted {
		out = append(out, &memqlv1.AuthoringConstruct{Kind: c.Kind, Name: c.Name})
	}
	return out
}

// requireAuthoringRole resolves the caller's access and enforces the authoring
// gate (owner or developer -- the same auth.CanAuthor bar the MCP `define` op
// uses). Returns allowed=true to proceed. When allowed=false the denial has
// already been sent to the client; the returned error is the send error (nil on
// a successful send), which the caller bubbles up as the handler's return.
func (s *streamSession) requireAuthoringRole(requestId, correlate string) (bool, error) {
	ac := s.ensureAccess(s.stream.Context())
	if ac == nil || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return false, s.sendQueryError(requestId, correlate, codes.PermissionDenied,
			"authoring requires the owner or developer role")
	}
	return true, nil
}

// authoringDiagnosticsToProto maps the engine-side SandboxDiagnostics onto the
// wire form. Kept here (not in the SDK) because it is the server-side
// translation; the SDK owns the inverse.
func authoringDiagnosticsToProto(diags []memqlengine.SandboxDiagnostic) []*memqlv1.AuthoringDiagnostic {
	out := make([]*memqlv1.AuthoringDiagnostic, 0, len(diags))
	for _, d := range diags {
		out = append(out, &memqlv1.AuthoringDiagnostic{
			Name:      d.Name,
			Kind:      d.Kind,
			Ok:        d.OK,
			Skipped:   d.Skipped,
			Error:     d.Error,
			Line:      int32(d.Line),
			Column:    int32(d.Column),
			EndLine:   int32(d.EndLine),
			EndColumn: int32(d.EndColumn),
		})
	}
	return out
}

// authoringConstructsToProto maps the engine-side DefinedConstructs onto the
// wire form.
func authoringConstructsToProto(defined []memqlengine.DefinedConstruct) []*memqlv1.AuthoringConstruct {
	out := make([]*memqlv1.AuthoringConstruct, 0, len(defined))
	for _, c := range defined {
		out = append(out, &memqlv1.AuthoringConstruct{Kind: c.Kind, Name: c.Name})
	}
	return out
}

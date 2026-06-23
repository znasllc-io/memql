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
			Name:    d.Name,
			Kind:    d.Kind,
			Ok:      d.OK,
			Skipped: d.Skipped,
			Error:   d.Error,
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

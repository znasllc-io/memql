package memql

// promote_site_handlers.go -- the operator surface over artifact promotion
// (epic memql#3748 / memql#3768).
//
// # Why this is a gRPC message and not a mutation
//
// Every other write to v1:platform:site goes through the engine's mutation
// path, and this one structurally cannot. The engine reaches exactly ONE
// schema -- the connection's search path decides which, and that is the entire
// environment boundary (memql#3765) -- so a promote expressed as a DSL mutation
// would write the row it read, in the environment it was already in, and do
// nothing at all. The promote names both schemas, which no mutation body can.
//
// # Why the seam is an interface
//
// The promoter lives in component/edge, in the root module, while this package
// is its own. Rather than drag the edge's object-storage dependencies across
// that boundary for a type, the seam is declared here and satisfied by an
// adapter in app/ -- the same shape as AgentTurnHandler above it.
//
// It is also the narrower contract: this interface can express nothing but
// "move a site's bundle reference", so no future edge capability leaks onto the
// stream by accident.

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// SitePromoteOutcome is what a promote or rollback did.
type SitePromoteOutcome struct {
	PreviousBundleRef string
	BundleRef         string
	Created           bool
	NoOp              bool
}

// SitePromoter moves a site's bundle reference between environments.
//
// bundleRef, when set, pins the value to write instead of resolving it from
// fromEnvironment: that is the ROLLBACK form, and it is the same call rather
// than a second method because rollback is the same write with the previous
// value. A separate method would be a second code path that could drift from
// the one it undoes.
type SitePromoter interface {
	PromoteSite(ctx context.Context, siteId, fromEnvironment, toEnvironment, bundleRef, hostname string) (SitePromoteOutcome, error)
}

// SetSitePromoter installs the promote seam. Called during app bootstrap on
// nodes that carry one; other node types leave it nil and the handler below
// reports that rather than panicking.
func (s *Server) SetSitePromoter(p SitePromoter) {
	if s == nil {
		return
	}
	s.sitePromoter = p
}

// handlePromoteSite handles PromoteSiteMsg requests.
//
// Owner-gated, matching handleDurablePromoteBundle next door and for the same
// reason: this writes to the production environment. It is the single most
// consequential write an operator can make from a client, and the fact that it
// is one row rather than a deploy makes it MORE important to gate, not less --
// a cheap irreversible-looking action invites being tried.
func (s *streamSession) handlePromoteSite(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.PromoteSiteMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "promote_site: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if allowed, err := s.requireOwnerRole(requestId, envelope.GetMessageId()); !allowed {
		return err
	}

	if s.service == nil || s.service.sitePromoter == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable,
			"promote_site: this node carries no site promoter")
	}

	siteId := strings.TrimSpace(msg.GetSiteId())
	if siteId == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument,
			"promote_site: a site id is required")
	}
	toEnv := strings.TrimSpace(msg.GetToEnvironment())
	if toEnv == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument,
			"promote_site: a target environment is required")
	}
	fromEnv := strings.TrimSpace(msg.GetFromEnvironment())
	bundleRef := strings.TrimSpace(msg.GetBundleRef())
	if fromEnv == "" && bundleRef == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument,
			"promote_site: give a source environment to promote from, or a bundle ref to pin (the rollback form)")
	}

	outcome, err := s.service.sitePromoter.PromoteSite(s.stream.Context(),
		siteId, fromEnv, toEnv, bundleRef, strings.TrimSpace(msg.GetHostname()))

	result := &memqlv1.PromoteSiteResult{
		RequestId: requestId,
		SiteId:    siteId,
	}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.PreviousBundleRef = outcome.PreviousBundleRef
		result.BundleRef = outcome.BundleRef
		result.Created = outcome.Created
		result.NoOp = outcome.NoOp
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_PromoteSiteResult{PromoteSiteResult: result},
	})
}

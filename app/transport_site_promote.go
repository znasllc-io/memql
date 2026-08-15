//go:build bff

package app

// transport_site_promote.go -- the bff half of artifact promotion
// (epic memql#3748 / memql#3768).
//
// The promote lives on the BFF and not on the edge, for the same reason the
// bundle-publish endpoint does (see transport_sites.go): the edge node is
// wildcard-routed by site hostname, so a site-agnostic operator action has no
// coherent address there.
//
// This file is where the two halves meet, exactly as transport_sites.go is for
// publish. component/grpc declares the seam it needs (memqlgrpc.SitePromoter)
// because it is a tiered module with no relative-path replace reaching the
// unsplit root module component/edge lives in; this file is part of that
// unsplit root module and so can see both.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/edge"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
)

// mountSitePromote installs the cross-schema promote seam on the gRPC server.
//
// A no-op when the node has no database handle: promote is one transaction over
// two schemas on one connection, so without a connection there is nothing to
// install, and handlePromoteSite reports the absence rather than panicking.
func (a *App) mountSitePromote() {
	if a.grpcServer == nil {
		return
	}
	db := a.BunDB()
	if db == nil {
		a.Logger.Warn("site promote not mounted: no database handle on this node")
		return
	}
	a.grpcServer.SetSitePromoter(sitePromoterAdapter{
		promoter: edge.NewPromoter(db),
		prefix:   edge.DefaultEnvironmentSchemaPrefix,
	})
}

// sitePromoterAdapter maps the wire's ENVIRONMENT names onto the schema names
// the promoter takes.
//
// The mapping is a COMPOSITION -- prefix + name -- and never a lookup, which is
// the constraint the epic imposes rather than a stylistic choice.
// TestNoEnvironmentBranchingInEngineCode fails the build on engine code so much
// as naming a deployment environment, in any form, because a name is what a
// branch is built out of and a table from environment to schema would be the
// second way to deploy the parity standard rejects. The environment therefore
// arrives from the caller as a value and leaves as a string, and adding one is
// a deployment change that touches nothing here.
type sitePromoterAdapter struct {
	promoter *edge.Promoter
	prefix   string
}

func (a sitePromoterAdapter) PromoteSite(ctx context.Context, siteId, fromEnvironment, toEnvironment, bundleRef, hostname string) (memqlgrpc.SitePromoteOutcome, error) {
	target, err := edge.SchemaFor(a.prefix, toEnvironment)
	if err != nil {
		return memqlgrpc.SitePromoteOutcome{}, fmt.Errorf("target %w", err)
	}

	// An explicit bundleRef is the ROLLBACK form: pin this value rather than
	// resolving one from a source environment. Same write, previous value.
	if strings.TrimSpace(bundleRef) != "" {
		res, err := a.promoter.SetBundleRef(ctx, target, siteId, bundleRef)
		return toOutcome(res), err
	}

	source, err := edge.SchemaFor(a.prefix, fromEnvironment)
	if err != nil {
		return memqlgrpc.SitePromoteOutcome{}, fmt.Errorf("source %w", err)
	}
	res, err := a.promoter.Promote(ctx, source, target, siteId, hostname)
	return toOutcome(res), err
}

func toOutcome(r edge.PromoteResult) memqlgrpc.SitePromoteOutcome {
	return memqlgrpc.SitePromoteOutcome{
		PreviousBundleRef: r.PreviousBundleRef,
		BundleRef:         r.BundleRef,
		Created:           r.Created,
		NoOp:              r.NoOp,
	}
}

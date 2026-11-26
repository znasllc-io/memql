package siteaccess

// tester_code.go -- the verdict on a tester's typed code for the engine's
// calls-testers gate. The calls service asks it before it applies a simulated
// payment or age result; nothing else reads it.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// TesterCodeVerdict is what a typed tester code turned out to be.
type TesterCodeVerdict int

const (
	// TesterCodeUnknown: not a live calls-testers code of this site. Malformed,
	// never minted, removed, replaced by a regenerate, another gate's, another
	// site's, or a copy of an organization the site has since left.
	TesterCodeUnknown TesterCodeVerdict = iota
	// TesterCodeExpired: exactly a code of this site's testers gate, past its
	// expiry. Only somebody holding the code can learn this.
	TesterCodeExpired
	// TesterCodeLive: the code opens simulated results now.
	TesterCodeLive
	// TesterCodeUnavailable: the site cannot hold codes at all (no such site, or
	// no package deployable), so no typed value could ever open it.
	TesterCodeUnavailable
)

// TesterCode decides a typed tester code for a site at `now`, by the same rules the
// access route opens a code by (codeOpensSite and the expiry), plus the gate. The
// site is read as the cluster and FRESH, so a removal, a regenerate, a new expiry or
// a move of the site's organization counts from the next request.
func (s *GraphStore) TesterCode(ctx context.Context, siteID, typed string, now time.Time) (TesterCodeVerdict, error) {
	var code *Code
	if memql.AccessCodeWellFormed(typed) {
		found, err := s.CodeByHash(ctx, siteID, memql.AccessCodeDigest(typed))
		if err != nil {
			return TesterCodeUnknown, err
		}
		code = found
	}
	site, err := s.SiteForAccess(ctx, siteID)
	if err != nil {
		return TesterCodeUnknown, err
	}
	if site == nil || site.DeployableName == "" {
		return TesterCodeUnavailable, nil
	}
	if code == nil || code.Guest || code.Gate != memql.AccessGateCallsTesters || code.Revoked || code.Generation < 1 ||
		memql.BareShortId(code.SiteID) != memql.BareShortId(site.ID) ||
		memql.BareShortId(code.AccountID) != memql.BareShortId(site.AccountID) {
		return TesterCodeUnknown, nil
	}
	if !now.Before(code.ExpiresAt) {
		return TesterCodeExpired, nil
	}
	return TesterCodeLive, nil
}

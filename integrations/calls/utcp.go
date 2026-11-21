package calls

import (
	"context"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// AgentManual declares only this site's fixed product operations. No MCP server
// is involved. The trusted site's published hostname, never a request Host,
// determines where a tool sends its credential.
func (s *Service) AgentManual(ctx context.Context, site callrules.SiteRef) (any, error) {
	host, err := s.store.publicHost(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return nil, err
	}
	return callrules.UTCPManual("https://" + host + "/_memql/calls/agent"), nil
}

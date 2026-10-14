package identity

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
	"github.com/znasllc-io/memql/component/memql"
)

// The ordinary integration factory supplies the engine on every node. Reuse
// the admin service's authority and audit path instead of giving the model a
// general identity mutation or borrowing a server actor for the whole call.
func (i *IdentityIntegration) handleSetUserDisplayName(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("identity.setUserDisplayName: engine unavailable")
	}
	for key := range args {
		if key != "userId" && key != "displayName" {
			return nil, fmt.Errorf("identity.setUserDisplayName: unsupported argument %q", key)
		}
	}
	service, err := adminops.New(&adminops.Service{
		Engine: i.engine,
		Logger: i.logger,
		Audit: &componentIdentity.SlogAuditLogger{
			Logger: i.logger,
			DB:     &componentIdentity.EngineAuditSink{Engine: i.engine, Logger: i.logger},
		},
	})
	if err != nil {
		return nil, err
	}
	userID, _ := args["userId"].(string)
	displayName, _ := args["displayName"].(string)
	result := service.SetUserDisplayName(ctx, userID, displayName)
	if !result.OK {
		return nil, fmt.Errorf("identity.setUserDisplayName: %s (audit %s)", result.ErrorMessage, result.AuditEventId)
	}
	return appReplyNode("setUserDisplayName", map[string]any{
		"ok":           true,
		"userId":       memql.BareShortId(strings.TrimSpace(userID)),
		"displayName":  strings.TrimSpace(displayName),
		"auditEventId": result.AuditEventId,
	}), nil
}

package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration is the email IntegrationProvider. It exposes a generic
// sendEmail capability to the MemQL DSL; product branches render their
// own bodies and call this capability to deliver.
type Integration struct {
	sender Sender
	logger *slog.Logger
}

// NewIntegration builds an email integration over the given Sender.
func NewIntegration(sender Sender, logger *slog.Logger) *Integration {
	return &Integration{sender: sender, logger: logger}
}

// IntegrationName returns the stable identifier.
func (i *Integration) IntegrationName() string { return "email" }

// Capabilities returns DSL-callable email operations.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "sendEmail",
			Description: "Deliver a transactional email. Caller supplies the rendered subject / text / html body.",
			Handler:     i.handleSendEmail,
			ArgsSchema: map[string]string{
				"to":       "string (required) - recipient address",
				"subject":  "string (required) - email subject",
				"textBody": "string (required) - plain-text body",
				"htmlBody": "string (optional) - HTML alternative",
			},
		},
		{
			Name: "status",
			Description: "Report this node's integration registry and the email lane's configuration. " +
				"Never includes a credential value -- only presence, source and the command that rotates it. " +
				"probe=true additionally runs a live, non-sending reachability check.",
			Handler: i.handleStatus,
			ArgsSchema: map[string]string{
				"probe": "boolean (optional) - run a live, non-sending reachability check against the configured provider",
			},
		},
	}
}

// handleStatus backs the `integrationStatus` builtin (memql#3323).
//
// ONE synthetic node, because that is what a top-level builtin call renders
// cleanly: the engine marshals the returned node map into a single result
// value, so a caller reads one payload rather than reassembling a set. The
// per-integration rows live inside that payload under `integrations`.
//
// The node is synthetic in the strict sense -- never persisted, no concept
// declaration, and a `concept` string deliberately outside the
// v{major}:{domain}:{entity} grammar so nothing mistakes it for graph state.
// This is process state; dsl/integrations/builtins.memql says why it is not a
// concept.
func (i *Integration) handleStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil {
		return nil, fmt.Errorf("email: integration not initialized")
	}
	if err := statusAuthorized(ctx); err != nil {
		return nil, err
	}

	probe := false
	if raw, ok := args["probe"]; ok {
		if b, isBool := raw.(bool); isBool {
			probe = b
		}
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"checkedAt":    time.Now().UTC().Format(time.RFC3339),
		"probed":       probe,
		"integrations": registryRollCall(i.emailReport(ctx, probe)),
	})
	if err != nil {
		return nil, fmt.Errorf("email.status: marshal report: %w", err)
	}

	return []memorynodes.MemoryNode{{
		ID:        "integrationStatus",
		Concept:   "integration:email:status",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// SenderAccess exposes the underlying Sender. Kept for Go callers that
// still render locally (guest_handlers.go) until the full guest-invite
// flow migrates to the DSL-plus-sendEmail path.
func (i *Integration) SenderAccess() Sender { return i.sender }

func (i *Integration) handleSendEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.sender == nil {
		return nil, fmt.Errorf("email: no sender configured")
	}

	msg := Message{
		To:       strings.TrimSpace(asString(args["to"])),
		Subject:  strings.TrimSpace(asString(args["subject"])),
		TextBody: asString(args["textBody"]),
		HTMLBody: asString(args["htmlBody"]),
	}
	if err := msg.Validate(); err != nil {
		return nil, err
	}

	if err := i.sender.Send(ctx, msg); err != nil {
		return nil, fmt.Errorf("email.sendEmail: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"delivered": true,
		"to":        msg.To,
		"subject":   msg.Subject,
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("email-send:%d", time.Now().UnixNano()),
		Concept:   "integration:email:send",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

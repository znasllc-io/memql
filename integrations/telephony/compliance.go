package telephony

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// complianceCapabilities are the consent + E911 management operations. Setting
// consent / E911 is owner/admin gated; the opt-out *check* runs inline in the
// outbound call path (CheckOutboundAllowed) and is not gated.
func (i *Integration) complianceCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "setConsent",
			Description: "Record TCPA consent / opt-out for an external number (owner/admin only). opted_out blocks future outbound calls.",
			Handler:     i.handleSetConsent,
			ArgsSchema: map[string]string{
				"phoneNumber": "string (required) - external E.164",
				"partitionId": "string (required)",
				"status":      "string (required) - opted_in | opted_out",
				"reason":      "string (optional)",
				"source":      "string (optional)",
			},
		},
		{
			Name:        "registerE911",
			Description: "Mark an owned DID as E911-registered + caller-ID verified before it goes live (owner/admin only).",
			Handler:     i.handleRegisterE911,
			ArgsSchema: map[string]string{
				"e164":          "string (required)",
				"e911AddressId": "string (optional) - carrier E911 address reference",
			},
		},
	}
}

// CheckOutboundAllowed enforces compliance before an outbound call: the callee
// must not be opted out (TCPA), and a caller-ID must be presented. Fails CLOSED
// on a consent-lookup error (treats an unverifiable number as not-allowed) so a
// transient DB issue can't leak a call to an opted-out party.
func (i *Integration) CheckOutboundAllowed(ctx context.Context, to, from string) error {
	if from == "" {
		return fmt.Errorf("telephony: outbound call requires a caller-ID (from)")
	}
	if i.engine == nil {
		return nil // no engine (tests): nothing to check against
	}
	optedOut, err := i.isOptedOut(ctx, to)
	if err != nil {
		return fmt.Errorf("telephony: consent check failed for %s (blocking): %w", to, err)
	}
	if optedOut {
		return fmt.Errorf("telephony: outbound call to %s blocked -- number is opted out (TCPA)", to)
	}
	return nil
}

// isOptedOut reports whether the newest consent state for a number is opted_out.
func (i *Integration) isOptedOut(ctx context.Context, e164 string) (bool, error) {
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query consentOptOut(phoneNumber: %q)`, e164))
	if err != nil {
		return false, err
	}
	return res != nil && res.Bundle != nil && len(res.Bundle.GetRootIds()) > 0, nil
}

func (i *Integration) handleSetConsent(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	phone, partitionID, status := asString(args["phoneNumber"]), asString(args["partitionId"]), asString(args["status"])
	if phone == "" || partitionID == "" || (status != "opted_in" && status != "opted_out") {
		return nil, fmt.Errorf("telephony.setConsent: phoneNumber, partitionId, and status (opted_in|opted_out) are required")
	}
	eng, err := i.requireEngine()
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(
		`mutation setConsent(phoneNumber: %q, partitionId: %q, status: %q, reason: %q, source: %q)`,
		phone, partitionID, status, asString(args["reason"]), asString(args["source"]),
	)
	if _, err := eng.Execute(ctx, q); err != nil {
		return nil, err
	}
	return handleNode("telephony:consent:set", map[string]string{"phoneNumber": phone, "status": status}), nil
}

func (i *Integration) handleRegisterE911(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	e164 := asString(args["e164"])
	if e164 == "" {
		return nil, fmt.Errorf("telephony.registerE911: e164 is required")
	}
	eng, err := i.requireEngine()
	if err != nil {
		return nil, err
	}
	id, err := i.numberRowID(ctx, e164)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(
		`mutation setNumberE911(id: %q, e911Registered: true, e911AddressId: %q, callerIdVerified: true)`,
		id, asString(args["e911AddressId"]),
	)
	if _, err := eng.Execute(ctx, q); err != nil {
		return nil, err
	}
	return handleNode("telephony:e911:registered", map[string]string{"e164": e164}), nil
}

// numberRowID resolves an owned DID's row id from its E.164.
func (i *Integration) numberRowID(ctx context.Context, e164 string) (string, error) {
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query numberByE164(e164: %q)`, e164))
	if err != nil {
		return "", err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.GetRootIds()) == 0 {
		return "", fmt.Errorf("telephony: owned number %s not found", e164)
	}
	return res.Bundle.GetRootIds()[0], nil
}

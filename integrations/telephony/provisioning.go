package telephony

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// provisioningCapabilities are the owner/admin-gated DID lifecycle operations
// backed by the active CarrierProvider. Backend functions only (no agent
// tools): buying numbers is privileged.
func (i *Integration) provisioningCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "searchNumbers",
			Description: "Search the carrier for purchasable DIDs (owner/admin only).",
			Handler:     i.handleSearchNumbers,
			ArgsSchema: map[string]string{
				"areaCode": "string (optional) - area / national destination code",
				"type":     "string (optional) - local | tollfree | mobile",
				"limit":    "integer (optional)",
			},
		},
		{
			Name:        "buyNumber",
			Description: "Purchase a DID, assign it to a partition, point it at our SIP edge, and provision inbound routing (owner/admin only).",
			Handler:     i.handleBuyNumber,
			ArgsSchema: map[string]string{
				"providerId":  "string (required) - the id from searchNumbers",
				"partitionId": "string (required) - tenant partition to assign the DID to",
				"purpose":     "string (optional) - inbound | outbound | both",
			},
		},
		{
			Name:        "configureInboundNumber",
			Description: "Point an owned DID at our SIP edge and (re)provision inbound routing (owner/admin only).",
			Handler:     i.handleConfigureInbound,
			ArgsSchema:  map[string]string{"e164": "string (required)", "partitionId": "string (required)"},
		},
		{
			Name:        "releaseNumber",
			Description: "Release an owned DID back to the carrier (owner/admin only).",
			Handler:     i.handleReleaseNumber,
			ArgsSchema:  map[string]string{"e164": "string (required)"},
		},
	}
}

// requireOwnerOrAdmin gates a privileged provisioning call. Mirrors the
// requiresOwnerOrAdmin DSL spec for Go capabilities.
func requireOwnerOrAdmin(ctx context.Context) error {
	u, ok := auth.UserFromContext(ctx)
	if !ok || !(auth.IsOwner(u) || auth.IsAdmin(u)) {
		return fmt.Errorf("telephony: number provisioning requires owner or admin")
	}
	return nil
}

func (i *Integration) handleSearchNumbers(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	q := NumberQuery{
		AreaCode: asString(args["areaCode"]),
		Type:     NumberType(asString(args["type"])),
		Limit:    asInt(args["limit"]),
	}
	nums, err := i.carrier.SearchNumbers(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]memorynodes.MemoryNode, 0, len(nums))
	for idx, n := range nums {
		b, _ := json.Marshal(n)
		out = append(out, memorynodes.MemoryNode{
			ID:        fmt.Sprintf("telephony:available:%s", n.ProviderID),
			Concept:   "integration:telephony:available-number",
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: time.Now().UTC().Add(time.Duration(-idx) * time.Nanosecond),
			Payload:   b,
		})
	}
	return out, nil
}

func (i *Integration) handleBuyNumber(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	providerID, partitionID := asString(args["providerId"]), asString(args["partitionId"])
	if providerID == "" || partitionID == "" {
		return nil, fmt.Errorf("telephony.buyNumber: providerId and partitionId are required")
	}
	purpose := asString(args["purpose"])
	if purpose == "" {
		purpose = "both"
	}
	num, err := i.carrier.BuyNumber(ctx, providerID)
	if err != nil {
		return nil, err
	}
	// Persist the number bound to its partition (Amendment A).
	i.recordNumber(ctx, num, partitionID, purpose)
	// Point the DID at our SIP edge + provision inbound routing so it accepts
	// calls immediately.
	if err := i.carrier.ConfigureInbound(ctx, num.E164, i.sipEdgeURI); err != nil && i.logger != nil {
		i.logger.Warn("telephony: configure-inbound after buy failed", "e164", num.E164, "err", err)
	}
	if trunkID, terr := i.EnsureInboundTrunk(ctx); terr == nil {
		if _, derr := i.EnsureDispatchRule(ctx, trunkID, num.E164, partitionID); derr != nil && i.logger != nil {
			i.logger.Warn("telephony: dispatch rule after buy failed", "e164", num.E164, "err", derr)
		}
	}
	num.Carrier = i.carrier.Name()
	return handleNode("telephony:number:bought", num), nil
}

func (i *Integration) handleConfigureInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	e164, partitionID := asString(args["e164"]), asString(args["partitionId"])
	if e164 == "" || partitionID == "" {
		return nil, fmt.Errorf("telephony.configureInboundNumber: e164 and partitionId are required")
	}
	if err := i.carrier.ConfigureInbound(ctx, e164, i.sipEdgeURI); err != nil {
		return nil, err
	}
	return i.handleProvisionInbound(ctx, map[string]any{"e164": e164, "partitionId": partitionID}, 0)
}

func (i *Integration) handleReleaseNumber(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwnerOrAdmin(ctx); err != nil {
		return nil, err
	}
	e164 := asString(args["e164"])
	if e164 == "" {
		return nil, fmt.Errorf("telephony.releaseNumber: e164 is required")
	}
	if err := i.carrier.ReleaseNumber(ctx, e164); err != nil {
		return nil, err
	}
	return handleNode("telephony:number:released", map[string]string{"e164": e164}), nil
}

// recordNumber persists a provisioned DID via mutationRecordNumber.
func (i *Integration) recordNumber(ctx context.Context, num Number, partitionID, purpose string) {
	eng, err := i.requireEngine()
	if err != nil {
		return
	}
	q := fmt.Sprintf(
		`mutationRecordNumber({e164: %q, carrier: %q, partitionId: %q, purpose: %q, providerId: %q, numberType: %q, monthlyCost: %s})`,
		num.E164, i.carrier.Name(), partitionID, purpose, num.ProviderID, string(orType(num.Type)), strconv.FormatFloat(num.MonthlyCost, 'f', -1, 64),
	)
	if _, err := eng.Execute(ctx, q); err != nil && i.logger != nil {
		i.logger.Warn("telephony: record number row failed", "e164", num.E164, "err", err)
	}
}

func orType(t NumberType) NumberType {
	if t == "" {
		return NumberTypeLocal
	}
	return t
}

// asInt coerces a tool/builtin arg to an int (handles float64 from JSON).
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

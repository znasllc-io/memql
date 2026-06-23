package telephony

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/livekit/protocol/livekit"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// inboundTrunkName is the single catch-all inbound trunk. Per-DID routing is
// done by dispatch rules (InboundNumbers filter), so one trunk fronts every
// provisioned DID.
const inboundTrunkName = "memql-telephony-inbound"

// inboundCapabilities are the DSL-callable inbound provisioning + call-record
// operations registered by Capabilities().
func (i *Integration) inboundCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "provisionInbound",
			Description: "Provision inbound routing for a DID: ensure the SIP inbound trunk and a dispatch rule mapping the DID to a partition-scoped room (tel-<partitionId>-*). Persists the trunk record.",
			Handler:     i.handleProvisionInbound,
			ArgsSchema: map[string]string{
				"e164":        "string (required) - the DID to route inbound",
				"partitionId": "string (required) - tenant partition the DID belongs to",
			},
		},
	}
}

// EnsureInboundTrunk returns the id of the catch-all inbound trunk, creating it
// once (idempotent by name). The trunk accepts calls to any DID routed to it;
// dispatch rules do the per-DID -> partition-room routing.
func (i *Integration) EnsureInboundTrunk(ctx context.Context) (string, error) {
	sip, err := i.requireSIP()
	if err != nil {
		return "", err
	}
	list, err := sip.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{})
	if err != nil {
		return "", fmt.Errorf("telephony: list inbound trunks: %w", err)
	}
	for _, t := range list.GetItems() {
		if t.GetName() == inboundTrunkName {
			return t.GetSipTrunkId(), nil
		}
	}
	created, err := sip.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
		Trunk: &livekit.SIPInboundTrunkInfo{
			Name: inboundTrunkName,
		},
	})
	if err != nil {
		return "", fmt.Errorf("telephony: create inbound trunk: %w", err)
	}
	return created.GetSipTrunkId(), nil
}

// EnsureDispatchRule creates an Individual dispatch rule routing inbound calls
// to `did` into a partition-scoped room (tel-<partitionId>-<auto>). The rule
// metadata carries the partition so the call-record path resolves it without
// parsing the room name (Amendment A).
func (i *Integration) EnsureDispatchRule(ctx context.Context, trunkID, did, partitionID string) (string, error) {
	sip, err := i.requireSIP()
	if err != nil {
		return "", err
	}
	meta, _ := json.Marshal(map[string]string{"partitionId": partitionID})
	rule, err := sip.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
		Rule: &livekit.SIPDispatchRule{
			Rule: &livekit.SIPDispatchRule_DispatchRuleIndividual{
				DispatchRuleIndividual: &livekit.SIPDispatchRuleIndividual{
					RoomPrefix: RoomPrefixForPartition(partitionID),
				},
			},
		},
		TrunkIds:       []string{trunkID},
		InboundNumbers: []string{did},
		Name:           "telephony-" + partitionID,
		Metadata:       string(meta),
	})
	if err != nil {
		return "", fmt.Errorf("telephony: create dispatch rule: %w", err)
	}
	return rule.GetSipDispatchRuleId(), nil
}

// handleProvisionInbound is the DSL capability: ensure trunk + dispatch rule
// for a DID -> partition room, then persist the trunk record.
func (i *Integration) handleProvisionInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	e164 := asString(args["e164"])
	partitionID := asString(args["partitionId"])
	if e164 == "" || partitionID == "" {
		return nil, fmt.Errorf("telephony.provisionInbound: e164 and partitionId are required")
	}
	carrier, err := i.requireCarrier()
	if err != nil {
		return nil, err
	}
	trunkID, err := i.EnsureInboundTrunk(ctx)
	if err != nil {
		return nil, err
	}
	ruleID, err := i.EnsureDispatchRule(ctx, trunkID, e164, partitionID)
	if err != nil {
		return nil, err
	}
	if eng, err := i.requireEngine(); err == nil {
		q := fmt.Sprintf(
			`recordTrunk({carrier: %q, direction: "inbound", name: %q, livekitTrunkId: %q, sipEdgeUri: %q, secretRef: "telephony-secrets"})`,
			carrier.Name(), inboundTrunkName, trunkID, i.sipEdgeURI,
		)
		if _, err := eng.Execute(ctx, q); err != nil && i.logger != nil {
			i.logger.Warn("telephony: record trunk row failed", "err", err)
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"e164":           e164,
		"partitionId":    partitionID,
		"trunkId":        trunkID,
		"dispatchRuleId": ruleID,
		"roomPrefix":     RoomPrefixForPartition(partitionID),
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("telephony-inbound:%s", e164),
		Concept:   "integration:telephony:inbound",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

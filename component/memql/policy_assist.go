package memql

import (
	"context"
	"encoding/json"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// PolicyProposal is a reviewable draft, never an assertion that a write ran.
// Revision comes from the server snapshot, not the model. Both Ask and Fleet
// save it through routingPolicySave, which validates again and performs CAS.
type PolicyProposal struct {
	Existing    bool     `json:"existing"`
	Action      string   `json:"action"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Primary     string   `json:"primary"`
	Fallbacks   []string `json:"fallbacks"`
	Explanation string   `json:"explanation"`
	Revision    int64    `json:"revision"`
}

var policyProposalSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action","name","description","primary","fallbacks","explanation"],"properties":{"action":{"type":"string","enum":["save","reset","unsupported"]},"name":{"type":"string"},"description":{"type":"string"},"primary":{"type":"string"},"fallbacks":{"type":"array","items":{"type":"string"}},"explanation":{"type":"string"}}}`)

func (e *MemQLEngine) policyDescribeBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := configurationAuthor(ctx); err != nil {
		return nil, err
	}
	sentence := stringArg(args, "sentence")
	if len(sentence) == 0 || len(sentence) > 12000 {
		return nil, fmt.Errorf("describe a policy in 1 to 12000 characters")
	}
	catalog, err := e.policies.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	result, err := e.InvokeAIStructured(ctx, "composeRoutingPolicy", map[string]any{"sentence": sentence, "catalog": string(encoded)}, "routing_policy_proposal", policyProposalSchema, true)
	if err != nil {
		return nil, err
	}
	var proposal PolicyProposal
	if err := json.Unmarshal([]byte(result), &proposal); err != nil {
		return nil, fmt.Errorf("the model returned an unreadable policy proposal: %w", err)
	}
	if proposal.Action == "unsupported" {
		return nil, fmt.Errorf("cannot compose this policy: %s", proposal.Explanation)
	}
	for _, entry := range catalog {
		if entry.Name == proposal.Name {
			proposal.Existing = true
		}
	}
	if len(catalog) > 0 {
		proposal.Revision = catalog[0].Revision
	}
	// Validate without persisting by exercising the SAME Save/Reset path
	// against an isolated store containing the exact inspected document.
	doc, defaults, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, err
	}
	if doc.Revision != proposal.Revision {
		return nil, fmt.Errorf("policies changed while composing; ask again against the new revision")
	}
	dry := newPolicyRegistry()
	dry.byName = defaults
	dry.AttachStore(&proposalPolicyStore{doc: doc})
	if proposal.Action == "reset" {
		err = dry.Reset(ctx, proposal.Revision, proposal.Name)
	} else if proposal.Action == "save" {
		err = dry.Save(ctx, proposal.Revision, PolicyConfig{Name: proposal.Name, Description: proposal.Description, Primary: proposal.Primary, Fallbacks: proposal.Fallbacks})
	} else {
		err = fmt.Errorf("the model proposed an unknown policy action")
	}
	if err != nil {
		return nil, fmt.Errorf("the proposed policy was refused: %w", err)
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "policy-proposal", Concept: "v1:router:policyCatalog", Payload: raw}}, nil
}

type proposalPolicyStore struct{ doc PolicyDocument }

func (s *proposalPolicyStore) Read(context.Context) (PolicyDocument, error) { return s.doc, nil }
func (s *proposalPolicyStore) CompareAndSwap(context.Context, int64, PolicyDocument) error {
	return nil
}

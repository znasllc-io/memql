package calls

import (
	_ "embed"
	"encoding/json"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// Template content and ordering are declared configuration, not native product
// branching. Every catalog item passes the same validation as an owner's draft.
//
//go:embed workflows/catalog.json
var workflowCatalogJSON []byte

func workflowCatalog() ([]callrules.WorkflowDefinition, error) {
	var out []callrules.WorkflowDefinition
	if json.Unmarshal(workflowCatalogJSON, &out) != nil {
		return nil, accountRefusal("workflow_catalog_unavailable")
	}
	for _, d := range out {
		if len(callrules.ValidateWorkflow(d)) != 0 {
			return nil, accountRefusal("workflow_catalog_unavailable")
		}
	}
	return out, nil
}

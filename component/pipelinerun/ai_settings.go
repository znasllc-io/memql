package pipelinerun

import (
	"context"

	"github.com/znasllc-io/memql/component/pipelines"
)

// aiDiagnosisOffMessage is recorded on a failed run whose maintenance policy
// asked for an AI analysis while the owner's diagnosis switch is off.
const aiDiagnosisOffMessage = "AI failure diagnosis is off in Pipelines settings."

// aiSettings reads the owner's pipeline AI switches (pipelines.AISettingsVariable)
// at the moment a feature would use them. Both are off unless the owner turned
// them on: an absent, unreadable or unparseable value reads as off, and so
// does a node with no variable reader. Reading at use rather than at run open
// keeps the switches out of the captured policy, so turning one never refuses
// a review in flight (independentReview's policy fence) and applies to the next
// review or failure, never to one already started.
func (d Deps) aiSettings(ctx context.Context) pipelines.AISettings {
	if d.SystemVariable == nil {
		return pipelines.AISettings{}
	}
	raw, err := d.SystemVariable(ctx, pipelines.AISettingsVariable)
	if err != nil {
		return pipelines.AISettings{}
	}
	return pipelines.ParseAISettings(raw)
}

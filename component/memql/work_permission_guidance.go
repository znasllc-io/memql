package memql

import (
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// WorkPermissionGuidance identifies a correction to a typed scope or an
// untyped request bound by the server to its exact pending native tool call. This is permission to reconsider the saved turn, never a computer
// grant. The native continuation still proves the owner, exact question,
// execution lineage and immutable checkpoint before calling the model.
func WorkPermissionGuidance(kind string, subject, answer map[string]any, artifactHash string) bool {
	if !workPermissionGuidanceSubject(kind, subject, artifactHash) {
		return false
	}
	feedback, ok := answer["feedback"].(string)
	return ok && len(answer) == 1 && strings.TrimSpace(feedback) != "" && feedback == strings.TrimSpace(feedback) && utf8.RuneCountInString(feedback) <= 4000
}

func workPermissionGuidanceSubject(kind string, subject map[string]any, artifactHash string) bool {
	if kind != work.ApprovalKindScopeElevation || stringField(subject, "agentId") == "" || work.ArtifactHash(subject) != artifactHash {
		return false
	}
	if typed, present := subject["computerPermission"]; present {
		if _, err := permission.Parse(typed); err != nil {
			return false
		}
	} else if _, ok := scopeCheckpointWitness(subject); !ok {
		return false
	}
	return true
}

package memql

import (
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// WorkPermissionGuidance identifies a correction to a server-produced typed
// scope. This is permission to reconsider the saved turn, never a computer
// grant. The native continuation still proves the owner, exact question,
// execution lineage and immutable checkpoint before calling the model.
func WorkPermissionGuidance(kind string, subject, answer map[string]any, artifactHash string) bool {
	if kind != work.ApprovalKindScopeElevation || stringField(subject, "agentId") == "" || work.ArtifactHash(subject) != artifactHash {
		return false
	}
	if _, err := permission.Parse(subject["computerPermission"]); err != nil {
		return false
	}
	feedback, ok := answer["feedback"].(string)
	return ok && len(answer) == 1 && strings.TrimSpace(feedback) != "" && feedback == strings.TrimSpace(feedback) && utf8.RuneCountInString(feedback) <= 4000
}

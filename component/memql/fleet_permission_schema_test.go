package memql

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/dsl"
)

// Exercise the shipped tool, not a separately maintained schema. These are the
// malformed operation shapes observed in a fleet home-directory request.
func TestFleetPermissionToolPublishesItsNestedContract(t *testing.T) {
	raw, err := fs.ReadFile(dsl.Tree(), "worker/tools.memql")
	require.NoError(t, err)
	source := string(raw)
	start := strings.Index(source, "tool requestComputerUseScope {")
	end := strings.Index(source[start:], "\n/// Perform a headless")
	decl, err := langparser.ParseToolDecl(source[start : start+end])
	require.NoError(t, err)
	tools, err := toolDeclToTool(decl, "worker/tools.memql")
	require.NoError(t, err)
	schema := compileSchema(t, tools[0].InputSchema)
	base := map[string]any{"intent": "List home files", "summary": "Read the chosen home", "requestedScope": "observe", "ownerUserId": "owner", "agentId": "agent"}
	for _, tc := range []struct {
		name, operation string
		valid           bool
	}{
		{"listing", `{"tool":"workerHost","action":"fs_list","args":{"path":"/home/person"},"directory":"/home/person"}`, true},
		{"command", `{"tool":"workerHost","action":"exec","args":{"cmd":"ls -la","cwd":"/home/person"}}`, true},
		{"missing tool", `{"action":"fs_list","args":{"path":"/home"}}`, false},
		{"missing args", `{"tool":"workerHost","action":"fs_list"}`, false},
		{"unknown action", `{"tool":"workerHost","action":"list","args":{"path":"/home"}}`, false},
		{"wrong command field", `{"tool":"workerHost","action":"exec","args":{"command":"ls","cwd":"/home/person"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var operation map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.operation), &operation))
			base["operation"] = operation
			err := schema.Validate(base)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

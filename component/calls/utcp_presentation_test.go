package calls

import (
	"strings"
	"testing"
)

func TestPresentationToolsKeepReadAndMutationAuthoritySeparate(t *testing.T) {
	count := 0
	for _, raw := range UTCPManual("https://example.test/_memql/calls/agent")["tools"].([]any) {
		tool := raw.(map[string]any)
		if !strings.HasPrefix(tool["name"].(string), "callkey_workflow_presentation_") {
			continue
		}
		count++
		request := tool["inputs"].(map[string]any)["properties"].(map[string]any)["request"].(map[string]any)
		workflow := request["properties"].(map[string]any)["workflow"].(map[string]any)
		props := workflow["properties"].(map[string]any)
		if workflow["additionalProperties"] != false || props["observation"] != nil || props["question"] != nil {
			t.Fatal("mixed presentation authority accepted")
		}
		if strings.HasSuffix(tool["name"].(string), "_status") {
			if len(props) != 1 || !strings.Contains(tool["description"].(string), "requires skills_read") {
				t.Fatal("status tool exposes mutable authority")
			}
		} else {
			if len(props) != 6 || !strings.Contains(tool["description"].(string), "BOTH skills_control and control") {
				t.Fatal("presentation mutation lacks exact guarded envelope")
			}
			for _, key := range []string{"runId", "expectedRevision", "expectedEpoch", "idempotencyKey", "presentation"} {
				if props[key] == nil {
					t.Fatalf("missing %s", key)
				}
			}
		}
		output := tool["outputs"].(map[string]any)["properties"].(map[string]any)
		if len(output) != 2 || output["presentation"] == nil || output["serverNow"] == nil {
			t.Fatal("tool leaks unrelated workflow result schema")
		}
	}
	if count != 3 {
		t.Fatalf("got %d presentation tools", count)
	}
}

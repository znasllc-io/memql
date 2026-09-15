package memql

import "testing"

func TestPayloadPathBuildersRejectSQLSyntax(t *testing.T) {
	for _, build := range []struct {
		name string
		fn   func([]string) (string, error)
	}{{"text", buildJSONPathExpression}, {"jsonb", buildJSONBPathExpression}} {
		for _, segment := range []string{"x' OR TRUE --", "x\\", "x,y", "x}", "x{", "x;DROP TABLE memory_nodes", "x\x00", "x\n'"} {
			t.Run(build.name+"/"+segment, func(t *testing.T) {
				if sql, err := build.fn([]string{"profile", segment}); err == nil || sql != "" {
					t.Fatalf("unsafe path accepted: sql=%q err=%v", sql, err)
				}
			})
		}
	}
}

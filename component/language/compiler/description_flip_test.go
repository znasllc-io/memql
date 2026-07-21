package compiler

// memql#2634: the compiler's function/automation JSON generators resolve the
// /// doc comment over @description.

import (
	"strings"
	"testing"
)

func TestCompiledDescriptions_DocCommentPrecedence(t *testing.T) {
	src := strings.Join([]string{
		"/// Automation doc channel.",
		"@trigger(event=\"system.shutdown\")",
		"@description(\"Automation annot channel.\")",
		"automation flipAuto {",
		"  args {",
		"    node any",
		"  }",
		"",
		"  step persist {",
		"    mutation createSpawnEvent (",
		"      nodeId: coalesce(node.id, \"\")",
		"    )",
		"  }",
		"}",
	}, "\n")
	res, err := CompileSource(src)
	if err != nil {
		t.Fatalf("CompileSource: %v", err)
	}
	if len(res.Automations) == 0 {
		t.Fatal("no automation compiled")
	}
	auto := res.Automations[0]
	if auto.Description != "Automation doc channel." {
		t.Errorf("AutomationOutput.Description = %q, want the /// channel", auto.Description)
	}
	if d, _ := auto.JSON["description"].(string); d != "Automation doc channel." {
		t.Errorf("compiled JSON description = %q, want the /// channel", d)
	}
}

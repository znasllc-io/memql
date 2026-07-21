package actions

// memql#2634: the action loader resolves the /// doc comment over
// @description (ruling 3).

import (
	"testing"
)

func TestLoadSource_DocCommentPrecedence(t *testing.T) {
	src := `use capabilities.shell.{ script }

/// Action doc channel.
@description("Action annot channel.")
action flipAction {
  args { }
  capability script(script: "deploy.noop")
}`
	acts, err := LoadSource(src, "t.memql")
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want 1", len(acts))
	}
	if acts[0].Description != "Action doc channel." {
		t.Errorf("action Description = %q, want the /// channel", acts[0].Description)
	}
}

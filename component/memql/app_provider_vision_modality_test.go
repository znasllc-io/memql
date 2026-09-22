package memql

import (
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// app_provider_vision_modality_test.go -- the app door serves vision, and
// still serves nothing it deliberately refused (issue memql#5523).
//
// THE REGISTRATION IS THE FEATURE, and the router decides by a runtime type
// assertion: `servesModality` in component/router asks
// `client.(common.VisionAIProvider)` and SKIPS a chain entry that does not
// answer. So before CallVision existed, every vision call walked past every
// app door silently -- and the decision row showed a chain that CONSIDERED the
// app and chose something else, which is indistinguishable from a policy that
// meant to.
//
// The `var _ common.VisionAIProvider` assertion beside the methods proves the
// interface is satisfied. What it cannot say is which OTHER interfaces the new
// method dragged in, and that half matters more: the tool-calling surfaces are
// absent on purpose, because a tool turn is MemQL driving and driving an app
// that is itself driving produces two agents fighting over one conversation.
// A provider that accidentally satisfied one of those would be selected for a
// tool turn and would break it, and the absence is enforced by nothing except
// the methods not being there.

func TestTheAppDoorServesVisionAndNothingItRefused(t *testing.T) {
	var client any = &appProvider{appId: "claude-code"}

	// WHAT IT MUST SERVE. Each is a modality component/router asks about by
	// exactly this assertion.
	if _, ok := client.(common.VisionAIProvider); !ok {
		t.Error("the app provider does not implement common.VisionAIProvider, so the router's " +
			"vision walk skips every app door and an operator reading the decision row sees a " +
			"chain that considered the app and passed")
	}
	if _, ok := client.(common.ChatAIProvider); !ok {
		t.Error("the app provider does not serve plain chat")
	}
	if _, ok := client.(common.ChatStructuredProvider); !ok {
		t.Error("the app provider does not serve structured chat")
	}

	// WHAT IT MUST NOT SERVE, and this is the half a compile-time assertion
	// cannot state. See the file header on app_provider.go: MemQL drives a
	// tool loop and an app drives itself.
	refused := map[string]bool{
		"common.ToolCallingChatAIProvider": func() bool {
			_, ok := client.(common.ToolCallingChatAIProvider)
			return ok
		}(),
		"common.ChatStreamProvider": func() bool {
			_, ok := client.(common.ChatStreamProvider)
			return ok
		}(),
		"common.ChatStreamWithToolsProvider": func() bool {
			_, ok := client.(common.ChatStreamWithToolsProvider)
			return ok
		}(),
	}
	for name, serves := range refused {
		if serves {
			t.Errorf("the app provider now satisfies %s -- the router will select an app door for "+
				"a turn MemQL drives, and an app that drives itself is a second agent in the same "+
				"conversation. A method added for another modality has reached one of these.", name)
		}
	}
}

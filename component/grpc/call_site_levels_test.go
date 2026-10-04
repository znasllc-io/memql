package memql

// call_site_levels_test.go -- the table that pins what every model call the AI
// handlers make declares (epic memql#5127, design D2, record section 6).
//
// The sibling of app/call_site_levels_test.go, and it exists separately for
// the ordinary reason: the request builders are unexported in each package,
// and a single table would have to export them to reach across the boundary --
// widening a surface to test it, which is the trade this repo does not make.
//
// What it checks is the REQUEST, not the resolution: the level asked for, the
// modality derived, whether a caller-named provider became a PIN, and that the
// context floor was actually estimated. A resolution needs a router, a
// registry and a database; a request is a pure function over values, so this
// runs everywhere rather than skipping.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestEveryAiHandlerCallSiteCarriesTheExpectedLevelAndModality(t *testing.T) {
	messages := []common.ChatMessage{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "what changed in the fleet today?"},
	}

	cases := []struct {
		site             string
		req              airoute.ResolveRequest
		level            airoute.Level
		modality         airoute.Modality
		promptName       string
		wantStructured   bool
		explicitProvider string
		pinnedBy         string
	}{
		{
			site:       "handleAiChatNonStream",
			req:        chatResolveRequest(messages, "", "v1:identity:user:asker"),
			level:      airoute.LevelStrong,
			modality:   airoute.ModalityChat,
			promptName: askPromptName,
		},
		{
			site:             "handleAiChatNonStream (caller pinned a provider)",
			req:              chatResolveRequest(messages, "chat54Mini", "v1:identity:user:asker"),
			level:            airoute.LevelStrong,
			modality:         airoute.ModalityChat,
			promptName:       askPromptName,
			explicitProvider: "chat54Mini",
			pinnedBy:         "v1:identity:user:asker",
		},
		{
			site:       "handleAiChatStream",
			req:        chatStreamResolveRequest(messages, "", "v1:identity:user:asker"),
			level:      airoute.LevelStrong,
			modality:   airoute.ModalityStreamingChat,
			promptName: askPromptName,
		},
		{
			site:             "handleAiChatStream (caller pinned a provider)",
			req:              chatStreamResolveRequest(messages, "streamClaudeSonnet", "v1:identity:user:asker"),
			level:            airoute.LevelStrong,
			modality:         airoute.ModalityStreamingChat,
			promptName:       askPromptName,
			explicitProvider: "streamClaudeSonnet",
			pinnedBy:         "v1:identity:user:asker",
		},
		{
			site:           "callSuggestWithSchema",
			req:            suggestResolveRequest(messages),
			level:          airoute.LevelFast,
			modality:       airoute.ModalityStructured,
			wantStructured: true,
		},
	}

	for _, c := range cases {
		t.Run(c.site, func(t *testing.T) {
			if c.req.Level != c.level {
				t.Errorf("declares level %q, expected %q", c.req.Level, c.level)
			}
			if c.req.Modality != c.modality {
				t.Errorf("derives modality %q, expected %q", c.req.Modality, c.modality)
			}
			if !c.req.Level.Valid() {
				t.Errorf("level %q is outside the closed four (%s)", c.req.Level, airoute.LevelNames())
			}
			if !c.req.Modality.Valid() {
				t.Errorf("modality %q is one the router does not derive", c.req.Modality)
			}
			if c.req.PromptName != c.promptName {
				t.Errorf("PromptName is %q, expected %q -- a rule branches on it", c.req.PromptName, c.promptName)
			}
			if c.req.Needs.Structured != c.wantStructured {
				t.Errorf("Needs.Structured is %v, expected %v", c.req.Needs.Structured, c.wantStructured)
			}
			if c.req.ExplicitProvider != c.explicitProvider {
				t.Errorf("ExplicitProvider is %q, expected %q -- a caller-named provider is a PIN, never a registry lookup",
					c.req.ExplicitProvider, c.explicitProvider)
			}
			// A caller-named provider is the CALLER's pin, and an unpinned
			// call names nobody -- the app gate admits a pinned app door only
			// when the pin is the session owner's own.
			if c.req.PinnedBy != c.pinnedBy {
				t.Errorf("PinnedBy is %q, expected %q", c.req.PinnedBy, c.pinnedBy)
			}
		})
	}
}

// TestTheChatSitesEstimateTheirContextFloor pins the half of the request that
// a table of constants cannot: MinContextTokens is DERIVED from the messages
// the call actually carries.
//
// The failure it prevents is quiet. A floor of zero admits every entry, and on
// the decision record it reads exactly like a floor that was measured and
// cleared -- so "nobody estimated" and "anything will do" would be the same
// value, and a long conversation would resolve onto a small-window model and
// truncate.
func TestTheChatSitesEstimateTheirContextFloor(t *testing.T) {
	short := []common.ChatMessage{{Role: "user", Content: "hi"}}
	long := []common.ChatMessage{{Role: "user", Content: strings.Repeat("a very long turn. ", 4000)}}

	shortFloor := chatResolveRequest(short, "", "").Needs.MinContextTokens
	longFloor := chatResolveRequest(long, "", "").Needs.MinContextTokens

	if shortFloor <= 0 {
		t.Fatalf("the short turn declares a floor of %d; a zero floor admits every entry and is indistinguishable "+
			"on the record from one that was measured", shortFloor)
	}
	if longFloor <= shortFloor {
		t.Fatalf("a turn carrying ~72,000 characters declares floor %d and a two-character one declares %d: "+
			"the floor is not derived from the messages at all", longFloor, shortFloor)
	}

	// The streaming site is the same turn with a different modality, so its
	// floor must be the same number. A divergence here means one of the two
	// stopped counting the messages.
	if got := chatStreamResolveRequest(long, "", "").Needs.MinContextTokens; got != longFloor {
		t.Fatalf("the streaming site declares floor %d for the same messages the non-streaming site floors at %d",
			got, longFloor)
	}
}

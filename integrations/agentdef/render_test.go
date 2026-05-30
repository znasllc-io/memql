package agentdef

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRenderIdentityBlock_Golden pins the shared identity region byte-for-byte
// across the representative agent shapes from the spike (#476 section 7.2): a
// full specialist, a personality-only GA, a description-only agent, and an
// empty agent. A drift in the renderer fails this test in the default CI lane.
func TestRenderIdentityBlock_Golden(t *testing.T) {
	cases := []struct {
		name string
		in   AgentGenerationContract
		want string
	}{
		{
			name: "full specialist (description + domains + personality)",
			in: AgentGenerationContract{
				Name:        "Sofia",
				Description: "Sales Specialist",
				Domains:     []string{"sales", "support"},
				Personality: "Warm, concise, and proactive.",
			},
			want: "Role: Sales Specialist\n" +
				"Domains: sales, support\n" +
				"\n" +
				"Warm, concise, and proactive.",
		},
		{
			name: "GA: personality only, no description or domains",
			in: AgentGenerationContract{
				Name:        "Assistant",
				Personality: "You are a helpful general assistant.",
			},
			want: "You are a helpful general assistant.",
		},
		{
			name: "description only, no domains, no personality",
			in: AgentGenerationContract{
				Name:        "Atlas",
				Description: "Research Specialist",
			},
			want: "Role: Research Specialist",
		},
		{
			name: "domains + personality, no description",
			in: AgentGenerationContract{
				Domains:     []string{"finance"},
				Personality: "Precise and numerate.",
			},
			want: "Domains: finance\n\nPrecise and numerate.",
		},
		{
			name: "empty contract renders empty",
			in:   AgentGenerationContract{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RenderIdentityBlock(tc.in))
		})
	}
}

package memql

import (
	"strings"
	"testing"
)

func TestParseSeedMemQL_HappyPath(t *testing.T) {
	src := `
use platform.partition

@scope("global")
@description("The default partition seeded at cluster bootstrap.")
seed defaultPartition {
  id:            "default"
  partitionType: "standard"
  displayName:   "Default"
}
`
	decl, err := parseSeedMemQL("default-partition.memql", []byte(src))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if decl.name != "defaultPartition" {
		t.Errorf("name = %q, want defaultPartition", decl.name)
	}
	if decl.useNamespace != "platform" || decl.useConcept != "partition" {
		t.Errorf("use = %s.%s, want platform.partition", decl.useNamespace, decl.useConcept)
	}
	if decl.scope != "global" {
		t.Errorf("scope = %q, want global", decl.scope)
	}
	if decl.description != "The default partition seeded at cluster bootstrap." {
		t.Errorf("description = %q", decl.description)
	}
	if got := decl.body.fields["id"].str; got != "default" {
		t.Errorf("body.id = %q, want default", got)
	}
	if got := decl.body.fields["partitionType"].str; got != "standard" {
		t.Errorf("body.partitionType = %q, want standard", got)
	}
}

func TestParseSeedMemQL_PerUserWithNestedBlocks(t *testing.T) {
	src := `
use agents.agent

@version("1.0.0")
@scope("perUser")
@templateFile("templates/assistant.tmpl")
@description("Per-user Assistant baseline.")
seed assistant {
  name:        "Assistant"
  description: "Designated fallback when no specialist fits."
  role:        "assistant"
  roleSlug:    "assistant"
  gender:      "female"

  providerConfig {
    llm {
      policyName:  "balancedChat"
      temperature: 0.7
      maxTokens:   4000
    }
  }

  capabilities {
    avatar:       true
    lipSync:      true
    vision:       true
    voiceToVoice: true
    claw:         false
    domains:      ["general"]
    keywords:     []
  }

  audioControl: "mirror_user"
  videoControl: "mirror_user"
}
`
	decl, err := parseSeedMemQL("general-assistant.memql", []byte(src))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if decl.name != "assistant" {
		t.Errorf("name = %q", decl.name)
	}
	if decl.scope != "perUser" {
		t.Errorf("scope = %q, want perUser", decl.scope)
	}
	if decl.templateFile != "templates/assistant.tmpl" {
		t.Errorf("templateFile = %q", decl.templateFile)
	}

	// Top-level scalars
	if got := decl.body.fields["name"].str; got != "Assistant" {
		t.Errorf("body.name = %q", got)
	}

	// Nested providerConfig.llm.policyName
	pc, ok := decl.body.nested["providerConfig"]
	if !ok {
		t.Fatal("missing providerConfig nested block")
	}
	llm, ok := pc.nested["llm"]
	if !ok {
		t.Fatal("missing providerConfig.llm nested block")
	}
	if got := llm.fields["policyName"].str; got != "balancedChat" {
		t.Errorf("llm.policyName = %q, want balancedChat", got)
	}
	if got := llm.fields["temperature"].floatV; got != 0.7 {
		t.Errorf("llm.temperature = %v, want 0.7", got)
	}
	if got := llm.fields["maxTokens"].intV; got != 4000 {
		t.Errorf("llm.maxTokens = %d, want 4000", got)
	}

	// Nested capabilities.* (mixed types incl. bool + string array)
	cap, ok := decl.body.nested["capabilities"]
	if !ok {
		t.Fatal("missing capabilities block")
	}
	if got := cap.fields["avatar"].boolV; !got {
		t.Errorf("capabilities.avatar = false, want true")
	}
	if got := cap.fields["claw"].boolV; got {
		t.Errorf("capabilities.claw = true, want false")
	}
	if got := cap.fields["domains"].stringsV; len(got) != 1 || got[0] != "general" {
		t.Errorf("capabilities.domains = %v, want [general]", got)
	}
	if got := cap.fields["keywords"].stringsV; len(got) != 0 {
		t.Errorf("capabilities.keywords = %v, want []", got)
	}
}

func TestParseSeedMemQL_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "missing use clause",
			src:     `@scope("global") seed foo { id: "x" }`,
			wantErr: "requires a `use",
		},
		{
			name:    "func-receiver form rejected",
			src:     "use platform.partition\nfunc (Seed) foo() {}",
			wantErr: "no `func (Seed)` form exists",
		},
		{
			name:    "invalid scope",
			src:     `use platform.partition` + "\n" + `@scope("perCluster") seed foo { id: "x" }`,
			wantErr: `@scope must be "global" or "perUser"`,
		},
		{
			name:    "unknown annotation",
			src:     `use platform.partition` + "\n" + `@nonsense("x") seed foo { id: "y" }`,
			wantErr: "unknown seed annotation",
		},
		{
			name:    "no seed decl",
			src:     `use platform.partition`,
			wantErr: "no seed declaration found",
		},
		{
			name:    "duplicate field",
			src:     `use platform.partition` + "\n" + `seed foo { id: "x" id: "y" }`,
			wantErr: "duplicate field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSeedMemQL("test.memql", []byte(tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}


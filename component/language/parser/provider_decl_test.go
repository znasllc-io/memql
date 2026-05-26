package parser

import "testing"

// TestParseProviderDecl_GoldenPath_OpenAIChild locks the typical
// child-provider authoring shape: @description + @type + @model +
// auth + params sub-blocks. env("VAR") in auth lands as "${VAR}".
func TestParseProviderDecl_GoldenPath_OpenAIChild(t *testing.T) {
	source := `@description("OpenAI GPT-5 Mini for fast chat completions")
@type("OpenAI")
@model("gpt-5-mini")
@modality("text")
provider chat5Mini {
  auth {
    apiKey     env("MEMQL_SI_OPENAI_API_KEY")
    projectId  env("MEMQL_SI_OPENAI_PROJECT_ID")
  }
  params {
    maxTokens            4096
    maxCompletionTokens  4096
    temperature          0.7
    voice                "nova"
  }
}`

	got, err := ParseProviderDecl(source)
	if err != nil {
		t.Fatalf("ParseProviderDecl: %v", err)
	}
	if got.Name != "chat5Mini" {
		t.Errorf("Name = %q, want chat5Mini", got.Name)
	}
	if got.Type != "OpenAI" {
		t.Errorf("Type = %q, want OpenAI", got.Type)
	}
	if got.Model != "gpt-5-mini" {
		t.Errorf("Model = %q, want gpt-5-mini", got.Model)
	}
	if got.Modality != "text" {
		t.Errorf("Modality = %q, want text", got.Modality)
	}
	if got.Description != "OpenAI GPT-5 Mini for fast chat completions" {
		t.Errorf("Description mismatch: %q", got.Description)
	}
	if got.IsBase {
		t.Error("IsBase = true, want false (this is a child provider)")
	}
	if got.IsDefault {
		t.Error("IsDefault = true, want false (no @default)")
	}
	if got.Auth["apiKey"] != "${MEMQL_SI_OPENAI_API_KEY}" {
		t.Errorf("Auth[apiKey] = %q, want ${MEMQL_SI_OPENAI_API_KEY}", got.Auth["apiKey"])
	}
	if got.Auth["projectId"] != "${MEMQL_SI_OPENAI_PROJECT_ID}" {
		t.Errorf("Auth[projectId] = %q, want ${MEMQL_SI_OPENAI_PROJECT_ID}", got.Auth["projectId"])
	}
	if got.Params["maxTokens"] != 4096 {
		t.Errorf("Params[maxTokens] = %v (%T), want 4096 int", got.Params["maxTokens"], got.Params["maxTokens"])
	}
	if got.Params["temperature"] != 0.7 {
		t.Errorf("Params[temperature] = %v (%T), want 0.7 float64", got.Params["temperature"], got.Params["temperature"])
	}
	if got.Params["voice"] != "nova" {
		t.Errorf("Params[voice] = %q, want nova", got.Params["voice"])
	}
}

// TestParseProviderDecl_BaseProvider locks the vendor-level @base
// shape: no @model, no params, auth only. Used by `provider openai
// { auth { apiKey env("...") } }` -- the entry every chat/embedding/
// tts child extends from via @extends("openai").
func TestParseProviderDecl_BaseProvider(t *testing.T) {
	source := `@base
@type("OpenAI")
provider openai {
  auth {
    apiKey  env("MEMQL_SI_OPENAI_API_KEY")
  }
}`

	got, err := ParseProviderDecl(source)
	if err != nil {
		t.Fatalf("ParseProviderDecl: %v", err)
	}
	if !got.IsBase {
		t.Error("IsBase = false, want true")
	}
	if got.Type != "OpenAI" {
		t.Errorf("Type = %q, want OpenAI", got.Type)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want empty (base providers have no model)", got.Model)
	}
	if got.Auth["apiKey"] != "${MEMQL_SI_OPENAI_API_KEY}" {
		t.Errorf("Auth[apiKey] = %q, want ${MEMQL_SI_OPENAI_API_KEY}", got.Auth["apiKey"])
	}
}

// TestParseProviderDecl_Extends locks @extends("parentName") parsing.
// Note: the parent's Type/Auth resolution is the loader's job; this
// test only confirms the parser surfaces the Extends string.
func TestParseProviderDecl_Extends(t *testing.T) {
	source := `@extends("openai")
@model("gpt-4o-mini")
provider chat4oMini {
  params {
    maxTokens 8192
  }
}`

	got, err := ParseProviderDecl(source)
	if err != nil {
		t.Fatalf("ParseProviderDecl: %v", err)
	}
	if got.Extends != "openai" {
		t.Errorf("Extends = %q, want openai", got.Extends)
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want gpt-4o-mini", got.Model)
	}
}

// TestParseProviderDecl_AuthStringLiteral locks support for
// quoted-string auth values (rare; mostly fixtures / tests).
func TestParseProviderDecl_AuthStringLiteral(t *testing.T) {
	source := `@type("OpenAI")
@model("gpt-test")
provider testProvider {
  auth {
    apiKey  "sk-test-literal"
  }
}`

	got, err := ParseProviderDecl(source)
	if err != nil {
		t.Fatalf("ParseProviderDecl: %v", err)
	}
	if got.Auth["apiKey"] != "sk-test-literal" {
		t.Errorf("Auth[apiKey] = %q, want sk-test-literal", got.Auth["apiKey"])
	}
}

// TestParseProviderDecl_RejectsUnknownSubBlock locks the body
// grammar: only `auth` and `params` are accepted.
func TestParseProviderDecl_RejectsUnknownSubBlock(t *testing.T) {
	source := `@type("OpenAI")
@model("gpt-x")
provider bogusProvider {
  bogusBlock {
    foo  "bar"
  }
}`

	_, err := ParseProviderDecl(source)
	if err == nil {
		t.Fatal("expected error for unknown sub-block, got nil")
	}
}

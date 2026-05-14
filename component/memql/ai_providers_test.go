package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseProviderConfigsSupportsArrays(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "secret")

	raw := []byte(`
[
  {
    "name": "providerA",
    "type": "OpenAI",
    "model": "gpt",
    "default": true,
    "auth": {
      "apiKey": "${TEST_OPENAI_KEY}"
    }
  },
  {
    "name": "providerB",
    "type": "OpenAI",
    "model": "gpt",
    "auth": {
      "apiKey": "${TEST_OPENAI_KEY}"
    },
    "params": {
      "temperature": 0
    }
  }
]`)

	configs, err := parseProviderConfigs("test.json", raw)
	require.NoError(t, err)
	require.Len(t, configs, 2)

	require.Equal(t, "providerA", configs[0].Name)
	require.True(t, configs[0].Default)
	require.Equal(t, "secret", configs[0].Auth["apiKey"])

	require.Equal(t, "providerB", configs[1].Name)
	require.False(t, configs[1].Default)
	require.Equal(t, 0.0, configs[1].Params["temperature"])
}

func TestParseProviderConfigsSingleObject(t *testing.T) {
	t.Setenv("TEST_SINGLE_KEY", "abc123")
	raw := []byte(`{"name":"single","type":"OpenAI","model":"gpt","auth":{"apiKey":"${TEST_SINGLE_KEY}"}}`)
	configs, err := parseProviderConfigs("single.json", raw)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	require.Equal(t, "single", configs[0].Name)
	require.Equal(t, "abc123", configs[0].Auth["apiKey"])
}

func TestParseProviderMemQL_BaseProvider(t *testing.T) {
	raw := []byte(`
@base
@type("OpenAI")
provider openai {
  auth {
    apiKey     env("TEST_KEY")
    projectId  env("TEST_PROJECT")
  }
}
`)
	cfg, err := parseProviderMemQL("test/_base.memql", raw)
	require.NoError(t, err)
	require.Equal(t, "openai", cfg.Name)
	require.Equal(t, "OpenAI", cfg.Type)
	require.True(t, cfg.Base)
	require.Empty(t, cfg.Model) // base providers don't need @model
	require.Equal(t, "${TEST_KEY}", cfg.Auth["apiKey"])
	require.Equal(t, "${TEST_PROJECT}", cfg.Auth["projectId"])
}

func TestParseProviderMemQL_ExtendsProvider(t *testing.T) {
	raw := []byte(`
@extends("openai")
@model("gpt-5-mini")
provider chat5Mini {
  params {
    maxCompletionTokens  4096
  }
}
`)
	cfg, err := parseProviderMemQL("test/chat5Mini.memql", raw)
	require.NoError(t, err)
	require.Equal(t, "chat5Mini", cfg.Name)
	require.Equal(t, "openai", cfg.Extends)
	require.Equal(t, "gpt-5-mini", cfg.Model)
	require.Empty(t, cfg.Type) // inherited from base
	require.Empty(t, cfg.Auth) // inherited from base
	require.Equal(t, 4096, cfg.Params["maxCompletionTokens"])
}

func TestParseProviderMemQL_ExtendsWithTypeOverride(t *testing.T) {
	raw := []byte(`
@extends("openai")
@type("OpenAIStream")
@model("gpt-5.2")
@default
provider stream5.2 {
  params {
    maxCompletionTokens  4096
  }
}
`)
	cfg, err := parseProviderMemQL("test/stream5.2.memql", raw)
	require.NoError(t, err)
	require.Equal(t, "stream5.2", cfg.Name)
	require.Equal(t, "openai", cfg.Extends)
	require.Equal(t, "OpenAIStream", cfg.Type) // explicitly overridden
	require.True(t, cfg.Default)
	require.Equal(t, "gpt-5.2", cfg.Model)
}

func TestParseProviderMemQL_ExtendsWithoutModel(t *testing.T) {
	// @extends without @model should fail (only base providers skip model).
	raw := []byte(`
@extends("openai")
provider badProvider {
  params {
    maxCompletionTokens  4096
  }
}
`)
	_, err := parseProviderMemQL("test/bad.memql", raw)
	require.Error(t, err)
	require.Contains(t, err.Error(), "@model is required")
}

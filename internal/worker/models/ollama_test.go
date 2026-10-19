package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestParseParameterSize. Ollama states a model's size the way a person
// reads it -- "8.0B" -- and the label carries a COUNT, because the engine
// ranks on it numerically (D5) and "70B" sorts before "8.0B" as text.
//
// EVERY UNREADABLE INPUT IS ABSENT, never zero-with-true. Absent sorts
// last in the engine's ranking; a zero it believed would be a claim that
// this model has no parameters, which sorts last too until somebody
// writes a comparison the other way round. Silence is the answer that
// stays right under a reader this side does not control.
func TestParseParameterSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"8.0B", 8000000000, true},
		{"70B", 70000000000, true},
		{"1.5B", 1500000000, true},
		{"135M", 135000000, true},
		{"268.10M", 268100000, true},
		{"1.5K", 1500, true},
		// Ollama spells the unit upper case; a runtime that does not is
		// still telling us the same fact.
		{"7.2b", 7200000000, true},
		{" 8.0B ", 8000000000, true},
		// Absent, each for its own reason: nothing said; a word rather
		// than a size; a unit this side does not know; a unit with no
		// number in front of it.
		{"", 0, false},
		{"unknown", 0, false},
		{"8.0X", 0, false},
		{"B", 0, false},
		// A bare number is REFUSED rather than guessed. "8" is either
		// eight parameters or eight billion depending on a convention the
		// string does not carry, and a wrong guess here does not fail --
		// it silently reorders the fleet.
		{"8000000000", 0, false},
		{"-8B", 0, false},
		{"0B", 0, false},
		// ParseFloat accepts these; converting either to int64 is
		// implementation-defined, and a garbage count would sort this
		// machine FIRST in a ranking that reads bigger as stronger.
		{"InfB", 0, false},
		{"NaNB", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseParameterSize(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseParameterSize(%q) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// ollamaTagsFixture is a recorded GET /api/tags body.
//
// The `details` block is the load-bearing part and the reason this test
// exists: parameter_size and quantization_level are returned HERE, by the
// call the probe already makes, so reading them costs no round trip and
// -- see TestProbeOllama_SizeSurvivesAFailedShow -- survives an /api/show
// that does not answer. /api/show's model_info carries a more exact
// general.parameter_count, and it is not worth having: params is an
// ordering signal, and precision bought by making the attribute depend on
// a second call that can fail is precision that costs the attribute.
//
// legacy:7b carries a details block with neither field, which is what an
// older Ollama and a hand-imported model both look like.
const ollamaTagsFixture = `{
  "models": [
    {
      "name": "llama3.1:8b",
      "model": "llama3.1:8b",
      "modified_at": "2026-08-14T09:12:44.101302595-07:00",
      "size": 4920753328,
      "digest": "42182419e9508c30c4b1fe55015f06b65f4ca4b9e28a744be55008d21998a093",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "llama",
        "families": ["llama"],
        "parameter_size": "8.0B",
        "quantization_level": "Q4_K_M"
      }
    },
    {
      "name": "nomic-embed-text:latest",
      "model": "nomic-embed-text:latest",
      "modified_at": "2026-08-02T18:31:02.884000110-07:00",
      "size": 274302450,
      "digest": "0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "nomic-bert",
        "families": ["nomic-bert"],
        "parameter_size": "137M",
        "quantization_level": "F16"
      }
    },
    {
      "name": "gemma3:270m",
      "model": "gemma3:270m",
      "modified_at": "2026-08-19T07:44:11.010112004-07:00",
      "size": 291736053,
      "digest": "e7d36fb2c3b31d0d2b1e0dd1b0f04ed3ba0e4ea0b47c3e4d5a6b7c8d9e0f1a2b",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "gemma3",
        "families": ["gemma3"],
        "parameter_size": "268.10M",
        "quantization_level": "Q8_0"
      }
    },
    {
      "name": "legacy:7b",
      "model": "legacy:7b",
      "modified_at": "2026-03-01T11:02:19.552000000-07:00",
      "size": 3826793677,
      "digest": "1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "llama",
        "families": ["llama"]
      }
    },
    {
      "name": "vanished:8b",
      "model": "vanished:8b",
      "modified_at": "2026-09-01T04:00:00.000000000-07:00",
      "size": 4661224676,
      "digest": "9f438cb9cd581fc025612d27f7c1a6669ff83a8bb0ed86c94fcf4c5440555697",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "qwen2",
        "families": ["qwen2"],
        "parameter_size": "7.6B",
        "quantization_level": "Q5_K_M"
      }
    }
  ]
}`

// ollamaFixtureShow is what /api/show answers per model. vanished:8b is
// absent on purpose: a real Ollama 404s for a model deleted between the
// two calls.
var ollamaFixtureShow = map[string]string{
	"llama3.1:8b":             `{"capabilities":["completion","tools"],"model_info":{"llama.context_length":131072}}`,
	"nomic-embed-text:latest": `{"capabilities":["embedding"],"model_info":{"nomic-bert.context_length":2048}}`,
	"gemma3:270m":             `{"capabilities":["completion"],"model_info":{"gemma3.context_length":32768}}`,
	"legacy:7b":               `{"capabilities":["completion"],"model_info":{"llama.context_length":4096}}`,
}

// ollamaFixtureStub serves the recorded bodies and counts the calls, so a
// test can say what discovery cost as well as what it learned.
func ollamaFixtureStub(t *testing.T) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var tagsHits, showHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tagsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ollamaTagsFixture))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&showHits, 1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, ok := ollamaFixtureShow[req.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &tagsHits, &showHits
}

// TestProbeOllama_SizeQuantAndTools drives the recorded fixture end to
// end: what /api/tags and /api/show say becomes what the machine tells
// the cluster.
func TestProbeOllama_SizeQuantAndTools(t *testing.T) {
	srv, tagsHits, showHits := ollamaFixtureStub(t)
	allow := []string{"llama3.1:8b", "nomic-embed-text:latest", "gemma3:270m", "legacy:7b", "vanished:8b"}
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{Allow: allow})

	byID := map[string]Info{}
	for _, m := range inv.Models {
		byID[m.ID] = m
	}

	tests := []struct {
		id    string
		attrs Attributes
		label string
	}{
		// tools is claimed and structured output is claimed WITH it, from
		// the same capability: Ollama has no structured-output capability
		// of its own, so `tools` is the proxy for both. Both or neither,
		// never one -- see ollamaStructuredOutput.
		{"llama3.1:8b",
			Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 1,
				Params: 8000000000, Quant: "Q4_K_M", Tools: true},
			"ctx=131072,structured=1,max=1,params=8000000000,quant=Q4_K_M,tools=1"},
		{"nomic-embed-text:latest",
			Attributes{ContextWindow: 2048, Embeddings: true, MaxConcurrent: 1,
				Params: 137000000, Quant: "F16"},
			"ctx=2048,embeddings=1,max=1,params=137000000,quant=F16"},
		// Size is not a capability: a small model states both and claims
		// neither tools nor structured output.
		{"gemma3:270m",
			Attributes{ContextWindow: 32768, MaxConcurrent: 1, Params: 268100000, Quant: "Q8_0"},
			"ctx=32768,max=1,params=268100000,quant=Q8_0"},
		// A details block without the two fields advertises neither,
		// rather than advertising a zero.
		{"legacy:7b",
			Attributes{ContextWindow: 4096, MaxConcurrent: 1},
			"ctx=4096,max=1"},
	}
	for _, tt := range tests {
		got, ok := byID[tt.id]
		if !ok {
			t.Errorf("%s is missing from the inventory", tt.id)
			continue
		}
		if got.Attributes != tt.attrs {
			t.Errorf("%s attributes = %+v, want %+v", tt.id, got.Attributes, tt.attrs)
		}
		if label := inv.Labels()[Label(tt.id)]; label != tt.label {
			t.Errorf("%s label = %q, want %q", tt.id, label, tt.label)
		}
	}

	// The two attributes ride the call the probe already made. A second
	// listing per model would be invisible on a laptop with three models
	// and would show up as a slow registration on a machine with thirty.
	if n := atomic.LoadInt32(tagsHits); n != 1 {
		t.Errorf("/api/tags was called %d times, want exactly 1", n)
	}
	if n := atomic.LoadInt32(showHits); n != 5 {
		t.Errorf("/api/show was called %d times, want one per model", n)
	}
}

// TestProbeOllama_SizeSurvivesAFailedShow is the reason the read is on
// /api/tags rather than /api/show.
//
// A model whose /api/show does not answer keeps every capability absent
// -- fail-closed, unchanged -- but it still states its SIZE, because that
// arrived with the listing. Reading the size from model_info instead
// would have made a model the router can still serve for free text into
// one that also refuses to say how big it is, and D5 sorts an unstated
// size last.
func TestProbeOllama_SizeSurvivesAFailedShow(t *testing.T) {
	srv, _, _ := ollamaFixtureStub(t)
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{Allow: []string{"vanished:8b"}})

	var got Info
	for _, m := range inv.Models {
		if m.ID == "vanished:8b" {
			got = m
		}
	}
	if got.ID == "" {
		t.Fatal("a model whose /api/show failed must still be reported")
	}
	if got.Params != 7600000000 || got.Quant != "Q5_K_M" {
		t.Errorf("size and quantization must survive a failed show: %+v", got.Attributes)
	}
	if got.ContextWindow != 0 || got.StructuredOutput || got.Embeddings || got.Tools {
		t.Errorf("an unreadable show must leave every capability absent: %+v", got.Attributes)
	}
	if want := "max=1,params=7600000000,quant=Q5_K_M"; inv.Labels()[Label("vanished:8b")] != want {
		t.Errorf("label = %q, want %q", inv.Labels()[Label("vanished:8b")], want)
	}
}

// TestOllamaStructuredOutput_ToolsIsTheProxy. Both or neither, and the
// pairing is the documented behaviour rather than an accident: Ollama has
// no structured-output capability, so a model that reports `tools`
// gets both claims and a model that does not gets neither. An operator
// who disagrees declares the model under an OpenAI-compatible runtime.
//
// And only for a model that generates text (memql-cockpit#551): Ollama
// 0.40.6 reports qwen3-embedding:0.6b as [tools thinking embedding], and
// an embedder claiming tools and structured output was sent chat calls.
func TestOllamaStructuredOutput_ToolsIsTheProxy(t *testing.T) {
	for _, tt := range []struct {
		caps []string
		want bool
	}{
		{[]string{"completion", "tools"}, true},
		{[]string{"COMPLETION", "TOOLS"}, true},
		{[]string{"completion", "vision", "tools", "thinking"}, true},
		{[]string{"TOOLS"}, false},
		{[]string{"tools", "thinking", "embedding"}, false},
		{[]string{"completion"}, false},
		{[]string{"embedding"}, false},
		{nil, false},
	} {
		if got := ollamaStructuredOutput(tt.caps); got != tt.want {
			t.Errorf("ollamaStructuredOutput(%v) = %t, want %t", tt.caps, got, tt.want)
		}
		if got := ollamaTools(tt.caps); got != tt.want {
			t.Errorf("tools from %v = %t, want %t -- the two must move together", tt.caps, got, tt.want)
		}
	}
}

// TestOllamaEmbedderClaimsNothingButEmbeddings is the probe as Ollama 0.40.6
// answered it for the two recommended models on a Mac (memql-cockpit#551).
func TestOllamaEmbedderClaimsNothingButEmbeddings(t *testing.T) {
	srv := modalityStub(t, map[string][]string{
		"qwen3.8:27b":          {"completion", "vision", "tools", "thinking"},
		"qwen3-embedding:0.6b": {"tools", "thinking", "embedding"},
	})
	got := probeAll(t, srv.URL)
	chat, embed := got["qwen3.8:27b"], got["qwen3-embedding:0.6b"]
	if !chat.Tools || !chat.StructuredOutput || !chat.Vision || chat.Embeddings {
		t.Errorf("the chat model must keep its claims: %+v", chat.Attributes)
	}
	if !embed.Embeddings || embed.Tools || embed.StructuredOutput || embed.Vision {
		t.Errorf("an embedder must claim embeddings and nothing a chat needs: %+v", embed.Attributes)
	}
}

// TestQuantLevel_UnknownIsAbsentNotAValue.
//
// Ollama answers "unknown" for a GGUF whose file type it does not
// recognise, which is every model pulled through hf.co on the machine
// this was written on. The distinction is not cosmetic: an absent quant
// is a fact this machine did not establish, while `quant=unknown` on the
// label is a claim that the level IS "unknown" -- and the engine reduces
// quantizations to a set across the fleet, so the word would sit in that
// set beside Q4_K_M as though somebody had chosen it.
func TestQuantLevel_UnknownIsAbsentNotAValue(t *testing.T) {
	for reported, want := range map[string]string{
		"unknown":  "",
		"UNKNOWN":  "",
		"Unknown":  "",
		" unknown": "",
		"":         "",
		"   ":      "",
		"Q4_K_M":   "Q4_K_M",
		" F16 ":    "F16",
		// Not a substring match: a real level that merely contains the
		// word keeps its value.
		"unknown-q4": "unknown-q4",
	} {
		if got := quantLevel(reported); got != want {
			t.Errorf("quantLevel(%q) = %q, want %q", reported, got, want)
		}
	}
}

package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func metFloor() FloorVerdict { return FloorVerdict{Met: true, Detail: "test"} }

// ollamaStub serves /api/tags and /api/show. show maps a model id to the
// JSON body /api/show returns for it; a model missing from the map gets a
// 404, which is how a real Ollama answers for a model that vanished
// between the two calls.
func ollamaStub(t *testing.T, tags []string, show map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var body struct {
			Models []map[string]string `json:"models"`
		}
		for _, id := range tags {
			body.Models = append(body.Models, map[string]string{"name": id, "model": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, ok := show[req.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func discovererFor(url string, env map[string]string) *Discoverer {
	return &Discoverer{
		OllamaBaseURL: url,
		FloorFn:       metFloor,
		Getenv:        func(k string) string { return env[k] },
	}
}

// TestProbeOllama_FullAttributes: the happy path, and the shape the
// engine will parse out of it.
func TestProbeOllama_FullAttributes(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"llama3.1:8b"}, map[string]string{
		"llama3.1:8b": `{"capabilities":["completion","tools"],"model_info":{"llama.context_length":131072}}`,
	})
	d := discovererFor(srv.URL, map[string]string{"OLLAMA_NUM_PARALLEL": "2"})

	inv := d.Probe(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
	got := inv.Models[0]
	if got.ID != "llama3.1:8b" || got.Kind != KindOllama || got.BaseURL != srv.URL {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.ContextWindow != 131072 || !got.StructuredOutput || got.Embeddings || got.MaxConcurrent != 2 {
		t.Errorf("attributes = %+v", got.Attributes)
	}
	if !got.Tools {
		t.Error("a model reporting the tools capability must advertise tools")
	}
	// This stub's /api/tags carries no `details` block, which is what an
	// older Ollama looks like. Size and quantization are then ABSENT
	// rather than zero -- ollama_test.go's recorded fixture is where they
	// are present.
	if got.Params != 0 || got.Quant != "" {
		t.Errorf("nothing in this listing states a size: %+v", got.Attributes)
	}
	if !got.Allowed {
		t.Error("an allowed model must be marked allowed")
	}
	if want := "ctx=131072,structured=1,max=2,tools=1"; inv.Labels()["model:llama3.1:8b"] != want {
		t.Errorf("label = %q, want %q", inv.Labels()["model:llama3.1:8b"], want)
	}
}

// TestProbeOllama_CapabilitiesFailClosed. A model that does not report
// tools is not claimed for structured output, and an embeddings model is
// claimed only for embeddings. The direction is the point: an
// over-claimed capability fails three layers away with a parse error that
// names nothing here.
func TestProbeOllama_CapabilitiesFailClosed(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"plain:7b", "nomic-embed-text", "mystery:1b"}, map[string]string{
		"plain:7b":         `{"capabilities":["completion"],"model_info":{"llama.context_length":8192}}`,
		"nomic-embed-text": `{"capabilities":["embedding"],"model_info":{"nomic-bert.context_length":2048}}`,
		// mystery is absent from show entirely: /api/show 404s.
	})
	d := discovererFor(srv.URL, nil)
	inv := d.Probe(context.Background(), Request{Allow: []string{"plain:7b", "nomic-embed-text", "mystery:1b"}})

	byID := map[string]Info{}
	for _, m := range inv.Models {
		byID[m.ID] = m
	}
	if got := byID["plain:7b"]; got.StructuredOutput || got.Tools {
		t.Error("a model without the tools capability must claim neither tools nor structured output")
	}
	if got := byID["nomic-embed-text"]; !got.Embeddings || got.StructuredOutput || got.Tools {
		t.Errorf("embeddings model = %+v", got.Attributes)
	}
	// A show that failed still leaves the model SERVABLE for free text --
	// it is installed, and dropping it would cost more than it saves.
	m, ok := byID["mystery:1b"]
	if !ok {
		t.Fatal("a model whose /api/show failed must still be reported")
	}
	if m.ContextWindow != 0 || m.StructuredOutput || m.Embeddings || m.Tools {
		t.Errorf("an unreadable show must leave every capability absent: %+v", m.Attributes)
	}
	if m.MaxConcurrent == 0 {
		t.Error("max concurrent must always be declared; absent means UNLIMITED to the engine")
	}
}

// TestProbeOllama_AbsentIsNotAnError. No Ollama is the ordinary case
// across most of a fleet.
func TestProbeOllama_AbsentIsNotAnError(t *testing.T) {
	// A port nothing is listening on.
	srv, _ := ollamaStub(t, nil, nil)
	dead := srv.URL
	srv.Close()

	d := discovererFor(dead, nil)
	inv := d.Probe(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 0 {
		t.Errorf("Models = %+v, want none", inv.Models)
	}
	if len(inv.ProbeNotes) != 1 || !strings.Contains(inv.ProbeNotes[0], "no Ollama at") {
		t.Errorf("ProbeNotes = %v, want one note naming the endpoint", inv.ProbeNotes)
	}
}

func TestProbeOllama_RunningButEmpty(t *testing.T) {
	srv, _ := ollamaStub(t, nil, nil)
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{})
	if len(inv.ProbeNotes) != 1 || !strings.Contains(inv.ProbeNotes[0], "no models pulled") {
		t.Errorf("ProbeNotes = %v", inv.ProbeNotes)
	}
}

// TestOllamaBaseURL. All three documented shapes of OLLAMA_HOST, because
// an operator who set it the way Ollama's own docs show must not have
// their machine silently offer nothing.
func TestOllamaBaseURL(t *testing.T) {
	tests := map[string]string{
		"":                       DefaultOllamaBaseURL,
		"127.0.0.1:11434":        "http://127.0.0.1:11434",
		"http://gpu-box:11434":   "http://gpu-box:11434",
		"gpu-box":                "http://gpu-box:11434",
		"https://gpu-box:11434/": "https://gpu-box:11434",
	}
	for host, want := range tests {
		d := &Discoverer{Getenv: func(k string) string {
			if k == "OLLAMA_HOST" {
				return host
			}
			return ""
		}}
		if got := d.ollamaBaseURL(); got != want {
			t.Errorf("OLLAMA_HOST=%q -> %q, want %q", host, got, want)
		}
	}
}

// TestOllamaParallelism_NeverZero. This is the one attribute whose
// absence is PERMISSIVE on the engine side, so it is never left absent.
func TestOllamaParallelism_NeverZero(t *testing.T) {
	for env, want := range map[string]int{"": 1, "0": 1, "-3": 1, "nonsense": 1, "4": 4} {
		d := &Discoverer{Getenv: func(k string) string {
			if k == "OLLAMA_NUM_PARALLEL" {
				return env
			}
			return ""
		}}
		if got := d.ollamaParallelism(); got != want {
			t.Errorf("OLLAMA_NUM_PARALLEL=%q -> %d, want %d", env, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Declared OpenAI-compatible runtimes
// -----------------------------------------------------------------------------

func openAIStub(t *testing.T, served []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data []map[string]string `json:"data"`
		}
		for _, id := range served {
			body.Data = append(body.Data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeDeclared_AttributesRideThrough(t *testing.T) {
	api := openAIStub(t, []string{"qwen2.5-7b-instruct"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	d := discovererFor(dead.URL, nil)
	inv := d.Probe(context.Background(), Request{
		Allow: []string{"qwen2.5-7b-instruct"},
		Runtimes: []DeclaredRuntime{{
			Name:    "lmstudio",
			BaseURL: api.URL,
			Models: []DeclaredModel{{
				ID: "qwen2.5-7b-instruct", ContextWindow: 32768, StructuredOutput: true, MaxConcurrent: 3,
				Params: 7620000000, Quant: " Q4_K_M ", Tools: true,
			}},
		}},
	})
	if len(inv.Models) != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
	got := inv.Models[0]
	if got.Kind != KindOpenAICompatible || got.Runtime != "lmstudio" || got.BaseURL != api.URL {
		t.Errorf("identity = %+v", got)
	}
	if got.ContextWindow != 32768 || !got.StructuredOutput || got.MaxConcurrent != 3 {
		t.Errorf("declared attributes did not ride through: %+v", got.Attributes)
	}
	// The three that arrived with this epic. They are asserted through
	// the LABEL rather than the struct because the struct is only half
	// the trip: probeDeclared can carry a field the renderer omits, and
	// the label is what the engine actually reads.
	//
	// Whitespace is in the fixture on purpose. An operator's policy.yaml
	// quant is hand-typed, and a stray space would render `quant= Q4_K_M`
	// -- which the engine's parser trims back to the same value, but
	// which makes the label bytes differ from an otherwise identical
	// machine's and rewrites the registration row on every reconnect.
	if got.Params != 7620000000 || got.Quant != "Q4_K_M" || !got.Tools {
		t.Errorf("declared params/quant/tools did not ride through: %+v", got.Attributes)
	}
	const wantLabel = "ctx=32768,structured=1,max=3,params=7620000000,quant=Q4_K_M,tools=1"
	if label := got.Attributes.String(); label != wantLabel {
		t.Errorf("label = %q, want %q", label, wantLabel)
	}
}

// TestProbeDeclared_ADeclarationDoesNotDeleteToolSupport.
//
// resolveDuplicates lets a declared entry win WHOLESALE over the Ollama
// probe, which is what makes declaring a model against Ollama's own /v1
// endpoint the documented way to overrule the capability heuristic. That
// escape hatch is only safe while a declaration can STATE tool support:
// if probeDeclared dropped the field, using the hatch to correct one
// attribute would silently remove another, and the model would stop being
// eligible for tool turns for a reason nothing on the machine reports.
func TestProbeDeclared_ADeclarationDoesNotDeleteToolSupport(t *testing.T) {
	api := openAIStub(t, []string{"llama3.1:8b"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow: []string{"llama3.1:8b"},
		Runtimes: []DeclaredRuntime{{
			Name:    "ollama-openai",
			BaseURL: api.URL,
			Models:  []DeclaredModel{{ID: "llama3.1:8b", StructuredOutput: true, Tools: true}},
		}},
	})
	if len(inv.Models) != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
	if !inv.Models[0].Tools {
		t.Errorf("a declaration that states tools must keep them: %+v", inv.Models[0].Attributes)
	}
}

// TestDeclaredModel_YAMLKeysAndFailClosedDefaults pins the policy.yaml
// spelling of what an operator states about a non-Ollama runtime.
//
// The declaration is the ONLY source of these for such a runtime: an
// OpenAI-compatible /v1/models returns ids and nothing else, so a size, a
// quantization level and tool support that are not written down cannot be
// observed at all. The keys are pinned because a typo in one is SILENT --
// yaml drops a key no field claims without a word, so `quantisation:` in
// a policy.yaml is a file the operator edited, a machine that advertises
// nothing new, and no message anywhere to connect the two.
//
// The three new keys are spelled as the LABEL spells them, deliberately:
// an operator comparing `quant=Q4_K_M` on the Fleet page against their
// policy.yaml reads one word in both places. context_window and
// structured_output are spelled long instead because their label keys
// (`ctx`, `structured`) are abbreviations nobody would guess from the
// file.
//
// Declaring a field is only half of it. probeDeclared (openai.go) is what
// turns a declaration into the advertised Attributes, so a field added
// here without a line there parses cleanly and is dropped on the floor --
// which looks exactly like an operator who mistyped the key.
func TestDeclaredModel_YAMLKeysAndFailClosedDefaults(t *testing.T) {
	const declaration = `
name: lmstudio
base_url: http://127.0.0.1:1234/v1
models:
  - id: qwen2.5-7b-instruct
    context_window: 32768
    structured_output: true
    max_concurrent: 2
    params: 7620000000
    quant: Q4_K_M
    tools: true
  - id: states-nothing
`
	var rt DeclaredRuntime
	if err := yaml.Unmarshal([]byte(declaration), &rt); err != nil {
		t.Fatalf("the declaration did not parse: %v", err)
	}
	if len(rt.Models) != 2 {
		t.Fatalf("models = %+v", rt.Models)
	}
	stated := rt.Models[0]
	if stated.Params != 7620000000 || stated.Quant != "Q4_K_M" || !stated.Tools {
		t.Errorf("declared params/quant/tools = %+v", stated)
	}
	if stated.ContextWindow != 32768 || !stated.StructuredOutput || stated.MaxConcurrent != 2 {
		t.Errorf("the existing keys must keep parsing: %+v", stated)
	}
	// Undeclared is ABSENT, never a zero this side made up. The engine
	// sorts a model that does not state its size last (D5); a params of 0
	// invented here would be this machine claiming a size it never read.
	if silent := rt.Models[1]; silent.Params != 0 || silent.Quant != "" || silent.Tools {
		t.Errorf("an operator who stated nothing must claim nothing: %+v", silent)
	}
}

// TestProbeDeclared_MaxConcurrentDefaultsToOne. Silence must not reach
// the engine as "unlimited".
func TestProbeDeclared_MaxConcurrentDefaultsToOne(t *testing.T) {
	api := openAIStub(t, []string{"m"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()
	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"m"},
		Runtimes: []DeclaredRuntime{{Name: "vllm", BaseURL: api.URL, Models: []DeclaredModel{{ID: "m"}}}},
	})
	if len(inv.Models) != 1 || inv.Models[0].MaxConcurrent != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
}

// TestProbeDeclared_DownEndpointOffersNothing. Advertising a model whose
// server is down means the router picks this machine, sends somebody's
// prompt, and the call fails at the last hop.
func TestProbeDeclared_DownEndpointOffersNothing(t *testing.T) {
	api := openAIStub(t, []string{"m"})
	url := api.URL
	api.Close()
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"m"},
		Runtimes: []DeclaredRuntime{{Name: "vllm", BaseURL: url, Models: []DeclaredModel{{ID: "m"}}}},
	})
	if len(inv.Models) != 0 {
		t.Fatalf("Models = %+v, want none", inv.Models)
	}
	if !strings.Contains(strings.Join(inv.ProbeNotes, " "), "did not answer") {
		t.Errorf("ProbeNotes = %v", inv.ProbeNotes)
	}
}

// TestProbeDeclared_NotCurrentlyServed. A model the operator declared but
// the endpoint is not serving right now is held back, and SAID so.
func TestProbeDeclared_NotCurrentlyServed(t *testing.T) {
	api := openAIStub(t, []string{"loaded"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow: []string{"loaded", "unloaded"},
		Runtimes: []DeclaredRuntime{{Name: "lmstudio", BaseURL: api.URL, Models: []DeclaredModel{
			{ID: "loaded"}, {ID: "unloaded"},
		}}},
	})
	if len(inv.Models) != 1 || inv.Models[0].ID != "loaded" {
		t.Fatalf("Models = %+v", inv.Models)
	}
	if !strings.Contains(strings.Join(inv.ProbeNotes, " "), "unloaded") {
		t.Errorf("ProbeNotes must name what was held back: %v", inv.ProbeNotes)
	}
}

// TestDiscover_BelowFloorProbesNothing. Not merely "offers nothing" --
// it must not reach for a runtime socket at all. This runs on every
// registration.
func TestDiscover_BelowFloorProbesNothing(t *testing.T) {
	srv, hits := ollamaStub(t, []string{"llama3.1:8b"}, map[string]string{
		"llama3.1:8b": `{"capabilities":["tools"],"model_info":{"llama.context_length":8192}}`,
	})
	d := &Discoverer{
		OllamaBaseURL: srv.URL,
		FloorFn:       func() FloorVerdict { return FloorVerdict{Reason: "below the floor"} },
	}
	inv := d.Discover(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 0 || len(inv.Advertised()) != 0 {
		t.Fatalf("below the floor a machine offers nothing: %+v", inv.Models)
	}
	if n := *hits; n != 0 {
		t.Errorf("the runtime was probed %d times below the floor; want 0", n)
	}
	if inv.Floor.Reason == "" {
		t.Error("the verdict must carry the reason")
	}
}

// TestProbe_StableOrder. An unstable order rewrites the registration row
// on every reconnect for no actual change.
func TestProbe_StableOrder(t *testing.T) {
	ids := []string{"zeta:1b", "alpha:1b", "mid:1b"}
	show := map[string]string{}
	for _, id := range ids {
		show[id] = fmt.Sprintf(`{"capabilities":["tools"],"model_info":{"x.context_length":%d}}`, 4096)
	}
	srv, _ := ollamaStub(t, ids, show)
	d := discovererFor(srv.URL, nil)

	var first []string
	for i := 0; i < 3; i++ {
		inv := d.Probe(context.Background(), Request{Allow: ids})
		var got []string
		for _, m := range inv.Models {
			got = append(got, m.ID)
		}
		if i == 0 {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed between probes: %v then %v", first, got)
		}
	}
	if strings.Join(first, ",") != "alpha:1b,mid:1b,zeta:1b" {
		t.Errorf("order = %v, want sorted by id", first)
	}
}

// TestProbe_DuplicateIdAcrossRuntimes.
//
// A model id is a label KEY. Two runtimes offering the same id cannot both
// be advertised -- the second overwrites the first's attributes, and Find
// would hand the call to whichever sorted first, so the machine would tell
// the cluster one context window and serve from a runtime with another.
//
// The DECLARED entry wins, and that direction is what makes the documented
// escape hatch work: declaring a model against Ollama's own /v1 surface
// with structured_output: true is how an operator overrules the `tools`
// heuristic, and an auto-discovered entry shadowing it would leave them
// editing a file that changes nothing.
func TestProbe_DuplicateIdAcrossRuntimes(t *testing.T) {
	ollama, _ := ollamaStub(t, []string{"llama3.1:8b"}, map[string]string{
		// The probe finds no tools capability, so it claims no
		// structured output -- exactly the case the escape hatch exists
		// for.
		"llama3.1:8b": `{"capabilities":["completion"],"model_info":{"llama.context_length":8192}}`,
	})
	declared := openAIStub(t, []string{"llama3.1:8b"})

	inv := discovererFor(ollama.URL, nil).Probe(context.Background(), Request{
		Allow: []string{"llama3.1:8b"},
		Runtimes: []DeclaredRuntime{{
			Name:    "ollama-openai",
			BaseURL: declared.URL,
			Models:  []DeclaredModel{{ID: "llama3.1:8b", ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 1}},
		}},
	})

	if len(inv.Models) != 1 {
		t.Fatalf("a model id must be reported once, got %+v", inv.Models)
	}
	got := inv.Models[0]
	if got.Kind != KindOpenAICompatible || got.Runtime != "ollama-openai" {
		t.Fatalf("the declared entry must win: %+v", got)
	}
	if !got.StructuredOutput || got.ContextWindow != 131072 {
		t.Errorf("the operator's stated attributes must survive: %+v", got.Attributes)
	}

	// What is advertised and what would serve the call must be the same
	// entry -- that is the whole point.
	if want := "ctx=131072,structured=1,max=1"; inv.Labels()["model:llama3.1:8b"] != want {
		t.Errorf("label = %q, want %q", inv.Labels()["model:llama3.1:8b"], want)
	}
	served, ok := inv.Find("llama3.1:8b")
	if !ok || served.Runtime != got.Runtime {
		t.Errorf("Find resolved %+v, which is not the entry the label describes", served)
	}
	if !strings.Contains(strings.Join(inv.ProbeNotes, " "), "offered by both") {
		t.Errorf("the shadowed entry must be reported, not silently dropped: %v", inv.ProbeNotes)
	}
}

// TestResolvedOllamaBaseURL_IsTheSameAnswerTheProbeUses.
//
// The pull path in internal/worker/inference asks this instead of reading
// OLLAMA_HOST for itself, so the two must be one answer. A second reading
// that drifted would put a pulled model somewhere the discoverer never
// looks -- and the operator would watch a pull succeed and the model
// never appear in the fleet, with nothing anywhere connecting the two.
func TestResolvedOllamaBaseURL_IsTheSameAnswerTheProbeUses(t *testing.T) {
	cases := map[string]string{
		"":                       DefaultOllamaBaseURL,
		"127.0.0.1:9999":         "http://127.0.0.1:9999",
		"http://box.local:11434": "http://box.local:11434",
		"box.local":              "http://box.local:11434",
	}
	for host, want := range cases {
		d := &Discoverer{Getenv: func(k string) string {
			if k == "OLLAMA_HOST" {
				return host
			}
			return ""
		}}
		if got := d.ResolvedOllamaBaseURL(); got != want {
			t.Errorf("OLLAMA_HOST=%q -> %q, want %q", host, got, want)
		}
		if got, internal := d.ResolvedOllamaBaseURL(), d.ollamaBaseURL(); got != internal {
			t.Errorf("the exported answer %q differs from the probe's %q", got, internal)
		}
	}
	// The explicit override still wins, which is what tests set.
	d := &Discoverer{OllamaBaseURL: "http://elsewhere:1234/"}
	if got := d.ResolvedOllamaBaseURL(); got != "http://elsewhere:1234" {
		t.Errorf("override -> %q", got)
	}
}

func TestDeclaredAudioRuntimeReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, response string
		want                     int
	}{
		{"empty OpenAI listing", "openai", `{"data":[]}`, 0},
		{"loaded Whisper", "whisper-cpp", `{"status":"ok"}`, 1},
		{"loading Whisper", "whisper-cpp", `{"status":"loading model"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := "/models"
				if tc.protocol == "whisper-cpp" {
					path = "/health"
				}
				if r.URL.Path != path {
					t.Errorf("probe path %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			d := discovererFor(server.URL, nil)
			rows, _ := d.probeDeclared(context.Background(), DeclaredRuntime{Name: "asr", BaseURL: server.URL, Transcription: tc.protocol, Models: []DeclaredModel{{ID: "whisper-base.en", AudioIn: true}}})
			if len(rows) != tc.want {
				t.Fatalf("offered %d, expected %d", len(rows), tc.want)
			}
		})
	}
}

package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modalityStub serves an Ollama with an exact capability list per model,
// so a test says precisely what the runtime claimed and reads back
// precisely what the machine advertises.
func modalityStub(t *testing.T, capabilities map[string][]string) *httptest.Server {
	t.Helper()
	type tag struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Details struct {
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		var body struct {
			Models []tag `json:"models"`
		}
		for id := range capabilities {
			body.Models = append(body.Models, tag{Name: id, Model: id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		caps, ok := capabilities[req.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// deadOllama points the native probe at a closed port. Discovery
// treats a runtime that does not answer as absent, which is what a test
// about DECLARED runtimes needs -- otherwise it reads whatever Ollama
// the machine running the tests happens to have installed.
func deadOllama(k string) string {
	if k == "OLLAMA_HOST" {
		return "http://127.0.0.1:1"
	}
	return ""
}

func probeAll(t *testing.T, base string) map[string]Info {
	t.Helper()
	d := &Discoverer{Getenv: func(k string) string {
		if k == "OLLAMA_HOST" {
			return base
		}
		return ""
	}}
	inv := d.Probe(context.Background(), Request{})
	out := map[string]Info{}
	for _, m := range inv.Models {
		out[m.ID] = m
	}
	return out
}

// Ollama ANSWERS the vision question: `vision` is a capability in
// /api/show, beside `tools` and `embedding`. So this one flag is a real
// probe, and it is claimed only when the runtime says so.
func TestOllamaProbeClaimsVisionFromTheCapabilityList(t *testing.T) {
	srv := modalityStub(t, map[string][]string{
		"seeing:9b": {"completion", "tools", "vision"},
		"blind:9b":  {"completion", "tools"},
	})
	got := probeAll(t, srv.URL)

	if !got["seeing:9b"].Vision {
		t.Error("a model whose runtime reports vision must advertise it")
	}
	if got["blind:9b"].Vision {
		t.Error("vision was claimed for a model whose runtime never said so")
	}
}

// Image generation the same way, and from the capability list ONLY.
// Which platforms the vendor offers it on is a fact this cockpit cannot
// verify and would be stale within a release; a capability the runtime
// just printed is a fact about the machine in front of us.
func TestOllamaProbeClaimsImageGenFromTheCapabilityList(t *testing.T) {
	srv := modalityStub(t, map[string][]string{
		"x/z-image-turbo": {"completion", "image"},
		"qwen3.5:9b":      {"completion", "tools"},
	})
	got := probeAll(t, srv.URL)

	if !got["x/z-image-turbo"].ImageGen {
		t.Error("a model whose runtime reports image generation must advertise it")
	}
	if got["qwen3.5:9b"].ImageGen {
		t.Error("image generation was claimed for a model whose runtime never said so")
	}
}

// THE OTHER TWO ARE NEVER GUESSED. Ollama reports no capability for
// audio in or audio out, so a bare Ollama offers neither -- and the
// failure this guards against is inferring one from a model's NAME. A
// "kokoro" in an id is not a runtime that answered, and a machine
// advertising audioout on that basis takes a speech call it cannot
// serve, with the failure landing on somebody else's prompt.
func TestOllamaProbeNeverGuessesAModalityFromAName(t *testing.T) {
	srv := modalityStub(t, map[string][]string{
		"kokoro-82m":              {"completion"},
		"whisper-large-v3-turbo":  {"completion"},
		"x/flux2-klein:4b":        {"completion"},
		"qwen3-omni-30b-instruct": {"completion", "tools"},
		"parakeet-tdt-1.1b":       {"completion"},
	})
	got := probeAll(t, srv.URL)

	for id, m := range got {
		if m.AudioIn {
			t.Errorf("%s: audio in was claimed with no capability reporting it", id)
		}
		if m.AudioOut {
			t.Errorf("%s: audio out was claimed with no capability reporting it", id)
		}
		if m.ImageGen {
			t.Errorf("%s: image generation was claimed from the model's name", id)
		}
		if m.Vision {
			t.Errorf("%s: vision was claimed from the model's name", id)
		}
	}
}

// A /api/show that FAILS leaves every modality absent, the same way it
// already leaves every capability absent. The model is still installed
// and still servable for free text.
func TestOllamaProbeLeavesModalitiesAbsentWhenShowFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"m:9b","model":"m:9b"}]}`))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := probeAll(t, srv.URL)["m:9b"]
	if m.ID != "m:9b" {
		t.Fatalf("the model was dropped entirely: %+v", m)
	}
	if m.Vision || m.AudioIn || m.AudioOut || m.ImageGen {
		t.Fatalf("a modality survived a failed /api/show: %+v", m.Attributes)
	}
}

// A DECLARED runtime's operator states all four, and every one of them
// reaches the label.
//
// This is the same silent-failure class PR #392 found for
// params/quant/tools: yaml accepts the key, the operator sees no error,
// and the attribute never reaches a label -- which looks exactly like a
// typo they cannot find. It matters MORE here, because three of these
// four have no probe anywhere, so a declaration dropped on the floor is
// a modality that can never be advertised by any route at all.
func TestDeclaredRuntimeCarriesEveryModality(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"kokoro-82m"},{"id":"whisper-large-v3"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// OLLAMA_HOST is pointed at a closed port, so the declared runtime
	// is the ONLY contributor. Without it the probe finds whatever
	// Ollama the machine running the tests happens to have, and this
	// test passes or fails on the CI runner's own installation.
	d := &Discoverer{Getenv: deadOllama}
	inv := d.Probe(context.Background(), Request{
		Runtimes: []DeclaredRuntime{{
			Name:    "local-speech",
			BaseURL: srv.URL + "/v1",
			Models: []DeclaredModel{
				{ID: "kokoro-82m", AudioOut: true},
				{ID: "whisper-large-v3", AudioIn: true},
			},
		}},
	})

	got := map[string]Info{}
	for _, m := range inv.Models {
		got[m.ID] = m
	}
	if !got["kokoro-82m"].AudioOut {
		t.Errorf("a declared audio_out was dropped: %+v", got["kokoro-82m"].Attributes)
	}
	if got["kokoro-82m"].AudioIn {
		t.Errorf("a modality nobody declared was claimed: %+v", got["kokoro-82m"].Attributes)
	}
	if !got["whisper-large-v3"].AudioIn {
		t.Errorf("a declared audio_in was dropped: %+v", got["whisper-large-v3"].Attributes)
	}
}

// And the declared vision / image_gen keys, which an operator may use to
// overrule the Ollama probe through the documented escape hatch.
func TestDeclaredRuntimeCarriesVisionAndImageGen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"seeing:9b"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// OLLAMA_HOST is pointed at a closed port, so the declared runtime
	// is the ONLY contributor. Without it the probe finds whatever
	// Ollama the machine running the tests happens to have, and this
	// test passes or fails on the CI runner's own installation.
	d := &Discoverer{Getenv: deadOllama}
	inv := d.Probe(context.Background(), Request{
		Runtimes: []DeclaredRuntime{{
			Name:    "lmstudio",
			BaseURL: srv.URL + "/v1",
			Models:  []DeclaredModel{{ID: "seeing:9b", Vision: true, ImageGen: true}},
		}},
	})
	if len(inv.Models) != 1 {
		t.Fatalf("got %d models", len(inv.Models))
	}
	if !inv.Models[0].Vision || !inv.Models[0].ImageGen {
		t.Fatalf("declared vision / image_gen were dropped: %+v", inv.Models[0].Attributes)
	}
}

// -----------------------------------------------------------------------------
// Runtime labels carry a version (memql-cockpit#400, record D7)
// -----------------------------------------------------------------------------

// The label VALUE becomes the version. It was empty before, so this
// changes every machine's label fingerprint once on rollout -- the cost
// record D8 accepts, and the same one params and quant already charged.
func TestRuntimeLabelCarriesTheVersion(t *testing.T) {
	inv := Inventory{
		Floor:           FloorVerdict{Met: true},
		Models:          []Info{{ID: "m:9b", Kind: KindOllama, Allowed: true}},
		RuntimeVersions: map[string]string{KindOllama: "0.13.0"},
	}
	if got := inv.Labels()["runtime:ollama"]; got != "0.13.0" {
		t.Fatalf("runtime:ollama = %q, want 0.13.0", got)
	}
}

// A runtime present with NO readable version keeps an empty value and
// keeps its label. Dropping the label because a version could not be
// read would hide a runtime that is running.
func TestRuntimeLabelSurvivesAnUnreadableVersion(t *testing.T) {
	inv := Inventory{
		Floor:  FloorVerdict{Met: true},
		Models: []Info{{ID: "m:9b", Kind: KindOllama, Allowed: true}},
	}
	labels := inv.Labels()
	got, present := labels["runtime:ollama"]
	if !present {
		t.Fatal("the runtime label was dropped because no version was read")
	}
	if got != "" {
		t.Fatalf("runtime:ollama = %q, want empty", got)
	}
}

// The version moves the fingerprint, which is the reconnect this
// change costs. Asserted so it is a thing somebody decided rather than
// discovered on deploy.
func TestTheRuntimeVersionChangesTheLabels(t *testing.T) {
	base := Inventory{
		Floor:  FloorVerdict{Met: true},
		Models: []Info{{ID: "m:9b", Kind: KindOllama, Allowed: true}},
	}
	versioned := base
	versioned.RuntimeVersions = map[string]string{KindOllama: "0.13.0"}

	if base.Labels()["runtime:ollama"] == versioned.Labels()["runtime:ollama"] {
		t.Fatal("adding a version must change the label")
	}
}

// A version recorded for a runtime that advertises NO model does not
// appear: Labels() is built from the advertised set, so a runtime
// nothing is served through contributes no label to carry a version on.
func TestRuntimeVersionForAnUnadvertisedRuntimeIsNotEmitted(t *testing.T) {
	inv := Inventory{
		Floor:           FloorVerdict{Met: true},
		Models:          []Info{{ID: "m:9b", Kind: KindOllama, Allowed: true}},
		RuntimeVersions: map[string]string{KindOllama: "0.13.0", "kokoro": "0.2.4"},
	}
	if _, present := inv.Labels()["runtime:kokoro"]; present {
		t.Fatal("a runtime with no advertised model produced a label")
	}
}

package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// The Ollama probe (spec D1: the one runtime discovered natively).
//
// Two calls, and both are needed, because they answer different
// questions. /api/tags says what is installed and what SHAPE each one is
// -- its size and its quantization, in the `details` block; /api/show is
// where the context length and the CAPABILITY list live. Advertising from
// tags alone would claim a context window this side never read.
//
// The split matters in one direction: a machine whose /api/show does not
// answer still states its sizes, because those arrived with the listing.
// A successful /api/show refines the total count and supplies tensor shapes
// for mixture active-parameter accounting; failure keeps the listing's total.

// ollamaTagsResponse is the shape of GET /api/tags.
type ollamaTagsResponse struct {
	Models []struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Details struct {
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

// ollamaShowResponse is the shape of POST /api/show.
//
// model_info is a free-form map whose keys are namespaced by architecture
// ("llama.context_length", "qwen2.context_length", ...), so the context
// length is found by suffix rather than by a key this code could enumerate
// -- a new architecture must not silently lose its context window.
type ollamaShowResponse struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
	Tensors      []ollamaTensor `json:"tensors"`
}

// probeOllama returns the models Ollama has, and a note when it has
// nothing to say.
//
// A REFUSED CONNECTION IS NOT AN ERROR. No Ollama on this machine is the
// ordinary case across most of a fleet, and it means "this machine offers
// no models" -- a fact the worker reports, not a condition it logs at warn
// level on every registration.
func (d *Discoverer) probeOllama(ctx context.Context, req Request) ([]Info, string) {
	base := d.ollamaBaseURL()

	var tags ollamaTagsResponse
	if err := d.getJSON(ctx, base+"/api/tags", "", &tags); err != nil {
		return nil, fmt.Sprintf("no Ollama at %s (%v)", base, err)
	}
	if len(tags.Models) == 0 {
		return nil, fmt.Sprintf("Ollama is running at %s but has no models pulled", base)
	}

	parallel, parallelFrom := d.ParallelSource(req.Parallel)
	resident := residentByID(req.Resident)
	out := make([]Info, 0, len(tags.Models))
	for _, m := range tags.Models {
		id := strings.TrimSpace(m.Model)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		info := Info{
			ID:            id,
			Kind:          KindOllama,
			Runtime:       KindOllama,
			BaseURL:       base,
			CeilingSource: parallelFrom,
			Attributes: Attributes{
				MaxConcurrent: parallel,
				// Otherwise verbatim: quantLevel folds only Ollama's own
				// "I could not tell" answer to absent, and the label
				// renderer (quantSafe) is still the single gate on what
				// characters a level may contain. Everything in between
				// reaches `memql worker models` as the odd string the
				// runtime actually reported, which is what an operator
				// compares against `ollama list`.
				Quant: quantLevel(m.Details.QuantizationLevel),
			},
		}
		if n, ok := parseParameterSize(m.Details.ParameterSize); ok {
			info.Params = n
		}
		// A /api/show that fails leaves every CAPABILITY absent, which
		// costs this model eligibility for structured, tool and embedding
		// prompts and nothing else. That is the right trade: the model is
		// still installed and still servable for free text, and it still
		// states the size and quantization that came back with the
		// listing.
		var show ollamaShowResponse
		if err := d.postJSON(ctx, base+"/api/show", map[string]string{"model": id}, &show); err == nil {
			if total := ollamaParameterCount(show.ModelInfo["general.parameter_count"]); total > 0 {
				info.Params = total
			}
			info.ActiveParams = ollamaActiveParams(show)
			info.ContextWindow = ollamaContextLength(show.ModelInfo)
			info.Embeddings = hasCapability(show.Capabilities, "embedding")
			info.Tools = hasCapability(show.Capabilities, "tools")
			info.StructuredOutput = ollamaStructuredOutput(show.Capabilities)
			// Vision is the ONE modality Ollama answers directly:
			// `vision` is a capability in /api/show, beside `tools` and
			// `embedding`. So this flag is a real probe, and it is the
			// only one of the four that can be true on a bare Ollama.
			info.Vision = hasCapability(show.Capabilities, "vision")
			// And image generation, IF this runtime reports it. Read
			// from the capability list rather than from anything about
			// the model's name or the host's platform: which platforms
			// Ollama offers image generation on is a vendor fact this
			// cockpit cannot verify and would be stale within a
			// release, where a capability the runtime just printed is a
			// fact about the machine in front of us.
			info.ImageGen = hasCapability(show.Capabilities, "image")
			//
			// AUDIO IN AND AUDIO OUT ARE DELIBERATELY NOT SET HERE.
			// Ollama reports no capability for either, and there is no
			// honest probe short of sending a real audio payload to
			// every model on the machine. An operator with a
			// transcription endpoint or a Kokoro speech runtime
			// declares it under models.runtimes; a bare Ollama offers
			// neither, which is true.
		}
		// AFTER the probe, because the window is capped by the context the
		// probe read. Settings ride the model, never the label.
		applyResident(&info, resident[id])
		out = append(out, info)
	}
	return out, ""
}

// ollamaStructuredOutput decides whether to claim schema-honouring output.
//
// Ollama has no "structured output" capability of its own; it accepts a
// `format` schema for any model and how well the model HOLDS to it is a
// property of the model. `tools` is the closest honest proxy -- a model
// trained to emit a constrained tool call is a model trained to emit
// constrained JSON -- and it is the one that covers the operational class
// the spec names (llama3.1:8b, qwen2.5:7b both report it).
//
// It is deliberately conservative, per the package's fail-closed rule. An
// operator who disagrees about a specific model has a stated escape
// hatch rather than an argument with this heuristic: declare it under an
// OpenAI-compatible runtime pointed at Ollama's own /v1 surface, with
// structured_output: true. Their claim, their machine. That declaration
// now has to state tools: true as well if they want tool turns too --
// a declared entry WINS over this probe (resolveDuplicates), so an
// unstated attribute there is not inherited from here, it is lost.
//
// The same capability also answers Attributes.Tools directly, and the two
// reads are not a redundancy to collapse. Here they are one bit read
// twice because Ollama offers one capability that speaks to both; on a
// declared runtime they are two independent claims by the operator. A
// single field would make the engine's tool-turn gate (D11) and its
// schema gate the same question, which no source actually answers.
func ollamaStructuredOutput(capabilities []string) bool {
	return hasCapability(capabilities, "tools")
}

func hasCapability(list []string, want string) bool {
	for _, c := range list {
		if strings.EqualFold(strings.TrimSpace(c), want) {
			return true
		}
	}
	return false
}

// ollamaContextLength finds the architecture-namespaced context length.
//
// JSON numbers arrive as float64; a context window is an integer count of
// tokens, and a value that does not survive the round trip is treated as
// absent rather than truncated.
func ollamaContextLength(info map[string]any) int {
	for k, v := range info {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n > 0 && float64(int(n)) == n {
				return int(n)
			}
		case json.Number:
			if i, err := n.Int64(); err == nil && i > 0 {
				return int(i)
			}
		}
	}
	return 0
}

// parseParameterSize turns Ollama's human size ("8.0B", "135M") into a
// parameter count.
//
// The label carries the COUNT because the engine ranks on it numerically
// (D5: strongest first is parameters descending). Passed through as text,
// "70B" would sort before "8.0B" and the fleet's biggest model would be
// picked last, with nothing anywhere reporting a fault.
//
// EVERYTHING IT CANNOT READ IS ABSENT -- never zero with a true beside
// it, which is the whole reason it returns a second value. Absent is a
// model that did not state its size, and D5 sorts that last; a zero this
// side invented is a claim, and the next person to write a comparison has
// no way to tell the two apart. A bare number with no unit is refused for
// the same reason: "8" is eight parameters or eight billion depending on
// a convention the string does not carry, and a wrong guess does not
// fail, it silently reorders the fleet.
//
// The guards on the parsed float are not defensive noise. ParseFloat
// accepts "Inf" and "NaN", and converting either to int64 is
// implementation-defined -- a garbage count sorts this machine FIRST in a
// ranking that reads bigger as stronger. maxParams keeps the
// multiplication inside int64 while sitting three orders of magnitude
// above any model that exists.
func parseParameterSize(s string) (int64, bool) {
	const maxParams = 1e15

	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, false
	}
	var mult float64
	switch s[len(s)-1] {
	case 'B', 'b':
		mult = 1e9
	case 'M', 'm':
		mult = 1e6
	case 'K', 'k':
		mult = 1e3
	default:
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0, false
	}
	count := math.Round(n * mult)
	if count < 1 || count > maxParams {
		return 0, false
	}
	return int64(count), true
}

// -----------------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------------

func (d *Discoverer) getJSON(ctx context.Context, url, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return d.do(req, out)
}

func (d *Discoverer) postJSON(ctx context.Context, url string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.do(req, out)
}

func (d *Discoverer) do(req *http.Request, out any) error {
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// quantLevel reads a quantization level out of what Ollama reported, and
// treats the literal string "unknown" as ABSENT.
//
// Ollama fills `details.quantization_level` with "unknown" for models
// whose GGUF does not carry a file type it recognises -- every model
// pulled from Hugging Face through `hf.co/<owner>/<repo>` on the machine
// this was found on, which is one of the two sources this epic exists to
// support. Passing it through renders `quant=unknown` on the label, and
// that is not a missing value, it is a POSITIVE CLAIM that the model is
// quantized at a level named "unknown": the engine's fleet projection
// reduces quantizations to a set across machines and would carry the
// word as a member, and an operator reading the Fleet page cannot tell it
// from a level somebody chose.
//
// So it goes back to absent, which is what the rest of this package does
// with every fact a probe could not establish -- and `memql worker
// models` then says "quantization not advertised", which is true and
// actionable, instead of "unknown", which reads like an answer.
func quantLevel(reported string) string {
	q := strings.TrimSpace(reported)
	if strings.EqualFold(q, "unknown") {
		return ""
	}
	return q
}

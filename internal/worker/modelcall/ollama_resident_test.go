package modelcall

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// ollama_resident_test.go pins what this machine tells its own Ollama
// about keeping a model loaded.
//
// Ollama applies the keep_alive a request states, up or down, and what a
// request that omits it does depends on the release (0.34.2 leaves the
// expiry alone; a model whose FIRST request omitted it loads with Ollama's
// five minutes). One route that leaves the field out -- an embedding, a
// vision turn, an image -- can therefore start a model the owner asked to
// keep for two hours at five minutes, and nothing anywhere says so.

var (
	foreverKeepAlive = models.KeepAlive{Set: true, Forever: true}
	twoHourKeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
)

const (
	ollamaEmbedFrame = `{"model":"m","embeddings":[[0.1,0.2]],"prompt_eval_count":3}`
	ollamaImageFrame = `{"model":"m","images":["aGk="]}`
)

// sentKeepAlive decodes the keep_alive a request carried: nil when the
// key was never sent. The raw message is kept so the test can tell the
// JSON number -1 from the string "-1", which Ollama refuses.
func sentKeepAlive(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var sent struct {
		KeepAlive json.RawMessage `json:"keep_alive"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return sent.KeepAlive
}

func sentNumCtx(t *testing.T, body []byte) int64 {
	t.Helper()
	var sent struct {
		Options struct {
			NumCtx int64 `json:"num_ctx"`
		} `json:"options"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return sent.Options.NumCtx
}

// TestOllamaKeepAlive_EveryGenerationRouteCarriesIt. Chat, vision (a chat
// turn), embeddings and image generation are the four routes this client
// sends a model work on; a fifth added later has to be added here.
func TestOllamaKeepAlive_EveryGenerationRouteCarriesIt(t *testing.T) {
	routes := []struct {
		name  string
		reply string
		call  func(c *ollamaClient) error
	}{
		{"chat", ollamaDoneFrame, func(c *ollamaClient) error {
			_, err := c.Chat(context.Background(), ChatRequest{Model: "m"}, discardEmit)
			return err
		}},
		{"vision", ollamaDoneFrame, func(c *ollamaClient) error {
			_, err := c.Vision(context.Background(), VisionRequest{
				Model:    "m",
				Messages: []Message{{Role: "user", Content: "what is this"}},
				Images:   []ImagePart{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
			}, discardEmit)
			return err
		}},
		{"embedding", ollamaEmbedFrame, func(c *ollamaClient) error {
			_, err := c.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"hi"}})
			return err
		}},
		{"image", ollamaImageFrame, func(c *ollamaClient) error {
			_, err := c.GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "a cube"})
			return err
		}},
	}
	for _, ka := range []struct {
		name string
		set  models.KeepAlive
		want string
	}{
		// Forever is the JSON NUMBER -1. The string "-1" has no unit and
		// Ollama's duration parser refuses it.
		{"forever", foreverKeepAlive, `-1`},
		{"a duration", twoHourKeepAlive, `"2h0m0s"`},
	} {
		for _, r := range routes {
			t.Run(ka.name+"/"+r.name, func(t *testing.T) {
				srv, body := serveRaw(t, r.reply)
				c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), keepAlive: ka.set}
				if err := r.call(c); err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
				if got := string(sentKeepAlive(t, *body)); got != ka.want {
					t.Errorf("%s sent keep_alive %s, want %s", r.name, got, ka.want)
				}
			})
		}
	}
}

// TestOllamaKeepAlive_UnsetSendsNothing. A machine whose owner wrote no
// entry behaves exactly as it did before this feature: the key is absent
// and Ollama's own default stands.
func TestOllamaKeepAlive_UnsetSendsNothing(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: 8192}}, discardEmit); err != nil {
		t.Fatal(err)
	}
	if got := sentKeepAlive(t, *body); got != nil {
		t.Errorf("keep_alive = %s with nothing configured, want the key absent", got)
	}
	if got := sentNumCtx(t, *body); got != 8192 {
		t.Errorf("num_ctx = %d, want the engine's 8192 untouched", got)
	}
}

// TestOllamaWorkingContext. Ollama reloads a model whenever the requested
// num_ctx differs from the loaded one, and the engine sends the next power
// of two above each call: measured on this fleet, 107 of 170 reloads in
// thirty days were a bucket change, not an idle timeout. The window makes
// every call that fits run at the same num_ctx.
func TestOllamaWorkingContext(t *testing.T) {
	const window = 16384
	tests := []struct {
		name      string
		requested int64
		want      int64
	}{
		{"a small call runs at the window", 8192, window},
		{"a call at the window runs at it", window, window},
		// Review focus: a call that needs MORE than the window runs at
		// what it asked for. Clamping it down would truncate a prompt.
		{"a larger call is never clamped down", 32768, 32768},
		{"a much larger call is never clamped down", 131072, 131072},
		// The engine sent no window: the runtime default would apply, and
		// the owner asked for a stable one.
		{"no request runs at the window", 0, window},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, body := serveRaw(t, ollamaDoneFrame)
			c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), workingContext: window}
			if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: tt.requested}}, discardEmit); err != nil {
				t.Fatal(err)
			}
			if got := sentNumCtx(t, *body); got != tt.want {
				t.Errorf("num_ctx = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOllamaWorkingContext_EmbeddingsKeepTheirFixedWindow. An embedder
// runs at 8192 whatever the chat window is, because its cache would
// otherwise be sized for the chat model's (embedWorkingContext).
func TestOllamaWorkingContext_EmbeddingsKeepTheirFixedWindow(t *testing.T) {
	srv, body := serveRaw(t, ollamaEmbedFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client(), workingContext: 32768}
	if _, err := c.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"hi"}}); err != nil {
		t.Fatal(err)
	}
	if got := sentNumCtx(t, *body); got != embedWorkingContext {
		t.Errorf("embedding num_ctx = %d, want the fixed %d", got, embedWorkingContext)
	}
}

// TestNewClient_CarriesTheModelsResidentSettings. NewClient is the single
// place a runtime is reached from a models.Info, so it is where the
// settings discovery attached have to be picked up -- by the manager that
// serves calls and by the probe, which must measure the path serving uses.
func TestNewClient_CarriesTheModelsResidentSettings(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	info := models.Info{
		ID: "m", Kind: models.KindOllama, BaseURL: srv.URL,
		KeepAlive: twoHourKeepAlive, WorkingContext: 16384,
	}
	c := NewClient(info, srv.Client(), nil)
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Params: Params{ContextTokens: 4096}}, discardEmit); err != nil {
		t.Fatal(err)
	}
	if got := string(sentKeepAlive(t, *body)); got != `"2h0m0s"` {
		t.Errorf("keep_alive = %s, want the model's 2h0m0s", got)
	}
	if got := sentNumCtx(t, *body); got != 16384 {
		t.Errorf("num_ctx = %d, want the model's 16384", got)
	}
}

// TestOpenAICompatibleRuntimesAreNeverSentKeepAlive. keep_alive is an
// Ollama field; a declared runtime's model never carries one (discovery
// refuses to attach it), and the client must not invent one.
func TestOpenAICompatibleRuntimesAreNeverSentKeepAlive(t *testing.T) {
	srv, body := serveRaw(t, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	info := models.Info{
		ID: "m", Kind: models.KindOpenAICompatible, BaseURL: srv.URL,
		KeepAlive: foreverKeepAlive, WorkingContext: 16384, // as a bug would leave them
	}
	c := NewClient(info, srv.Client(), nil)
	_, _ = c.Chat(context.Background(), ChatRequest{Model: "m"}, discardEmit)
	if got := sentKeepAlive(t, *body); got != nil {
		t.Errorf("an OpenAI-compatible request carried keep_alive %s", got)
	}
}

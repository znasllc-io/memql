package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultOpenAIEmbeddingsURL is the upstream embeddings endpoint. The client
// carries it on a field (defaulted here) rather than inlining it at the request
// site purely so tests can point the client at an httptest server.
const defaultOpenAIEmbeddingsURL = "https://api.openai.com/v1/embeddings"

// OpenAIEmbeddingClient implements EmbeddingAIProvider using the OpenAI embeddings API.
//
// # Error-content invariant (memql#3186)
//
// No error returned by this client may contain any byte of the upstream response
// body. The text this client submits for embedding is user content (e.g.
// v1:harness:observation.content, v1:actions:action.intent), and errors from here
// propagate unbroken into log sinks that log them verbatim. Whether the vendor
// echoes the submitted input back in an error body is a vendor behaviour memQL
// neither controls nor is notified about when it changes -- so the bound is held
// here, at the source, rather than at each of the (unbounded set of) log lines.
//
// Every error-construction site in this file is annotated with how it upholds
// that invariant. Adding a new one obliges you to do the same.
type OpenAIEmbeddingClient struct {
	apiKey     string
	model      string
	dimensions int
	baseURL    string
	httpClient *http.Client
}

// Compile-time interface assertions.
var _ AIProvider = (*OpenAIEmbeddingClient)(nil)
var _ EmbeddingAIProvider = (*OpenAIEmbeddingClient)(nil)

// NewOpenAIEmbeddingClient creates an OpenAI embedding client.
func NewOpenAIEmbeddingClient(apiKey, model string, dimensions int) *OpenAIEmbeddingClient {
	return &OpenAIEmbeddingClient{
		apiKey:     apiKey,
		model:      model,
		dimensions: dimensions,
		baseURL:    defaultOpenAIEmbeddingsURL,
		httpClient: &http.Client{},
	}
}

// Call satisfies the AIProvider interface. It returns the embedding as a JSON string.
func (c *OpenAIEmbeddingClient) Call(ctx context.Context, prompt string) (any, error) {
	vec, err := c.Embed(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return vec, nil
}

// Dimensions returns the vector dimensionality (e.g., 1536).
func (c *OpenAIEmbeddingClient) Dimensions() int { return c.dimensions }

// Embed returns a vector embedding for the given text.
func (c *OpenAIEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	batched, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(batched) == 0 {
		return nil, fmt.Errorf("embedding API returned no vectors")
	}
	return batched[0], nil
}

// EmbedBatch embeds multiple texts in a single API call.
func (c *OpenAIEmbeddingClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := map[string]any{
		"input":      texts,
		"model":      c.model,
		"dimensions": c.dimensions,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	url := c.baseURL
	if url == "" {
		url = defaultOpenAIEmbeddingsURL
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		// SIBLING PATH DECISION (memql#3186): left as %w, deliberately.
		// io.ReadAll surfaces the *reader's* error -- transport-level failures
		// ("unexpected EOF", "http2: stream error ...", "context deadline
		// exceeded"). The bytes it managed to read are returned in the first
		// return value, never folded into the error value. There is no path by
		// which a response-body byte reaches this string, so wrapping is safe
		// and the underlying error is worth keeping for errors.Is/As.
		return nil, fmt.Errorf("read embedding response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// TRUNCATE-vs-DROP: neither. We drop the body wholesale and replace it
		// with an allow-list of the vendor's own classification tokens.
		//
		// A fixed-budget truncation was rejected: it bounds the *volume* of
		// leaked bytes, not their *class*. A 256-byte prefix of a body that has
		// begun echoing the submitted input -- or of a WAF/proxy interstitial
		// that quotes request headers -- is still a verbatim leak, and the
		// issue's own rationale is precisely that the vendor's echo behaviour
		// can change without memQL being told. A prefix bound does not survive
		// that change; an allow-list does.
		//
		// A blanket drop was rejected too: `error.type` / `error.code` are the
		// actual debuggability payload (invalid_api_key, rate_limit_exceeded,
		// insufficient_quota, context_length_exceeded) and are vendor-defined
		// enumerations, not free text -- unlike `error.message`, which is prose
		// the vendor composes and is the one field that could ever quote input.
		// `message` is therefore never surfaced. The tokens are additionally
		// shape-checked (see openAIErrorDetail) so that a misbehaving upstream
		// cannot smuggle content through a field we expected to be an enum.
		return nil, fmt.Errorf("embedding API error %d%s", resp.StatusCode, openAIErrorDetail(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		// SIBLING PATH DECISION (memql#3186): hardened, unlike the read path.
		// A %w here *does* carry response bytes: *json.SyntaxError renders the
		// offending byte ("invalid character 'h' ..."), and
		// *json.UnmarshalTypeError renders a body-derived value fragment
		// ("number 1e999999"). Today those are each a token wide and this path
		// is only reachable on a 200 (whose body is float vectors, not the
		// input) -- but encoding/json's error *text* is not a stability
		// contract, and "no response byte ever appears in an error from this
		// client" is an invariant a reviewer can check by inspection whereas
		// "these particular json error shapes happen to be narrow today" is
		// not. So we keep the structural facts (offset, our own struct's field
		// path, our own Go type) and drop the rendered message.
		return nil, fmt.Errorf("parse embedding response: %s", jsonDecodeErrorDetail(err))
	}

	vectors := make([][]float32, len(result.Data))
	for _, d := range result.Data {
		if d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}
	return vectors, nil
}

// maxErrorTokenLen bounds an accepted classification token. OpenAI's longest
// documented error code is well under this; anything longer is not an enum
// value and is discarded rather than trusted.
const maxErrorTokenLen = 48

// openAIErrorDetail renders the vendor's error classification from a non-200
// embeddings response, and nothing else. It returns either "" or a short
// parenthesised suffix such as ` (type=invalid_request_error code=invalid_api_key)`.
//
// It reads ONLY `error.type` and `error.code`. `error.message` is prose the
// vendor composes and is the one field of the envelope that could ever quote the
// submitted input, so it is never read. See the call site for the full
// drop-vs-truncate rationale (memql#3186).
func openAIErrorDetail(body []byte) string {
	// Both fields are decoded into `any` rather than `string` so that a null or
	// numeric value (OpenAI does send `"code": null`) is a miss on that field
	// instead of a decode error that would discard the sibling field too.
	var env struct {
		Error struct {
			Type any `json:"type"`
			Code any `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}

	parts := make([]string, 0, 2)
	if tok, ok := errorClassificationToken(env.Error.Type); ok {
		parts = append(parts, "type="+tok)
	}
	if tok, ok := errorClassificationToken(env.Error.Code); ok {
		parts = append(parts, "code="+tok)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// errorClassificationToken accepts a decoded envelope field only if it is
// shaped like an enum value: a short, non-empty string of identifier characters.
// This is the belt to the allow-list's braces -- it means that even if the
// upstream (or something impersonating it) stuffs the submitted input into a
// field we expected to be an enum, nothing resembling free text escapes.
func errorClassificationToken(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" || len(s) > maxErrorTokenLen {
		return "", false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return "", false
		}
	}
	return s, true
}

// jsonDecodeErrorDetail describes a decode failure of the 200-response body
// using only structural facts -- byte offsets, the field path within the target
// struct declared above, and that struct's own Go types. The rendered
// encoding/json message is dropped because it can embed body-derived fragments.
func jsonDecodeErrorDetail(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("malformed JSON at byte offset %d", syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		// Field is the path within the local `result` struct (which contains no
		// map[string]... member, so no body-supplied key can land in it) and
		// Type is that struct's Go type. Both are ours, not the upstream's.
		return fmt.Sprintf("unexpected JSON type for field %q (want %s) at byte offset %d",
			typeErr.Field, typeErr.Type, typeErr.Offset)
	}
	return "unparseable JSON body"
}

package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// defaultOpenAIBaseURL is the API root the embeddings call is made against. The
// client carries it on a field (defaulted here) rather than inlining it at the
// request site purely so tests can point the client at an httptest server.
//
// It is the ROOT rather than the full endpoint since this call moved onto the
// official SDK, which composes "embeddings" onto it.
const defaultOpenAIBaseURL = "https://api.openai.com/v1/"

// OpenAIEmbeddingClient implements EmbeddingAIProvider using the OpenAI embeddings API.
//
// # Error-content invariant (memql#3186)
//
// No error returned by this client may contain any byte of the upstream response
// body. The text this client submits for embedding is user content (e.g.
// v1:work:observation.content), and errors from here
// propagate unbroken into log sinks that log them verbatim. Whether the vendor
// echoes the submitted input back in an error body is a vendor behaviour MemQL
// neither controls nor is notified about when it changes -- so the bound is held
// here, at the source, rather than at each of the (unbounded set of) log lines.
//
// Every error-construction site in this file is annotated with how it upholds
// that invariant. Adding a new one obliges you to do the same.
type OpenAIEmbeddingClient struct {
	// bearer is the federated credential source (epic memql#5088). It replaced
	// an apiKey field: there is no manually entered vendor key anywhere, and a
	// bearer expires within the hour, so it is resolved per request rather than
	// held.
	bearer     BearerSource
	model      string
	dimensions int
	baseURL    string
	httpClient *http.Client
}

// Compile-time interface assertions.
var _ AIProvider = (*OpenAIEmbeddingClient)(nil)
var _ EmbeddingAIProvider = (*OpenAIEmbeddingClient)(nil)

// NewOpenAIEmbeddingClient creates an OpenAI embedding client.
func NewOpenAIEmbeddingClient(model string, dimensions int) *OpenAIEmbeddingClient {
	return &OpenAIEmbeddingClient{
		model:      model,
		dimensions: dimensions,
		baseURL:    defaultOpenAIBaseURL,
		// Embeddings now ride the guarded transport (memql#5088). The LLM guard
		// fingerprints /chat/completions only, so an embedding call is OBSERVED
		// by the transport and deliberately NOT counted as an LLM call against
		// the loop caps -- which is what the cost-control doc always said the
		// intent was, and what a bare http.Client could not deliver.
		httpClient: guardedHTTPClient(nil),
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

	baseURL := c.baseURL
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = guardedHTTPClient(nil)
	}

	// The SDK client is built per call rather than held on the struct because
	// baseURL and httpClient are a TEST SEAM that is written after construction
	// (openai_embedding_error_test.go), and a memoised client would capture
	// whichever values happened to exist at construction time. One option-slice
	// allocation against a network round trip is not a cost worth that trap.
	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
		// Clear any ambient OPENAI_API_KEY the SDK's defaults picked up out of
		// the process environment; the middleware below is the only thing
		// entitled to set this header.
		option.WithHeaderDel("Authorization"),
	}
	if c.bearer != nil {
		opts = append(opts, option.WithMiddleware(openaiBearerMiddleware(c.bearer)))
	}
	client := openai.NewClient(opts...)

	// THE SDK BUILDS AND SENDS THE REQUEST; THIS FILE STILL READS THE BODY.
	//
	// WithResponseBodyInto a *[]byte hands back the raw contents and skips the
	// SDK's own deserialization entirely. That is what keeps the whole error
	// taxonomy below -- the allow-listed non-200 rendering and the structural
	// parse-stage error -- working exactly as it did, rather than being
	// replaced by whatever text the SDK's decoder happens to produce. The
	// decoder's error text is not a stability contract, and memql#3186's
	// invariant is only checkable by inspection if the set of error strings
	// that can escape this file is closed.
	//
	// What the SDK is used FOR is the part worth having: the credential, the
	// base URL, the retry policy, the guarded transport, and a request body
	// serialised by the vendor's own types.
	var respBody []byte
	_, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input:      openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		Model:      openai.EmbeddingModel(c.model),
		Dimensions: openai.Int(int64(c.dimensions)),
	}, option.WithResponseBodyInto(&respBody))
	if err != nil {
		// THE ERROR-CONTENT INVARIANT SURVIVES THE SDK, and it takes work.
		//
		// openai.Error.Error() renders the UNMODIFIED response body
		// (apierror.go: it formats r.JSON.raw into the message). Returning the
		// SDK's error unwrapped -- or %w-ing it -- would put the vendor's error
		// body, which may echo the submitted input, straight into every log
		// sink that logs these errors verbatim. That is precisely the leak
		// memql#3186 exists to prevent, reintroduced by an SDK swap that looks
		// like a refactor.
		//
		// So no error from the SDK is ever returned as it stands: it goes
		// through sanitizeEmbeddingCallError, which keeps the status code and
		// the vendor's own allow-listed classification tokens and drops
		// everything else. TestEmbedBatchNon200DoesNotLeakResponseBody is the
		// negative control, and its canary is a response body that echoes the
		// submitted text.
		return nil, sanitizeEmbeddingCallError(err)
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

// sanitizeEmbeddingCallError converts an error from the OpenAI SDK into one
// that carries no byte of the upstream response body (memql#3186).
//
// It classifies rather than wraps, and the default arm is the important one:
// an SDK error this function does not RECOGNISE has its message dropped
// entirely and is reported by Go type alone. That is deliberate and is the
// opposite of the usual instinct to preserve the message. The invariant this
// client declares -- "no response byte ever appears in an error from here" --
// is only checkable by inspection if the set of error texts that can escape is
// closed. An unrecognised error from a dependency whose error rendering is not
// a stability contract is exactly the case where preserving the message would
// make the invariant unverifiable.
//
// The two arms that DO keep information:
//
//   - *openai.Error is the vendor's own HTTP error. Its RawJSON() is the
//     response body, which is fed to the SAME allow-list the hand-rolled path
//     used: error.type and error.code only, shape-checked as enum tokens,
//     with error.message (the one field that could quote the input) never read.
//   - a context error is the caller's own cancellation or deadline. It carries
//     no body by construction and callers test for it with errors.Is, so it is
//     wrapped rather than flattened.
func sanitizeEmbeddingCallError(err error) error {
	if err == nil {
		return nil
	}
	// Native refusals contain only budget metadata and must retain their type
	// so optional Training can save a continuation and park before inference.
	var ceiling *RunCeilingError
	if errors.As(err, &ceiling) {
		return ceiling
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		// TRUNCATE-vs-DROP: neither. The body is dropped wholesale and replaced
		// with an allow-list of the vendor's own classification tokens.
		//
		// A fixed-budget truncation was rejected: it bounds the VOLUME of
		// leaked bytes, not their CLASS. A 256-byte prefix of a body that has
		// begun echoing the submitted input -- or of a WAF/proxy interstitial
		// that quotes request headers -- is still a verbatim leak, and the
		// issue's own rationale is precisely that the vendor's echo behaviour
		// can change without MemQL being told. A prefix bound does not survive
		// that change; an allow-list does.
		//
		// A blanket drop was rejected too: type and code are the actual
		// debuggability payload (invalid_api_key, rate_limit_exceeded,
		// insufficient_quota, context_length_exceeded) and are vendor-defined
		// enumerations, not free text -- unlike message, which is prose the
		// vendor composes and is the one field that could ever quote input.
		//
		// The SDK has already parsed the envelope into typed fields, so the
		// allow-list is applied to THOSE rather than re-parsing the raw body:
		// apiErr.Message is simply never read. They are still shape-checked by
		// errorClassificationToken, because a parsed field is only as much of
		// an enum as the upstream chose to make it -- a misbehaving vendor
		// could put prose in `type`, and that is what the shape check is for.
		return fmt.Errorf("embedding API error %d%s",
			apiErr.StatusCode, openAIErrorDetailFromFields(apiErr.Type, apiErr.Code))
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("embedding API call: %w", err)
	}

	// Everything else: structure only. %T names the Go type, which is a fact
	// about our dependency rather than about the response.
	return fmt.Errorf("embedding API call failed (%T)", err)
}

// maxErrorTokenLen bounds an accepted classification token. OpenAI's longest
// documented error code is well under this; anything longer is not an enum
// value and is discarded rather than trusted.
const maxErrorTokenLen = 48

// openAIErrorDetailFromFields renders the vendor's error classification from
// the SDK's already-parsed envelope fields. Same allow-list, same shape check,
// same output shape as openAIErrorDetail -- the only difference is that the
// bytes were parsed by the SDK rather than here.
func openAIErrorDetailFromFields(errType, errCode string) string {
	parts := make([]string, 0, 2)
	if tok, ok := errorClassificationToken(errType); ok {
		parts = append(parts, "type="+tok)
	}
	if tok, ok := errorClassificationToken(errCode); ok {
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

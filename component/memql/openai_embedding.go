package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAIEmbeddingClient implements EmbeddingSIProvider using the OpenAI embeddings API.
type OpenAIEmbeddingClient struct {
	apiKey     string
	model      string
	dimensions int
	httpClient *http.Client
}

// Compile-time interface assertions.
var _ SIProvider = (*OpenAIEmbeddingClient)(nil)
var _ EmbeddingSIProvider = (*OpenAIEmbeddingClient)(nil)

// NewOpenAIEmbeddingClient creates an OpenAI embedding client.
func NewOpenAIEmbeddingClient(apiKey, model string, dimensions int) *OpenAIEmbeddingClient {
	return &OpenAIEmbeddingClient{
		apiKey:     apiKey,
		model:      model,
		dimensions: dimensions,
		httpClient: &http.Client{},
	}
}

// Call satisfies the SIProvider interface. It returns the embedding as a JSON string.
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

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
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
		return nil, fmt.Errorf("read embedding response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embedding response: %w", err)
	}

	vectors := make([][]float32, len(result.Data))
	for _, d := range result.Data {
		if d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}
	return vectors, nil
}

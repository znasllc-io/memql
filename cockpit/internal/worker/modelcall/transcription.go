package modelcall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Dedicated ASR servers consume a multipart file, not a chat input_audio part.
func (c *openAIClient) transcribeFile(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	if len(req.Audio) == 0 || len(req.Audio) > 16<<20 {
		return TranscribeResult{}, fmt.Errorf("transcription audio must contain at most 16 MB")
	}
	if req.Format == "" || strings.ContainsAny(req.Format, "/\\.\r\n") {
		return TranscribeResult{}, fmt.Errorf("unsupported transcription container")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "speech."+req.Format)
	if err != nil {
		return TranscribeResult{}, err
	}
	if _, err = file.Write(req.Audio); err != nil {
		return TranscribeResult{}, err
	}
	fields := map[string]string{"model": req.Model, "response_format": "json"}
	if req.Prompt != "" {
		fields["prompt"] = req.Prompt
	}
	for key, value := range fields {
		if err = writer.WriteField(key, value); err != nil {
			return TranscribeResult{}, err
		}
	}
	if err = writer.Close(); err != nil {
		return TranscribeResult{}, err
	}
	path := "/audio/transcriptions"
	if c.transcription == "whisper-cpp" {
		path = "/inference"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, &body)
	if err != nil {
		return TranscribeResult{}, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return TranscribeResult{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return TranscribeResult{}, err
	}
	if len(data) > 1<<20 {
		return TranscribeResult{}, fmt.Errorf("transcription response exceeds 1 MB")
	}
	if response.StatusCode != http.StatusOK {
		return TranscribeResult{}, fmt.Errorf("transcription runtime returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Text  string          `json:"text"`
		Error json.RawMessage `json:"error"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return TranscribeResult{}, fmt.Errorf("invalid transcription response: %w", err)
	}
	if len(result.Error) > 0 && string(result.Error) != "null" {
		return TranscribeResult{}, fmt.Errorf("transcription runtime reported an error")
	}
	return TranscribeResult{Text: result.Text}, nil
}

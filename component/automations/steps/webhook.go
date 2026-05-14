package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/automations"
)

const defaultHTTPTimeout = 30 * time.Second

// isDemoMode returns true if MEMQL_DEMO_MODE environment variable is set to "true" or "1".
// In demo mode, webhook steps log the request instead of making real HTTP calls.
func isDemoMode() bool {
	val := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_DEMO_MODE")))
	return val == "true" || val == "1"
}

// redactURL redacts sensitive tokens from webhook URLs for safe logging.
// Example: "https://discord.com/api/webhooks/123/secret" -> "https://discord.com/api/webhooks/123/[REDACTED]"
func redactURL(rawURL string) string {
	// Common webhook patterns with tokens in the path
	if strings.Contains(rawURL, "/webhooks/") {
		// Discord, Slack incoming webhooks: .../webhooks/id/token
		parts := strings.Split(rawURL, "/webhooks/")
		if len(parts) == 2 {
			remainder := parts[1]
			segments := strings.SplitN(remainder, "/", 2)
			if len(segments) == 2 {
				return parts[0] + "/webhooks/" + segments[0] + "/[REDACTED]"
			}
		}
	}
	// Fallback: redact query string if present (often contains tokens)
	if idx := strings.Index(rawURL, "?"); idx != -1 {
		return rawURL[:idx] + "?[REDACTED]"
	}
	return rawURL
}

// WebhookExecutor executes HTTP requests.
type WebhookExecutor struct {
	httpClient *http.Client
}

// Execute runs a webhook step.
func (e *WebhookExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Webhook == nil {
		result.Status = "failed"
		result.Error = "webhook configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("webhook configuration is required")
	}

	webhook := step.Webhook

	// Evaluate URL with $ expressions
	url, err := stepCtx.Evaluator.EvaluateString(webhook.URL)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate URL: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate URL: %w", err)
	}

	method := strings.ToUpper(strings.TrimSpace(webhook.Method))
	if method == "" {
		method = "POST"
	}

	// Build request body
	var bodyReader io.Reader
	var bodyData any

	if webhook.IncludeResult {
		// Include result from a previous step
		resultFrom := webhook.ResultFrom
		if resultFrom == "" {
			// Find the most recent step result
			for stepId, stepResult := range stepCtx.Execution.Steps {
				if stepId != step.ID && stepResult.Status == "success" {
					bodyData = stepResult.Result
				}
			}
		} else {
			if stepResult, ok := stepCtx.Execution.Steps[resultFrom]; ok {
				bodyData = stepResult.Result
			}
		}
	} else if webhook.Body != nil {
		// Evaluate $ expressions in body
		evaluatedBody, err := stepCtx.Evaluator.EvaluateMap(webhook.Body)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate body: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate body: %w", err)
		}
		bodyData = evaluatedBody
	}

	if bodyData != nil {
		data, err := json.Marshal(bodyData)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to marshal body: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to create request: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to create request: %w", err)
	}

	// Set content type if body is present
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Set headers with $ expression evaluation
	for key, value := range webhook.Headers {
		evaluatedValue, err := stepCtx.Evaluator.EvaluateString(value)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate header %s: %v", key, err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate header %s: %w", key, err)
		}
		req.Header.Set(key, evaluatedValue)
	}

	// Determine timeout
	client := e.getClient()
	if timeout := strings.TrimSpace(webhook.Timeout); timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil {
			client = &http.Client{Timeout: d}
		}
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing webhook step",
			"step", step.ID,
			"url", redactURL(url),
			"method", method,
		)
	}

	// In demo mode, log the request and return a mock success response
	if isDemoMode() {
		if stepCtx.Logger != nil {
			stepCtx.Logger.Info("DEMO MODE: webhook request simulated",
				"component", automations.ComponentName,
				"step", step.ID,
				"url", redactURL(url),
				"method", method,
				"body", bodyData,
			)
		}

		result.Status = "success"
		result.Result = map[string]any{
			"demo":    true,
			"message": "Request simulated in demo mode",
			"url":     url,
			"method":  method,
		}
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"url":        url,
			"method":     method,
			"statusCode": 200,
			"demoMode":   true,
		}
		return result, nil
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("webhook request failed: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to read response: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode >= 400 {
		result.Status = "failed"
		result.Error = fmt.Sprintf("webhook returned status %d: %s", resp.StatusCode, string(respBody))
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	// Parse response body
	var responseData any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &responseData); err != nil {
			// If not JSON, store as string
			responseData = string(respBody)
		}
	}

	result.Status = "success"
	result.Result = responseData
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"url":        url,
		"method":     method,
		"statusCode": resp.StatusCode,
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:      runId,
		StepId:     step.ID,
		StepType:   "webhook",
		Status:     result.Status,
		URL:        url,
		StatusCode: resp.StatusCode,
		Duration:   float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("webhook step completed",
			"step", step.ID,
			"url", redactURL(url),
			"statusCode", resp.StatusCode,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

func (e *WebhookExecutor) getClient() *http.Client {
	if e.httpClient != nil {
		return e.httpClient
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

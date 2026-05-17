package steps

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// DetectLeadSignalExecutor performs deterministic regex-based detection of lead signals.
// This is a cost-control pre-filter to avoid unnecessary AI calls.
type DetectLeadSignalExecutor struct{}

// Common regex patterns for lead signal detection
var (
	// Email pattern: standard email format
	emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

	// Phone pattern: various formats, will normalize to E.164
	// Matches: +1-555-123-4567, (555) 123-4567, 555.123.4567, +14155551234, etc.
	phonePattern = regexp.MustCompile(`(?:\+?1[-.\s]?)?\(?[2-9]\d{2}\)?[-.\s]?\d{3}[-.\s]?\d{4}`)

	// International phone pattern: starts with + followed by country code
	intlPhonePattern = regexp.MustCompile(`\+[1-9]\d{6,14}`)

	// Intent signal keywords (case-insensitive matching done separately)
	intentKeywords = []string{
		"interested", "buy", "purchase", "pricing", "quote", "demo",
		"trial", "sign up", "signup", "subscribe", "contact", "call me",
		"reach out", "schedule", "appointment", "meeting", "budget",
		"how much", "cost", "price", "investment", "enterprise",
	}

	// Product mention patterns (can be customized per deployment)
	productKeywords = []string{
		"product", "service", "solution", "platform", "software",
		"plan", "tier", "package", "offering", "feature",
	}
)

// LeadSignalResult contains the detection results.
type LeadSignalResult struct {
	// ShouldExtract indicates if AI extraction should proceed
	ShouldExtract bool `json:"shouldExtract"`

	// HasEmail indicates if an email was detected
	HasEmail bool `json:"hasEmail"`

	// HasPhone indicates if a phone number was detected
	HasPhone bool `json:"hasPhone"`

	// HasProductMention indicates if a product-related mention was found
	HasProductMention bool `json:"hasProductMention"`

	// HasIntentSignal indicates if purchase intent signals were found
	HasIntentSignal bool `json:"hasIntentSignal"`

	// DetectedEmail is the first email found (if any)
	DetectedEmail string `json:"detectedEmail,omitempty"`

	// DetectedPhone is the first phone found, normalized toward E.164 (if any)
	DetectedPhone string `json:"detectedPhone,omitempty"`

	// DetectedIntents lists the matched intent keywords
	DetectedIntents []string `json:"detectedIntents,omitempty"`

	// DetectedProducts lists the matched product keywords
	DetectedProducts []string `json:"detectedProducts,omitempty"`
}

// Execute runs the lead signal detection step.
func (e *DetectLeadSignalExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	// Get configuration from step
	cfg := getDetectLeadSignalConfig(step)
	if cfg == nil || cfg.Source == "" {
		result.Status = "failed"
		result.Error = "detectLeadSignal requires source configuration"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, nil
	}

	// Resolve the source text
	sourceValue, err := stepCtx.Evaluator.EvaluateStepReference(cfg.Source)
	if err != nil {
		result.Status = "failed"
		result.Error = "failed to evaluate source: " + err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, nil
	}

	text, ok := sourceValue.(string)
	if !ok {
		// Try to convert to string
		text = automations.FormatValue(sourceValue)
	}

	// Perform detection
	signalResult := detectSignals(text, cfg)

	result.Status = "success"
	result.Result = signalResult
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"source":        cfg.Source,
		"textLength":    len(text),
		"shouldExtract": signalResult.ShouldExtract,
		"hasEmail":      signalResult.HasEmail,
		"hasPhone":      signalResult.HasPhone,
	}

	// Record step execution
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:    runId,
		StepId:   step.ID,
		StepType: "detectLeadSignal",
		Status:   result.Status,
		Duration: float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("detectLeadSignal completed",
			"step", step.ID,
			"shouldExtract", signalResult.ShouldExtract,
			"hasEmail", signalResult.HasEmail,
			"hasPhone", signalResult.HasPhone,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// getDetectLeadSignalConfig extracts configuration from a step.
func getDetectLeadSignalConfig(step *automations.Step) *automations.DetectLeadSignalStepConfig {
	if step == nil {
		return nil
	}

	return step.DetectLeadSignal
}

// detectSignals performs the actual signal detection.
func detectSignals(text string, cfg *automations.DetectLeadSignalStepConfig) *LeadSignalResult {
	result := &LeadSignalResult{}
	textLower := strings.ToLower(text)

	// Detect email
	if match := emailPattern.FindString(text); match != "" {
		result.HasEmail = true
		result.DetectedEmail = strings.ToLower(match)
	}

	// Detect phone (try international first, then domestic patterns)
	if match := intlPhonePattern.FindString(text); match != "" {
		result.HasPhone = true
		result.DetectedPhone = normalizePhone(match)
	} else if match := phonePattern.FindString(text); match != "" {
		result.HasPhone = true
		result.DetectedPhone = normalizePhone(match)
	}

	// Detect intent signals
	// Copy slice to avoid mutating global intentKeywords
	allIntentKeywords := append([]string{}, intentKeywords...)
	if cfg != nil && len(cfg.CustomIntentKeywords) > 0 {
		allIntentKeywords = append(allIntentKeywords, cfg.CustomIntentKeywords...)
	}
	for _, keyword := range allIntentKeywords {
		if strings.Contains(textLower, strings.ToLower(keyword)) {
			result.HasIntentSignal = true
			result.DetectedIntents = append(result.DetectedIntents, keyword)
		}
	}

	// Detect product mentions
	// Copy slice to avoid mutating global productKeywords
	allProductKeywords := append([]string{}, productKeywords...)
	if cfg != nil && len(cfg.CustomProductKeywords) > 0 {
		allProductKeywords = append(allProductKeywords, cfg.CustomProductKeywords...)
	}
	for _, keyword := range allProductKeywords {
		if strings.Contains(textLower, strings.ToLower(keyword)) {
			result.HasProductMention = true
			result.DetectedProducts = append(result.DetectedProducts, keyword)
		}
	}

	// Determine if extraction should proceed
	// Extract if: has contact info OR (has intent AND product mention)
	result.ShouldExtract = result.HasEmail || result.HasPhone ||
		(result.HasIntentSignal && result.HasProductMention)

	return result
}

// normalizePhone attempts to normalize a phone number toward E.164 format.
// This is a best-effort normalization - the AI will do final E.164 conversion.
func normalizePhone(phone string) string {
	// Remove all non-digit characters except leading +
	var normalized strings.Builder
	hasPlus := strings.HasPrefix(phone, "+")
	if hasPlus {
		normalized.WriteByte('+')
	}

	for _, r := range phone {
		if r >= '0' && r <= '9' {
			normalized.WriteRune(r)
		}
	}

	result := normalized.String()

	// If it's a 10-digit US number without country code, add +1
	digitsOnly := strings.TrimPrefix(result, "+")
	if len(digitsOnly) == 10 && !hasPlus {
		return "+1" + digitsOnly
	}

	// If it's 11 digits starting with 1, ensure + prefix
	if len(digitsOnly) == 11 && digitsOnly[0] == '1' && !hasPlus {
		return "+" + digitsOnly
	}

	return result
}

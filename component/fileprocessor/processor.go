// Package fileprocessor extracts plain text from uploaded file data.
// It dispatches to type-specific extractors based on MIME type.
package fileprocessor

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// Processor extracts plain text from raw file bytes.
type Processor interface {
	Extract(ctx context.Context, mimeType string, data []byte) (string, error)
}

// SupportedMIMETypes is the set of MIME types this processor can handle.
var SupportedMIMETypes = map[string]bool{
	"application/pdf": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"text/plain":    true,
	"text/markdown": true,
	"image/jpeg":    true,
	"image/png":     true,
	"image/gif":     true,
	"image/webp":    true,
}

// IsSupportedMIMEType returns true if the MIME type is supported by this processor.
func IsSupportedMIMEType(mimeType string) bool {
	return SupportedMIMETypes[normalizeMIME(mimeType)]
}

// DefaultProcessor dispatches extraction to type-specific handlers.
type DefaultProcessor struct {
	visionProvider common.VisionAIProvider
}

// NewDefaultProcessor creates a Processor that uses the provided VisionAIProvider for image description.
func NewDefaultProcessor(visionProvider common.VisionAIProvider) *DefaultProcessor {
	return &DefaultProcessor{visionProvider: visionProvider}
}

// Extract selects the appropriate extractor for the given MIME type.
func (p *DefaultProcessor) Extract(ctx context.Context, mimeType string, data []byte) (string, error) {
	norm := normalizeMIME(mimeType)
	switch norm {
	case "application/pdf":
		return extractPDF(data)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return extractDOCX(data)
	case "text/plain", "text/markdown":
		return extractText(data)
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return describeImage(ctx, p.visionProvider, norm, data)
	default:
		return "", fmt.Errorf("unsupported MIME type: %s", mimeType)
	}
}

// normalizeMIME strips parameters and lowercases a MIME type string.
func normalizeMIME(mimeType string) string {
	mimeType = strings.TrimSpace(strings.ToLower(mimeType))
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	return mimeType
}

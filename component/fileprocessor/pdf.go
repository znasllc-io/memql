package fileprocessor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// extractPDF extracts plain text from PDF bytes using the ledongthuc/pdf library.
func extractPDF(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}

	numPages := r.NumPage()
	if numPages == 0 {
		return "", nil
	}

	var sb strings.Builder
	for i := 1; i <= numPages; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			// Skip pages that cannot be parsed.
			continue
		}
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			sb.WriteString(trimmed)
			sb.WriteByte('\n')
		}
		// Stop if we've accumulated enough text.
		if sb.Len() >= maxTextBytes {
			break
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

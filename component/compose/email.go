package compose

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// EmailTemplate is an editable authoring file, not a published campaign or an
// instruction to send. The campaign renderer supplies recipient merge values.
type EmailTemplate struct {
	Subject  string `json:"subject"`
	TextBody string `json:"textBody"`
	HTMLBody string `json:"htmlBody"`
}

// RenderEmailTemplate preserves email HTML exactly, without the document HTML
// renderer's Markdown conversion or page chrome. Its preview is sandboxed by the
// editor; delivery still uses Campaigns' renderer and organization checks.
func RenderEmailTemplate(source string) (Result, error) {
	if len(source) > 2<<20 {
		return Result{}, errors.New("email template exceeds 2 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.DisallowUnknownFields()
	var email EmailTemplate
	if err := decoder.Decode(&email); err != nil {
		return Result{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Result{}, errors.New("email template contains trailing content")
	}
	if strings.TrimSpace(email.Subject) == "" || strings.ContainsAny(email.Subject, "\r\n") || len(email.Subject) > 998 {
		return Result{}, errors.New("email subject must be a nonempty single line under 999 bytes")
	}
	if strings.TrimSpace(email.TextBody) == "" || strings.TrimSpace(email.HTMLBody) == "" {
		return Result{}, errors.New("email template needs both editable HTML and a plain-text alternative")
	}
	data, err := json.MarshalIndent(email, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Bytes: append(data, '\n'), Embedded: false, Note: "Email JSON contains only subject, textBody, and htmlBody; provenance is stored on the composition."}, nil
}

package compose

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// EmailTemplate is an editable authoring file, not a published campaign or an
// instruction to send. The campaign renderer supplies recipient merge values.
type EmailTemplate struct {
	Subject  string `json:"subject"`
	TextBody string `json:"textBody"`
	HTMLBody string `json:"htmlBody"`
}

// ParseEmailTemplate validates the editable file. Campaigns also supports
// plain-text messages with an empty HTML alternative.
func ParseEmailTemplate(source string) (EmailTemplate, error) {
	if len(source) > 2<<20 {
		return EmailTemplate{}, errors.New("email template exceeds 2 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.DisallowUnknownFields()
	var email EmailTemplate
	if err := decoder.Decode(&email); err != nil {
		return EmailTemplate{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return EmailTemplate{}, errors.New("email template contains trailing content")
	}
	if strings.TrimSpace(email.Subject) == "" || strings.IndexFunc(email.Subject, unicode.IsControl) >= 0 || len(email.Subject) > 998 {
		return EmailTemplate{}, errors.New("email subject must be a nonempty single line under 999 bytes")
	}
	if strings.TrimSpace(email.TextBody) == "" {
		return EmailTemplate{}, errors.New("email template needs a plain-text body")
	}
	return email, nil
}

// RenderEmailTemplate requires the editable HTML promised by an AI email draft.
func RenderEmailTemplate(source string) (Result, error) {
	email, err := ParseEmailTemplate(source)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(email.HTMLBody) == "" {
		return Result{}, errors.New("generated email template needs editable HTML")
	}
	data, err := json.MarshalIndent(email, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Bytes: append(data, '\n'), Embedded: false, Note: "Email JSON contains only subject, textBody, and htmlBody; provenance is stored on the composition."}, nil
}

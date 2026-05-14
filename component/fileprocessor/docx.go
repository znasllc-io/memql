package fileprocessor

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// extractDOCX extracts plain text from a DOCX file using only stdlib zip + XML.
// DOCX is a ZIP archive containing word/document.xml with text in <w:t> elements.
func extractDOCX(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open docx zip: %w", err)
	}

	// Locate word/document.xml inside the ZIP.
	var docFile *zip.File
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", fmt.Errorf("word/document.xml not found in docx archive")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("open document.xml: %w", err)
	}
	defer rc.Close()

	xmlBytes, err := io.ReadAll(io.LimitReader(rc, maxTextBytes))
	if err != nil {
		return "", fmt.Errorf("read document.xml: %w", err)
	}

	text, err := extractTextFromWordXML(xmlBytes)
	if err != nil {
		return "", fmt.Errorf("parse document.xml: %w", err)
	}

	return strings.TrimSpace(text), nil
}

// extractTextFromWordXML parses word/document.xml and concatenates <w:t> element content.
func extractTextFromWordXML(xmlData []byte) (string, error) {
	var sb strings.Builder
	decoder := xml.NewDecoder(bytes.NewReader(xmlData))

	inText := false
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sb.String(), nil // return partial text on XML error
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// <w:t> elements contain the actual text runs
			if t.Name.Local == "t" {
				inText = true
			}
			// <w:p> marks a paragraph boundary
			if t.Name.Local == "p" && sb.Len() > 0 {
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}

	return sb.String(), nil
}

package compose

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"time"
)

// A .docx IS A ZIP OF XML AT KNOWN PATHS, which is why this file needs
// no library. `archive/zip` is in the standard library and the same
// observation component/packages/source.go and component/sitepublish
// already lean on -- the alternative was a vendor dependency whose only
// job would be writing four small documents.
//
// The four parts a minimal, valid, Word-openable document needs:
//
//	[Content_Types].xml          what each part is
//	_rels/.rels                  the package's root relationships
//	word/document.xml            the content
//	docProps/core.xml            THE PROVENANCE (Dublin Core)
//	docProps/app.xml             the generator
//	docProps/custom.xml          THE REST OF THE PROVENANCE
//
// core.xml is where a reader's File -> Properties dialog looks, so the
// facts a person would check by hand -- title, author, description, when
// -- go there in the standard Dublin Core terms rather than in custom
// properties. The MemQL-specific ids go in custom.xml, which is the part
// of the format that exists for exactly this.

const (
	docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>
<Override PartName="/docProps/custom.xml" ContentType="application/vnd.openxmlformats-officedocument.custom-properties+xml"/>
</Types>`

	docxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>
<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/custom-properties" Target="docProps/custom.xml"/>
</Relationships>`

	docxAppProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">
<Application>MemQL Materializer</Application>
</Properties>`
)

func renderDOCX(d Draft, p Provenance) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	parts := []struct {
		name string
		body string
	}{
		{"[Content_Types].xml", docxContentTypes},
		{"_rels/.rels", docxRootRels},
		{"word/document.xml", docxDocument(d)},
		{"docProps/core.xml", docxCoreProps(p, d)},
		{"docProps/app.xml", docxAppProps},
		{"docProps/custom.xml", docxCustomProps(p)},
	}
	for _, part := range parts {
		w, err := zw.Create(part.name)
		if err != nil {
			return nil, fmt.Errorf("compose: docx part %s: %w", part.name, err)
		}
		if _, err := w.Write([]byte(part.body)); err != nil {
			return nil, fmt.Errorf("compose: docx part %s: %w", part.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compose: closing docx: %w", err)
	}
	return buf.Bytes(), nil
}

// docxDocument renders the draft as WordprocessingML paragraphs.
//
// The title takes the built-in `Title` style and each paragraph is plain
// body text. THIS IS NOT A MARKDOWN RENDERER, for renderHTML's reason:
// a half-implemented one that handles bold and not tables produces
// documents wrong in ways nobody predicts. A template (design D7) is how
// a branded, styled document is produced, and that is the honest
// division -- this writer makes a correct document, a template makes a
// handsome one.
func docxDocument(d Draft) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	if title := strings.TrimSpace(d.Title); title != "" {
		b.WriteString(`<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t xml:space="preserve">`)
		b.WriteString(escapeXML(title))
		b.WriteString(`</w:t></w:r></w:p>`)
	}
	for _, para := range splitParagraphs(d.Body) {
		b.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		b.WriteString(escapeXML(para))
		b.WriteString(`</w:t></w:r></w:p>`)
	}
	b.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr>`)
	b.WriteString(`</w:body></w:document>`)
	return b.String()
}

// docxCoreProps is the Dublin Core block a reader's File -> Properties
// dialog shows. The facts a person would check by hand go HERE rather
// than in custom properties, because this is the part every word
// processor surfaces without being asked.
func docxCoreProps(p Provenance, d Draft) string {
	stamp := p.CreatedAt.UTC().Format(time.RFC3339)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`)
	writeXMLElement(&b, "dc:title", firstNonEmpty(p.Title, d.Title))
	writeXMLElement(&b, "dc:creator", firstNonEmpty(p.AuthorName, "MemQL Materializer"))
	writeXMLElement(&b, "cp:lastModifiedBy", firstNonEmpty(p.AuthorName, "MemQL Materializer"))
	writeXMLElement(&b, "dc:description", p.Statement)
	writeXMLElement(&b, "cp:keywords", p.SourceSummary()+"; "+p.ModelSummary())
	b.WriteString(`<dcterms:created xsi:type="dcterms:W3CDTF">` + escapeXML(stamp) + `</dcterms:created>`)
	b.WriteString(`<dcterms:modified xsi:type="dcterms:W3CDTF">` + escapeXML(stamp) + `</dcterms:modified>`)
	b.WriteString(`</cp:coreProperties>`)
	return b.String()
}

// docxCustomProps carries the ids -- the facts that let somebody trace a
// document in a downloads folder back to the row explaining it.
//
// Every property is a string, including the ids, because a custom
// property's type is declared per property and a reader that guesses
// wrong renders nothing. A traceable id that renders as blank is worth
// less than a string.
func docxCustomProps(p Provenance) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">`)
	pid := 1
	add := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		pid++
		fmt.Fprintf(&b, `<property fmtid="{D5CDD505-2E9C-101B-9397-08002B2CF9AE}" pid="%d" name="%s"><vt:lpwstr>%s</vt:lpwstr></property>`,
			pid, escapeXML(name), escapeXML(value))
	}
	add("MemQL Instance", p.Instance)
	add("MemQL Composition", p.CompositionId)
	add("MemQL Goal", p.GoalId)
	add("MemQL Author", p.AuthorId)
	add("MemQL Template", p.TemplateName)
	add("MemQL Sources", p.SourceSummary())
	add("MemQL Models", p.ModelSummary())
	b.WriteString(`</Properties>`)
	return b.String()
}

func writeXMLElement(b *strings.Builder, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString("<" + name + ">" + escapeXML(value) + "</" + name + ">")
}

// escapeXML escapes the five predefined entities AND strips the control
// characters XML 1.0 forbids outright.
//
// THE STRIP IS THE HALF THAT MATTERS. Every value here is free text --
// somebody's goal statement, a row's own field -- and a lone 0x0B in one
// of them produces a .docx that Word refuses to open with a message
// naming nothing. Escaping alone does not help: `&#11;` is equally
// invalid in XML 1.0.
func escapeXML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		case '\t', '\n', '\r':
			b.WriteRune(r)
		default:
			if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

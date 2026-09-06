package compose

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// THE ONE VENDOR DEPENDENCY IN THIS EPIC, and the reason is that a PDF
// is the one format here that cannot be hand-written responsibly. A
// .docx is a zip of XML at known paths (docx.go needs no library at
// all); a PDF needs a cross-reference table, an object graph and font
// metrics, and rolling those by hand is how a project ships a file that
// opens in one reader and not another.
//
// github.com/go-pdf/fpdf is pure Go with no CGO -- which matters,
// because the engine's node images are distroless and the voice binary
// is the only one in the product that carries a C toolchain.
//
// TWO PROVENANCE CHANNELS, AND BOTH ARE WRITTEN. The Info dictionary is
// what a reader's Document Properties dialog shows, so the facts a
// person checks by hand go there; the XMP packet is the structured,
// standard one that survives a re-save in most tooling and is what a
// machine reads. Writing one and not the other means either a person or
// a program cannot see the provenance, and there is no reason to choose.

func renderPDF(d Draft, p Provenance) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(20, 20, 20)
	pdf.SetAutoPageBreak(true, 20)

	// The Info dictionary. `Producer` names this product rather than
	// the library: somebody reading Document Properties is asking what
	// made the file, and "fpdf" is not an answer they can act on.
	pdf.SetTitle(firstNonEmpty(d.Title, p.Title), true)
	pdf.SetAuthor(firstNonEmpty(p.AuthorName, "MemQL Materializer"), true)
	pdf.SetSubject(p.Statement, true)
	pdf.SetCreator("MemQL Materializer", true)
	pdf.SetKeywords(strings.TrimSpace(p.SourceSummary()+"; "+p.ModelSummary()), true)
	pdf.SetXmpMetadata(xmpPacket(d, p))

	pdf.AddPage()
	if title := strings.TrimSpace(firstNonEmpty(d.Title, p.Title)); title != "" {
		pdf.SetFont("Helvetica", "B", 20)
		pdf.MultiCell(0, 9, tr(title), "", "L", false)
		pdf.Ln(4)
	}
	pdf.SetFont("Helvetica", "", 11)
	for _, para := range splitParagraphs(d.Body) {
		pdf.MultiCell(0, 6, tr(para), "", "L", false)
		pdf.Ln(3)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("compose: writing pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// tr folds text into the Latin-1 range fpdf's core fonts can encode.
//
// WITHOUT THIS, A SMART QUOTE RENDERS AS NOTHING. The core PDF fonts
// are single-byte encoded, and a model's prose is full of curly quotes,
// en dashes and ellipses -- so the characters that would silently
// vanish are precisely the ones composed text is made of. Folding them
// to their ASCII equivalents is visible and correct; dropping them is
// neither. Anything still out of range becomes '?' rather than
// disappearing, so a missing character is a mark somebody can see.
func tr(s string) string {
	replacer := strings.NewReplacer(
		"‘", "'", "’", "'", "‚", "'", "‛", "'",
		"“", "\"", "”", "\"", "„", "\"", "‟", "\"",
		"–", "-", "—", "--", "―", "--", "−", "-",
		"…", "...", " ", " ", "•", "-", "→", "->",
		"«", "<<", "»", ">>", "‹", "<", "›", ">",
	)
	folded := replacer.Replace(s)
	var b strings.Builder
	b.Grow(len(folded))
	for _, r := range folded {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20:
			// Control bytes are dropped rather than substituted: they
			// were never visible, so a '?' in their place would be
			// this writer inventing a character.
		case r <= 0xFF:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// xmpPacket is the structured provenance a machine reads: Dublin Core
// for the human-facing facts, an XMP Basic block for the producer, and a
// MemQL namespace for the ids that make a file traceable to its record.
func xmpPacket(d Draft, p Provenance) []byte {
	stamp := p.CreatedAt.UTC().Format(time.RFC3339)
	var b strings.Builder
	b.WriteString(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>` + "\n")
	b.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/">` + "\n")
	b.WriteString(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n")
	b.WriteString(`<rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmlns:memql="https://memql.dev/ns/compose/1.0/">` + "\n")

	if title := firstNonEmpty(d.Title, p.Title); title != "" {
		b.WriteString(`<dc:title><rdf:Alt><rdf:li xml:lang="x-default">` + escapeXML(title) + `</rdf:li></rdf:Alt></dc:title>` + "\n")
	}
	if p.AuthorName != "" {
		b.WriteString(`<dc:creator><rdf:Seq><rdf:li>` + escapeXML(p.AuthorName) + `</rdf:li></rdf:Seq></dc:creator>` + "\n")
	}
	if p.Statement != "" {
		b.WriteString(`<dc:description><rdf:Alt><rdf:li xml:lang="x-default">` + escapeXML(p.Statement) + `</rdf:li></rdf:Alt></dc:description>` + "\n")
	}
	b.WriteString(`<xmp:CreatorTool>MemQL Materializer</xmp:CreatorTool>` + "\n")
	b.WriteString(`<xmp:CreateDate>` + escapeXML(stamp) + `</xmp:CreateDate>` + "\n")

	writeXMPField(&b, "instance", p.Instance)
	writeXMPField(&b, "compositionId", p.CompositionId)
	writeXMPField(&b, "goalId", p.GoalId)
	writeXMPField(&b, "authorId", p.AuthorId)
	writeXMPField(&b, "template", p.TemplateName)
	writeXMPField(&b, "sources", p.SourceSummary())
	// modelsUsed is written even when it is "no model": on a catalog
	// hit nothing reached a provider, which is the product's headline
	// claim rather than a missing value, and a reader finding the key
	// absent could not tell that from a writer that forgot it.
	writeXMPField(&b, "modelsUsed", p.ModelSummary())

	b.WriteString(`</rdf:Description>` + "\n</rdf:RDF>\n</x:xmpmeta>\n<?xpacket end=\"w\"?>")
	return []byte(b.String())
}

func writeXMPField(b *strings.Builder, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString("<memql:" + name + ">" + escapeXML(value) + "</memql:" + name + ">\n")
}

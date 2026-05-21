package knowledge

import (
	"regexp"
	"strings"
)

// MaxChunkTitleLen is the hard cap on the post-sanitization length
// of a knowledge-chunk title. Titles are meant to be short topical
// noun phrases ("Customer onboarding flow", "GA team org chart");
// anything past 512 runes is either an accidental dump of a body
// paragraph or an attempt to flood the retrieval surface with
// instruction-shaped text. Truncating at this cap keeps the
// downstream prompt context bounded regardless of input shape.
const MaxChunkTitleLen = 512

// untitledChunkTitle is the fallback emitted when sanitization
// strips a title down to the empty string (e.g. the original was
// ALL markup / role markers). Beats letting `## ` empty headers
// leak into the chunk body the indexer composes from
// `## {title}\n\n{body}`.
const untitledChunkTitle = "untitled"

// leadingMarkdownHeader strips one or more `#` characters at the
// very start of the (already-trimmed) string -- the "## title"
// markdown-header form.
var leadingMarkdownHeader = regexp.MustCompile(`^#+\s*`)

// trailingMarkdownHeader strips one or more `#` characters at the
// very end of the string (with optional whitespace before).
// Catches the "## title ##" decorative form so a stray trailing
// "##" doesn't survive cleanup.
var trailingMarkdownHeader = regexp.MustCompile(`\s*#+\s*$`)

// roleMarkerInline matches role-marker fragments that can appear
// anywhere in the title and are unambiguously instruction-shaped.
// Conservative: tokenizer markers (`<|im_start|>` etc.) and the
// `### System:` / `### Instruction:` markdown-flavored variants.
// Each match is replaced with a single space.
var roleMarkerInline = regexp.MustCompile(
	`(?i)(<\|im_start\|>|<\|im_end\|>|<\|system\|>|<\|assistant\|>|<\|user\|>|###\s*system\s*:?|###\s*instruction\s*:?)`,
)

// leadingRoleLabel matches a role label at the very start of the
// (already-leading-markdown-stripped) string -- e.g. titles that
// begin "System: <directive>" or "Instruction: <directive>". The
// label + trailing colon + any following whitespace get redacted.
// Only matched at the start so legitimate titles like
// "Linux system: kernel notes" pass through.
var leadingRoleLabel = regexp.MustCompile(
	`(?i)^(system|assistant|user|instruction)\s*:\s*`,
)

// whitespaceCollapse rewrites every run of one-or-more whitespace
// characters into a single space. Lets the cleanup pass leave a
// neatly-spaced title after role markers and headers are stripped.
var whitespaceCollapse = regexp.MustCompile(`\s+`)

// SanitizeChunkTitle returns a defensive cleaning of a user-supplied
// chunk title before it lands in the knowledge index. The goal is
// to keep the title a topical noun phrase and not a vehicle for
// prompt injection at retrieval time.
//
// Transformations (applied in order):
//
//  1. Trim leading + trailing whitespace.
//  2. Strip a leading markdown header ("##", "###", etc.).
//  3. Redact inline role-marker fragments: tokenizer markers
//     (<|im_start|>, etc.) and the "### System:" / "### Instruction:"
//     markdown-flavored variants.
//  4. Strip a leading role label ("System:", "Instruction:", etc.)
//     -- only at the very start; mid-sentence colons survive.
//  5. Collapse internal whitespace runs to a single space.
//  6. Truncate to MaxChunkTitleLen runes.
//  7. If the cleaning leaves the empty string, return "untitled"
//     so the indexer's `## {title}` header construction stays
//     well-formed.
//
// This is defense-in-depth ON TOP OF the prompt-render-time
// [[BEGIN UNTRUSTED RETRIEVED KNOWLEDGE]] framing (bff-copresent
// PR #25). The framing is the primary defense; sanitization
// reduces the obvious attack surface so a poisoned title doesn't
// even sit in the retrieval pool to nudge the model.
//
// Surfaced by bff-copresent#29 (Wave 4 prompt-injection audit).
func SanitizeChunkTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = leadingMarkdownHeader.ReplaceAllString(s, "")
	s = trailingMarkdownHeader.ReplaceAllString(s, "")
	s = roleMarkerInline.ReplaceAllString(s, " ")
	// Re-trim before the leading-role-label check so role labels
	// exposed by inline-marker removal (e.g. "## <|im_start|>
	// Instruction: ..." -> " Instruction: ...") still anchor.
	s = strings.TrimSpace(s)
	s = leadingRoleLabel.ReplaceAllString(s, "")
	s = whitespaceCollapse.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Rune-aware truncation so multi-byte characters don't get cut
	// mid-codepoint.
	if r := []rune(s); len(r) > MaxChunkTitleLen {
		s = string(r[:MaxChunkTitleLen])
	}

	if s == "" {
		return untitledChunkTitle
	}
	return s
}

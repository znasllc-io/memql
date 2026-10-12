package emaildoc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/compose"
)

// Block types the parser needs to name; the others only appear in blockKeys.
const (
	typeImage   = "image"
	typeButton  = "button"
	typeText    = "text"
	typeSection = "section"
	typeColumns = "columns"
	typeSocial  = "social"
	typeFooter  = "footer"
	typeHTML    = "html"
)

// maxHrefBytes bounds a link; maxHTMLBytes bounds an html block.
const (
	maxHrefBytes = 2000
	maxHTMLBytes = 64 << 10
)

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// The keys each object of the format accepts. Anything else is refused: the
// check is on the exact key, because encoding/json would otherwise match
// "Text" to the field text and a long s ("ſize") to the field size.
var (
	layoutKeys     = wordSet("subject preheader layout theme blocks")
	customKeys     = wordSet("subject preheader textBody htmlBody")
	themeKeys      = wordSet("page canvas primary secondary accent tint text muted headingFont bodyFont webFonts")
	columnKeys     = wordSet("width cellPadding blocks")
	socialLinkKeys = wordSet("network href icon width")
	footerLinkKeys = wordSet("label href")

	// blockKeys is, per block type, every key the type accepts: id, type and
	// role, the keys of its own, and padding on every type but spacer.
	blockKeys = func() map[string]map[string]bool {
		own := map[string]string{
			"image":     "src alt width height align href bleed",
			"heading":   "text size lineHeight color align italic weight font letterSpacing",
			"text":      "text size lineHeight listLineHeight gap color linkColor align italic weight font letterSpacing",
			"highlight": "text size lineHeight gap color background align italic weight font letterSpacing",
			"button":    "label href color textColor width align size paddingY font",
			"divider":   "color height",
			"spacer":    "height",
			"section":   "background inset maxWidth cellPadding blocks",
			"columns":   "background inset maxWidth gap stackBelow columns",
			"social":    "title links maxWidth gap",
			"footer":    "links lines background inset maxWidth cellPadding size lineHeight letterSpacing color linkColor separator gap after font",
			"html":      "html",
		}
		keys := make(map[string]map[string]bool, len(own))
		for typ, list := range own {
			set := wordSet("id type role " + list)
			if typ != "spacer" {
				set["padding"] = true
			}
			keys[typ] = set
		}
		return keys
	}()
)

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hexPattern      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	fontPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 -]{0,39}$`)
	spacingPattern  = regexp.MustCompile(`^-?[0-9]{1,2}(\.[0-9]{1,3})?em$`)
	builtinPattern  = regexp.MustCompile(`^builtin:[a-z]{1,40}$`)
	orgPattern      = regexp.MustCompile(`^org:[A-Za-z0-9_-]{1,160}$`)
	artifactPattern = regexp.MustCompile(`^artifact:[A-Za-z0-9_-]{1,160}$`)
)

// colourTokens are the colours a block may name: the eight of the theme and white.
var colourTokens = []string{"page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted", "white"}

// colourList reads "page, canvas, ..., muted or white".
var colourList = strings.Join(colourTokens[:len(colourTokens)-1], ", ") + " or " + colourTokens[len(colourTokens)-1]

func isColourToken(v string) bool {
	for _, token := range colourTokens {
		if v == token {
			return true
		}
	}
	return false
}

// hrefHint is the sentence for a link that is none of the allowed kinds.
var hrefHint = "a link must be a web address, an email address, a phone number or one of " +
	TagUnsubscribeURL + ", " + TagPreferencesURL + ", " + TagViewURL + " or " + TagConfirmURL

const (
	hostTagHint = "a web address cannot take a merge tag in its host, user name or port; put merge tags in the path or query"
	mailTagHint = "email and phone links cannot hold a merge tag"
	// A valid address percent-encodes these, so one in a link is a mistake or
	// an attempt to leave the attribute the link is written in.
	encodedCharsHint = "a link cannot contain quotes, angle brackets, backticks or backslashes; percent-encode them"
)

// hrefProblem says what is wrong with a link, or "" when it is allowed. A link
// is empty (when allowEmpty), a web address (http:// or https://), an email
// address (mailto:), a phone number (tel:) or exactly one of the four link
// merge tags. It is at most 2000 bytes and holds no space, control character,
// quote, angle bracket, backtick or backslash.
//
// A merge tag is replaced with a value a recipient supplied, and that value
// must never choose where a link goes. So no other merge tag may stand where
// the site is chosen: a web address takes merge tags in its path, query and
// fragment ("https://example.com/?e={{email}}") but not in its host, user name
// or port, and an email or phone link takes none.
func hrefProblem(href string, allowEmpty bool) string {
	if href == "" {
		if allowEmpty {
			return ""
		}
		return hrefHint
	}
	for _, tag := range linkTags {
		if href == tag {
			return ""
		}
	}
	if len(href) > maxHrefBytes {
		return "a link can be at most 2000 bytes"
	}
	if strings.IndexFunc(href, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "a link cannot contain spaces or control characters"
	}
	if strings.ContainsAny(href, "\"<>`\\") {
		return encodedCharsHint
	}
	for _, scheme := range []string{"http://", "https://"} {
		if hasScheme(href, scheme) {
			if strings.Contains(authority(href[len(scheme):]), "{{") {
				return hostTagHint
			}
			return ""
		}
	}
	for _, scheme := range []string{"mailto:", "tel:"} {
		if hasScheme(href, scheme) {
			if strings.Contains(href, "{{") {
				return mailTagHint
			}
			return ""
		}
	}
	return hrefHint
}

// hasScheme reports whether href starts with the scheme, in any case, and has
// something after it.
func hasScheme(href, scheme string) bool {
	return len(href) > len(scheme) && strings.EqualFold(href[:len(scheme)], scheme)
}

// authority is the part of a web address, after "http://", that names the site:
// the user name, host and port, up to the first "/", "?" or "#". A browser skips
// any further slashes after the two, so "https:////host/" is the site "host".
func authority(afterScheme string) string {
	rest := strings.TrimLeft(afterScheme, "/")
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		return rest[:end]
	}
	return rest
}

const pictureHint = "a picture must be a Library file (artifact:) or an embedded PNG, JPEG or GIF"

// pictureProblem says what is wrong with an image source, or "" when it is
// allowed. A source is empty (a placeholder the preview draws as a grey box),
// artifact:<id> (a Library file, resolved when the email is rendered) or a
// base64 data URI of a PNG, JPEG or GIF that decodes, is at most 1 MiB and
// matches the type it names. Anything else, a web address included, is refused:
// an email carries its pictures, it does not fetch them.
func pictureProblem(src string) string {
	if src == "" {
		return ""
	}
	if strings.HasPrefix(src, "artifact:") {
		if artifactPattern.MatchString(src) {
			return ""
		}
		return pictureHint
	}
	rest, ok := strings.CutPrefix(src, "data:image/")
	if !ok {
		return pictureHint
	}
	format, payload, ok := strings.Cut(rest, ";base64,")
	if !ok || (format != "png" && format != "jpeg" && format != "gif") {
		return pictureHint
	}
	if payload == "" {
		return "the embedded picture is empty"
	}
	if len(payload) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return "email image exceeds 1 MiB"
	}
	const notBase64 = "the embedded picture is not valid base64"
	if strings.ContainsAny(payload, "\r\n") {
		return notBase64
	}
	data, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil {
		return notBase64
	}
	if err := compose.EmailImageData(data, "image/"+format); err != nil {
		return err.Error()
	}
	return ""
}

// clip shortens s to at most limit characters for a message: an error that
// repeats a hostile 2 MiB id or tag name back would be a message nobody can read.
func clip(s string, limit int) string {
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

func quote(s string) string { return strconv.Quote(clip(s, 60)) }

func inRange(v, lo, hi float64) bool { return v >= lo && v <= hi }

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func rangeMessage(name string, lo, hi float64) string {
	return name + " must be between " + number(lo) + " and " + number(hi) + " pixels"
}

var (
	errNotObject     = errors.New("not an object")
	errNotList       = errors.New("not a list")
	errTooManyBlocks = errors.New("an email can have at most 300 blocks")
)

func isNull(raw json.RawMessage) bool { return bytes.Equal(raw, []byte("null")) }

// readObject splits a JSON object into its members, raw. duplicate is the first
// key the object repeats ("" when none): encoding/json keeps the last of two
// equal keys without a word, and a document that says two things about one
// field is refused, not resolved.
func readObject(raw json.RawMessage) (fields map[string]json.RawMessage, duplicate string, err error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, "", errNotObject
	}
	fields = map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, "", err
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, "", err
		}
		if _, seen := fields[key]; seen && duplicate == "" {
			duplicate = key
		}
		fields[key] = value
	}
	return fields, duplicate, nil
}

// readList splits a JSON array into its items, raw, reading at most limit of
// them; tooMany says there were more.
func readList(raw json.RawMessage, limit int) (items []json.RawMessage, tooMany bool, err error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, false, errNotList
	}
	for decoder.More() {
		if len(items) == limit {
			return items, true, nil
		}
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	return items, false, nil
}

// unknownKey returns the first key (in alphabetical order, so the message does
// not change from run to run) that is not allowed, or "".
func unknownKey(fields map[string]json.RawMessage, allowed map[string]bool) string {
	var unknown []string
	for key := range fields {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	return unknown[0]
}

// decodeFields decodes members whose keys have already been checked, exactly,
// into a struct.
func decodeFields(fields map[string]json.RawMessage, dst any) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

// decodeMessage turns a decoding error into a sentence about the field.
func decodeMessage(err error) string {
	var wrong *json.UnmarshalTypeError
	if errors.As(err, &wrong) && wrong.Field != "" {
		return wrong.Field + " must be " + describeType(wrong.Type)
	}
	return err.Error()
}

func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "a whole number"
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "a list of text"
		}
		return "a list"
	case reflect.Pointer:
		return describeType(t.Elem())
	}
	return "a different kind of value"
}

// textOf reads a member that must be a string; an absent or null member is "".
func textOf(raw json.RawMessage) (string, error) {
	var s string
	if len(raw) == 0 {
		return "", nil
	}
	err := json.Unmarshal(raw, &s)
	return s, err
}

// Parse reads and validates an email document.
//
// The document is exactly one of two modes: a layout (blocks) or custom HTML
// (textBody and htmlBody). Every key, value, link, picture and piece of markup
// is checked, and the first thing wrong is returned as a sentence that names
// it, with the block's id when it is in a block. Parse never repairs a
// document and never returns a partly valid one.
func Parse(data []byte) (Document, error) {
	if len(data) > MaxBytes {
		return Document{}, errors.New("email document exceeds 2 MiB")
	}
	fields, err := readDocument(data)
	if err != nil {
		return Document{}, err
	}
	return parseDocument(fields)
}

// readDocument reads the one JSON object the file is, and nothing after it.
func readDocument(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("email document is empty")
		}
		return nil, fmt.Errorf("email document is not valid JSON: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("email document contains trailing content")
	}
	fields, duplicate, err := readObject(raw)
	if err != nil {
		return nil, errors.New("email document must be a JSON object")
	}
	if duplicate != "" {
		return nil, fmt.Errorf("duplicate field %s", quote(duplicate))
	}
	return fields, nil
}

func parseDocument(fields map[string]json.RawMessage) (Document, error) {
	_, hasBlocks := fields["blocks"]
	_, hasText := fields["textBody"]
	_, hasHTML := fields["htmlBody"]
	custom := hasText || hasHTML
	var allowed map[string]bool
	switch {
	case hasBlocks && custom:
		return Document{}, errors.New("an email document is either a layout (blocks) or custom HTML (textBody and htmlBody), not both")
	case hasBlocks:
		allowed = layoutKeys
	case custom:
		allowed = customKeys
	default:
		return Document{}, errors.New("an email document needs blocks (a layout) or textBody and htmlBody (custom HTML)")
	}
	if key := unknownKey(fields, allowed); key != "" {
		return Document{}, fmt.Errorf("unknown field %s", quote(key))
	}

	var d Document
	for _, member := range []struct {
		key string
		dst *string
	}{{"subject", &d.Subject}, {"preheader", &d.Preheader}, {"layout", &d.Layout}, {"textBody", &d.TextBody}, {"htmlBody", &d.HTMLBody}} {
		raw, present := fields[member.key]
		if !present {
			continue
		}
		if err := json.Unmarshal(raw, member.dst); err != nil {
			return Document{}, fmt.Errorf("%s must be text", member.key)
		}
	}
	if strings.TrimSpace(d.Subject) == "" || strings.IndexFunc(d.Subject, unicode.IsControl) >= 0 || len(d.Subject) > 998 {
		return Document{}, errors.New("email subject must be a nonempty single line under 999 bytes")
	}
	if utf8.RuneCountInString(d.Preheader) > 200 || strings.IndexFunc(d.Preheader, unicode.IsControl) >= 0 {
		return Document{}, errors.New("the preheader must be one line of at most 200 characters")
	}

	if !hasBlocks {
		if strings.TrimSpace(d.TextBody) == "" {
			return Document{}, errors.New("email needs a plain-text body")
		}
		return d, nil
	}
	if d.Layout != "" && !builtinPattern.MatchString(d.Layout) && !orgPattern.MatchString(d.Layout) {
		return Document{}, errors.New("layout must be empty, builtin:<name> or org:<id>")
	}
	theme, err := parseTheme(fields["theme"])
	if err != nil {
		return Document{}, err
	}
	d.Theme = theme
	if isNull(fields["blocks"]) {
		return Document{}, errors.New("blocks must be a list of blocks")
	}
	p := &parser{seen: map[string]bool{}}
	blocks, err := p.children(fields["blocks"], inTop, "")
	if err != nil {
		return Document{}, err
	}
	d.Blocks = blocks
	return d, nil
}

func parseTheme(raw json.RawMessage) (Theme, error) {
	if len(raw) == 0 || isNull(raw) {
		return Theme{}, errors.New("theme is required in a layout: the colours page, canvas, primary, secondary, accent, tint, text and muted, and the fonts headingFont and bodyFont")
	}
	fields, duplicate, err := readObject(raw)
	if err != nil {
		return Theme{}, errors.New("theme must be an object")
	}
	if duplicate != "" {
		return Theme{}, fmt.Errorf("theme: duplicate field %s", quote(duplicate))
	}
	if key := unknownKey(fields, themeKeys); key != "" {
		return Theme{}, fmt.Errorf("theme: unknown field %s", quote(key))
	}
	var theme Theme
	if err := decodeFields(fields, &theme); err != nil {
		return Theme{}, fmt.Errorf("theme: %s", decodeMessage(err))
	}
	for _, colour := range []struct{ name, value string }{
		{"page", theme.Page}, {"canvas", theme.Canvas}, {"primary", theme.Primary}, {"secondary", theme.Secondary},
		{"accent", theme.Accent}, {"tint", theme.Tint}, {"text", theme.Text}, {"muted", theme.Muted},
	} {
		if !hexPattern.MatchString(colour.value) {
			return Theme{}, fmt.Errorf("theme: %s must be a #rrggbb colour", colour.name)
		}
	}
	for _, font := range []struct{ name, value string }{{"headingFont", theme.HeadingFont}, {"bodyFont", theme.BodyFont}} {
		if !fontPattern.MatchString(font.value) {
			return Theme{}, fmt.Errorf("theme: %s must be a font family name", font.name)
		}
	}
	return theme, nil
}

// container says where a block sits, which decides what it may hold.
type container int

const (
	inTop container = iota
	inSection
	inColumn
)

// misplaced says why a block of this type cannot sit in this container. A
// section is a top-level band, columns sit at the top level or in a section, a
// footer is the last thing in the email, and nothing else nests.
func misplaced(in container, typ string) string {
	switch {
	case typ == typeFooter && in != inTop:
		return "a footer belongs at the top level"
	case typ == typeSection && in == inSection:
		return "a section cannot contain another section"
	case typ == typeSection && in == inColumn:
		return "a column cannot contain a section"
	case typ == typeColumns && in == inColumn:
		return "columns cannot contain columns"
	}
	return ""
}

// parser walks the blocks of one document, which share one set of ids and one
// count.
type parser struct {
	seen  map[string]bool
	count int
}

// children reads a list of blocks. where names the owner of the list for the
// messages ("" at the top level). An absent list is no blocks (nil); a present
// one is never nil, so an empty layout is still a layout.
func (p *parser) children(raw json.RawMessage, in container, where string) ([]Block, error) {
	if len(raw) == 0 || (in != inTop && isNull(raw)) {
		return nil, nil
	}
	list, tooMany, err := readList(raw, MaxBlocks)
	if err != nil {
		prefix := ""
		if where != "" {
			prefix = where + ": "
		}
		return nil, errors.New(prefix + "blocks must be a list of blocks")
	}
	if tooMany {
		return nil, errTooManyBlocks
	}
	blocks := make([]Block, 0, len(list))
	for _, item := range list {
		b, err := p.block(item, in)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

func (p *parser) block(raw json.RawMessage, in container) (Block, error) {
	p.count++
	if p.count > MaxBlocks {
		return Block{}, errTooManyBlocks
	}
	fields, duplicate, err := readObject(raw)
	if err != nil {
		return Block{}, errors.New("a block must be an object")
	}
	id, err := textOf(fields["id"])
	if err != nil {
		return Block{}, errors.New("a block id must be text")
	}
	if !idPattern.MatchString(id) {
		return Block{}, fmt.Errorf("block id %s must be lowercase letters, digits and hyphens (1 to 64 characters, starting with a letter or digit)", quote(id))
	}
	if p.seen[id] {
		return Block{}, fmt.Errorf("block id %s is used twice", quote(id))
	}
	p.seen[id] = true
	where := "block " + quote(id)

	typ, err := textOf(fields["type"])
	if err != nil {
		return Block{}, fmt.Errorf("%s: type must be text", where)
	}
	allowed, known := blockKeys[typ]
	if !known {
		return Block{}, fmt.Errorf("%s: unknown block type %s", where, quote(typ))
	}
	if problem := misplaced(in, typ); problem != "" {
		return Block{}, fmt.Errorf("%s: %s", where, problem)
	}
	if duplicate != "" {
		return Block{}, fmt.Errorf("%s: duplicate field %s", where, quote(duplicate))
	}
	if key := unknownKey(fields, allowed); key != "" {
		return Block{}, fmt.Errorf("%s: unknown field %s", where, quote(key))
	}

	// The members that hold structure are read by hand below; everything else
	// is a plain value of the Block struct.
	plain := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		switch key {
		case "padding", "inset", "blocks", "columns", "links":
		default:
			plain[key] = value
		}
	}
	var b Block
	if err := decodeFields(plain, &b); err != nil {
		return Block{}, fmt.Errorf("%s: %s", where, decodeMessage(err))
	}
	if b.Padding, err = fourLengths(fields["padding"], "padding"); err != nil {
		return Block{}, fmt.Errorf("%s: %w", where, err)
	}
	if b.Inset, err = fourLengths(fields["inset"], "inset"); err != nil {
		return Block{}, fmt.Errorf("%s: %w", where, err)
	}
	if problem := blockProblem(&b); problem != "" {
		return Block{}, fmt.Errorf("%s: %s", where, problem)
	}
	if len(b.Lines) == 0 {
		b.Lines = nil
	}

	switch b.Type {
	case typeSection:
		if b.Blocks, err = p.children(fields["blocks"], inSection, where); err != nil {
			return Block{}, err
		}
		if len(b.Blocks) == 0 {
			b.Blocks = nil
		}
	case typeColumns:
		if b.Columns, err = p.columns(fields["columns"], where); err != nil {
			return Block{}, err
		}
	case typeSocial:
		if b.Links, err = readLinks(fields["links"], where, socialLinkKeys, 8, "social needs 1 to 8 links", socialLinkProblem); err != nil {
			return Block{}, err
		}
		if len(b.Links) == 0 {
			return Block{}, fmt.Errorf("%s: social needs 1 to 8 links", where)
		}
	case typeFooter:
		if b.Links, err = readLinks(fields["links"], where, footerLinkKeys, 8, "a footer can have at most 8 links", footerLinkProblem); err != nil {
			return Block{}, err
		}
		if len(b.Links) == 0 {
			b.Links = nil
		}
	}
	return b, nil
}

// fourLengths reads padding or inset: four numbers, top, right, bottom, left.
func fourLengths(raw json.RawMessage, name string) (*[4]float64, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil || len(values) != 4 {
		return nil, fmt.Errorf("%s must be four numbers (top, right, bottom, left)", name)
	}
	var lengths [4]float64
	for i, v := range values {
		if !inRange(v, 0, 200) {
			return nil, fmt.Errorf("%s values must be between 0 and 200 pixels", name)
		}
		lengths[i] = v
	}
	return &lengths, nil
}

// blockProblem checks the values of one block, whatever its type, and says
// what is wrong with the first that is, or "" when all are fine. Zero is an
// absent value, except where noted. Links, columns and nested blocks are read
// by the caller.
func blockProblem(b *Block) string {
	switch b.Role {
	case "", "brand", "content":
	default:
		return `role must be "brand" or "content"`
	}
	for _, colour := range []struct{ name, value string }{{"color", b.Color}, {"textColor", b.TextColor}, {"linkColor", b.LinkColor}, {"background", b.Background}} {
		if colour.value != "" && !isColourToken(colour.value) {
			return colour.name + " must name a theme colour (" + colourList + ")"
		}
	}
	switch b.Align {
	case "", "left", "center", "right":
	default:
		return "align must be left, center or right"
	}
	if b.Weight != 0 && b.Weight != 400 && b.Weight != 700 {
		return "weight must be 400 or 700"
	}
	switch b.Font {
	case "", "heading", "body", "base":
	default:
		return "font must be heading, body or base"
	}
	if b.LetterSpacing != "" && !spacingPattern.MatchString(b.LetterSpacing) {
		return "letterSpacing must be a length in em, such as 0.05em or -0.02em"
	}
	if b.Size != 0 && !inRange(b.Size, 8, 96) {
		return rangeMessage("size", 8, 96)
	}
	for _, length := range []struct {
		name  string
		value float64
		limit float64
	}{
		{"lineHeight", b.LineHeight, 200}, {"listLineHeight", b.ListLineHeight, 200}, {"after", b.After, 200}, {"paddingY", b.PaddingY, 200},
		{"height", b.Height, 2000}, {"maxWidth", b.MaxWidth, 2000}, {"stackBelow", b.StackBelow, 2000},
	} {
		if !inRange(length.value, 0, length.limit) {
			return rangeMessage(length.name, 0, length.limit)
		}
	}
	for _, length := range []struct {
		name  string
		value *float64
	}{{"gap", b.Gap}, {"cellPadding", b.CellPadding}} {
		if length.value != nil && !inRange(*length.value, 0, 200) {
			return rangeMessage(length.name, 0, 200)
		}
	}

	switch b.Type {
	case typeImage:
		if b.Width < 1 || b.Width > 600 {
			return rangeMessage("width", 1, 600)
		}
		if problem := pictureProblem(b.Src); problem != "" {
			return problem
		}
		return hrefProblem(b.Href, true)
	case typeButton:
		if n := len(b.Label); n < 1 || n > 200 {
			return "label must be 1 to 200 bytes"
		}
		if b.Width < 0 || b.Width > 600 {
			return rangeMessage("width", 0, 600)
		}
		return hrefProblem(b.Href, true)
	case typeText:
		return markdownProblem(b.Text)
	case typeHTML:
		if len(b.HTML) > maxHTMLBytes {
			return "an html block can be at most 64 KiB"
		}
		// The Document keeps the author's markup as written; what the renderer
		// puts in the email is safeMarkup of it.
		if _, err := safeMarkup(b.HTML); err != nil {
			return err.Error()
		}
	case typeFooter:
		if len(b.Lines) > 12 {
			return "a footer can have at most 12 lines"
		}
		for n, line := range b.Lines {
			if utf8.RuneCountInString(line) > 300 {
				return fmt.Sprintf("line %d is longer than 300 characters", n+1)
			}
		}
		if b.Separator != nil && len(*b.Separator) > 40 {
			return "separator must be at most 40 bytes"
		}
	}
	return ""
}

// markdownProblem reads the Markdown of a text block and applies the link rule
// to every link in it. A link in Markdown always has an address.
func markdownProblem(text string) string {
	elements, err := parseMarkdown(text)
	if err != nil {
		return err.Error()
	}
	linkProblem := func(inline []mdInline) string {
		for _, node := range inline {
			if node.Kind == mdLink {
				if problem := hrefProblem(node.URL, false); problem != "" {
					return problem
				}
			}
		}
		return ""
	}
	for _, element := range elements {
		if problem := linkProblem(element.Inline); problem != "" {
			return problem
		}
		for _, item := range element.Items {
			if problem := linkProblem(item.Inline); problem != "" {
				return problem
			}
		}
	}
	return ""
}

// columns reads the columns of a columns block: one to four, each a width in
// percent (together at most 100, give or take rounding) and a list of blocks.
func (p *parser) columns(raw json.RawMessage, where string) ([]Column, error) {
	list, tooMany, err := readList(raw, 5)
	if err != nil || tooMany || len(list) == 0 || len(list) > 4 {
		return nil, fmt.Errorf("%s: columns needs 1 to 4 columns", where)
	}
	columns := make([]Column, 0, len(list))
	total := 0.0
	for n, item := range list {
		at := fmt.Sprintf("%s: column %d", where, n+1)
		fields, duplicate, err := readObject(item)
		if err != nil {
			return nil, fmt.Errorf("%s must be an object", at)
		}
		if duplicate != "" {
			return nil, fmt.Errorf("%s: duplicate field %s", at, quote(duplicate))
		}
		if key := unknownKey(fields, columnKeys); key != "" {
			return nil, fmt.Errorf("%s: unknown field %s", at, quote(key))
		}
		plain := make(map[string]json.RawMessage, len(fields))
		for key, value := range fields {
			if key != "blocks" {
				plain[key] = value
			}
		}
		var column Column
		if err := decodeFields(plain, &column); err != nil {
			return nil, fmt.Errorf("%s: %s", at, decodeMessage(err))
		}
		if column.Width <= 0 {
			return nil, fmt.Errorf("%s: width must be greater than 0 percent", at)
		}
		if column.CellPadding != nil && !inRange(*column.CellPadding, 0, 200) {
			return nil, fmt.Errorf("%s: %s", at, rangeMessage("cellPadding", 0, 200))
		}
		if column.Blocks, err = p.children(fields["blocks"], inColumn, at); err != nil {
			return nil, err
		}
		if column.Blocks == nil {
			column.Blocks = []Block{}
		}
		total += column.Width
		columns = append(columns, column)
	}
	if total > 100.01 {
		return nil, fmt.Errorf("%s: column widths add up to more than 100 percent", where)
	}
	return columns, nil
}

// readLinks reads the links of a social or footer block: at most limit of them,
// each an object with only the keys of its kind, each checked by check.
func readLinks(raw json.RawMessage, where string, keys map[string]bool, limit int, tooManyMessage string, check func(Link) string) ([]Link, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	list, tooMany, err := readList(raw, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: links must be a list of links", where)
	}
	if tooMany {
		return nil, fmt.Errorf("%s: %s", where, tooManyMessage)
	}
	links := make([]Link, 0, len(list))
	for n, item := range list {
		at := fmt.Sprintf("%s: link %d", where, n+1)
		fields, duplicate, err := readObject(item)
		if err != nil {
			return nil, fmt.Errorf("%s must be an object", at)
		}
		if duplicate != "" {
			return nil, fmt.Errorf("%s: duplicate field %s", at, quote(duplicate))
		}
		if key := unknownKey(fields, keys); key != "" {
			return nil, fmt.Errorf("%s: unknown field %s", at, quote(key))
		}
		var link Link
		if err := decodeFields(fields, &link); err != nil {
			return nil, fmt.Errorf("%s: %s", at, decodeMessage(err))
		}
		if problem := check(link); problem != "" {
			return nil, fmt.Errorf("%s: %s", at, problem)
		}
		links = append(links, link)
	}
	return links, nil
}

func socialLinkProblem(l Link) string {
	if n := len(l.Network); n < 1 || n > 40 {
		return "network must be 1 to 40 bytes"
	}
	if problem := hrefProblem(l.Href, true); problem != "" {
		return problem
	}
	if problem := pictureProblem(l.Icon); problem != "" {
		return problem
	}
	if l.Width < 0 || l.Width > 600 {
		return rangeMessage("width", 0, 600)
	}
	return ""
}

func footerLinkProblem(l Link) string {
	if n := utf8.RuneCountInString(l.Label); n < 1 || n > 80 {
		return "label must be 1 to 80 characters"
	}
	return hrefProblem(l.Href, true)
}

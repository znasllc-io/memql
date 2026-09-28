// Package docsmd reads the repository's markdown documentation the way both
// the docs gates and the docs bundle need to: the front-matter block, the
// links on a page with their byte offsets, the HTML comments outside code,
// and a page's one-paragraph description.
//
// It exists so the code that decides what memql.io publishes and the code the
// root gates run are the same code (memql#5717). Before it, four independent
// front-matter readers disagreed on quoting: the gate trimmed the quotes from
// `audience: "public"` and passed the page, while the bundler compared the
// quoted string and silently left the page out. A reader, a gate and a
// bundler that share one parser cannot drift into disagreeing about what a
// page says.
//
// Standard library only, in the core module, on the core/maketargets
// pattern: the root package's gates and cmd/docs-gen/bundle both import it.
package docsmd

import "strings"

// Split separates a page into its front-matter block and its body.
//
// front is the block exactly as written, from the opening `---` line through
// the closing `---` line and its line ending; body is everything after it.
// ok is false when content does not open with a `---` line or the block never
// closes, and then front is empty and body is the whole content.
func Split(content string) (front, body string, ok bool) {
	first, rest, found := strings.Cut(content, "\n")
	if !found || strings.TrimRight(first, "\r") != "---" {
		return "", content, false
	}
	offset := len(first) + 1
	for len(rest) > 0 {
		line, tail, more := strings.Cut(rest, "\n")
		end := offset + len(line)
		if more {
			end++
		}
		if strings.TrimRight(line, "\r") == "---" {
			return content[:end], content[end:], true
		}
		offset = end
		rest = tail
		if !more {
			break
		}
	}
	return "", content, false
}

// Parse reads the front-matter block as the docs gates always have: one
// `key: value` per line, split on the first colon, both sides trimmed of
// space and the value trimmed of surrounding quotes, so `audience: "public"`
// and `audience: public` read the same. A line with no colon is ignored. It
// deliberately does not pull in a YAML library: the contract is a flat set of
// scalar keys, and a real YAML parser would accept nested structures the
// standard does not define.
//
// ok is false when content has no closed block; fm is then nil and body is
// the whole content.
func Parse(content string) (fm map[string]string, body string, ok bool) {
	front, body, ok := Split(content)
	if !ok {
		return nil, content, false
	}
	fm = map[string]string{}
	lines := strings.Split(front, "\n")
	// The first line is the opening `---`; the last non-empty one the closing.
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")
		if line == "---" {
			break
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		if key != "" {
			fm[key] = val
		}
	}
	return fm, body, true
}

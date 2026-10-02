package compose

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	pure "github.com/znasllc-io/memql/component/compose"
	"golang.org/x/net/html"
)

// EmbedEmailAssets resolves model-visible asset handles against the captured,
// authorized input bytes. Reference-only images never enter this allowlist.
// The output carries its image bytes, so revisions and scheduled campaigns do
// not depend on a mutable Library file or an expiring authenticated URL.
func EmbedEmailAssets(source string, references []Resolved) (string, error) {
	draft, err := pure.ParseEmailTemplate(source)
	if err != nil {
		return "", err
	}
	if err := validateGeneratedEmailMarkup(draft.HTMLBody); err != nil {
		return "", err
	}
	assets := map[string]SourceContent{}
	embedded := map[string]bool{}
	for _, reference := range references {
		if !reference.Ref.Content || !reference.Ref.IncludeImages {
			continue
		}
		for _, file := range reference.Files {
			if len(file.Image) > 0 {
				assets["memql-asset:"+(pure.Result{Bytes: file.Image}).SHA256()] = file
				embedded["data:"+file.MimeType+";base64,"+base64.StdEncoding.EncodeToString(file.Image)] = true
			}
		}
	}
	draft.HTMLBody, err = pure.RewriteEmailImageSources(draft.HTMLBody, func(src string) (string, error) {
		if !strings.HasPrefix(src, "memql-asset:") {
			if src == "" || embedded[src] {
				return src, nil
			}
			return "", fmt.Errorf("generated email images must use the files explicitly selected for inclusion")
		}
		file, ok := assets[src]
		if !ok {
			return "", fmt.Errorf("the draft used an image that was not selected for inclusion")
		}
		if err := pure.EmailImageData(file.Image, file.MimeType); err != nil {
			return "", err
		}
		return "data:" + file.MimeType + ";base64," + base64.StdEncoding.EncodeToString(file.Image), nil
	})
	if err != nil {
		return "", err
	}
	if _, _, err := pure.ExtractEmailImages(draft.HTMLBody); err != nil {
		return "", err
	}
	data, err := json.Marshal(draft)
	if err != nil {
		return "", err
	}
	if _, err := pure.ParseEmailTemplate(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

// Generated drafts use inline styles and img elements. Restrict alternate
// resource channels too: validating img.src alone would let an unselected
// image escape through srcset, CSS, SVG or Outlook conditional markup.
// This applies to generated drafts, not the user's general HTML editor.
func validateGeneratedEmailMarkup(source string) error {
	allowed := map[string]bool{}
	for _, tag := range strings.Fields("html head body title div span p br hr a img table thead tbody tfoot tr td th caption colgroup col h1 h2 h3 h4 h5 h6 strong b em i u s small sup sub blockquote pre code ul ol li center font") {
		allowed[tag] = true
	}
	attributes := map[string]bool{}
	for _, name := range strings.Fields("style class id title lang dir role aria-label aria-hidden align valign bgcolor width height border cellpadding cellspacing colspan rowspan scope face color size target rel href src alt") {
		attributes[name] = true
	}
	z := html.NewTokenizer(strings.NewReader(source))
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() == io.EOF {
				return nil
			}
			return z.Err()
		}
		if kind == html.CommentToken && strings.Contains(z.Token().Data, "<") {
			return fmt.Errorf("generated email cannot contain hidden conditional markup")
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		if !allowed[token.Data] {
			return fmt.Errorf("generated email uses unsupported markup; use email tables, inline styles and selected img assets")
		}
		for _, attr := range token.Attr {
			name, value := strings.ToLower(attr.Key), strings.ToLower(strings.TrimSpace(attr.Val))
			if !attributes[name] || (name == "src" && token.Data != "img") || (name == "href" && token.Data != "a") {
				return fmt.Errorf("generated email uses an unsupported resource or active attribute")
			}
			if name == "style" {
				// Backslashes and comments can conceal CSS resource functions;
				// imports and style blocks are not part of inline email styling.
				for _, forbidden := range []string{"\\", "/*", "@", "url", "image(", "image-set", "expression", "behavior", "-moz-binding"} {
					if strings.Contains(value, forbidden) {
						return fmt.Errorf("generated email styles cannot load images; use selected img assets")
					}
				}
			}
			if name == "href" && value != "" && !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "mailto:") && !strings.HasPrefix(value, "{{") {
				return fmt.Errorf("generated email links must use a web address, email address, or merge field")
			}
		}
	}
}

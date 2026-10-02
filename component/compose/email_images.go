package compose

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// EmailImage is a local raster carried by an editable template. Sending uses
// inline MIME/CID attachments, never private storage URLs or a public upload.
type EmailImage struct {
	ContentID string
	MIMEType  string
	Data      []byte
}

// RewriteEmailImageSources preserves the original HTML except for the img
// start tags whose src changes. No URL is fetched and no document is executed.
func RewriteEmailImageSources(source string, resolve func(string) (string, error)) (string, error) {
	if len(source) > 2<<20 {
		return "", fmt.Errorf("email HTML exceeds 2 MiB")
	}
	z := html.NewTokenizer(strings.NewReader(source))
	var output strings.Builder
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() == io.EOF {
				return output.String(), nil
			}
			return "", z.Err()
		}
		raw := string(z.Raw())
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			token := z.Token()
			if token.Data == "img" {
				changed, seen := false, false
				for n := range token.Attr {
					if token.Attr[n].Key != "src" {
						continue
					}
					if seen {
						return "", fmt.Errorf("email image has duplicate src attributes")
					}
					seen = true
					next, err := resolve(token.Attr[n].Val)
					if err != nil {
						return "", err
					}
					changed = changed || next != token.Attr[n].Val
					token.Attr[n].Val = next
				}
				if changed {
					raw = token.String()
				}
			}
		}
		output.WriteString(raw)
	}
}

// EmailImageData validates bounded PNG/JPEG/GIF bytes before they become a
// data URI or attachment. SVG and arbitrary data MIME types are never assets.
func EmailImageData(data []byte, mime string) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return fmt.Errorf("email image exceeds 1 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") || mime != "image/"+format {
		return fmt.Errorf("email image must contain valid PNG, JPEG, or GIF bytes")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 24_000_000 {
		return fmt.Errorf("email image exceeds 24 million pixels")
	}
	return nil
}

// ExtractEmailImages converts only embedded raster img sources to CID. It
// preserves ordinary HTML and leaves external image URLs unchanged.
func ExtractEmailImages(source string) (string, []EmailImage, error) {
	var images []EmailImage
	seen := map[string]string{}
	total := 0
	body, err := RewriteEmailImageSources(source, func(src string) (string, error) {
		normalized := strings.ToLower(strings.TrimSpace(src))
		if strings.HasPrefix(normalized, "memql-asset:") || strings.HasPrefix(normalized, "cid:") {
			return "", fmt.Errorf("email contains an unresolved image asset")
		}
		if !strings.HasPrefix(normalized, "data:") {
			return src, nil
		}
		src = strings.TrimSpace(src)
		if cid, ok := seen[src]; ok {
			return "cid:" + cid, nil
		}
		prefix, encoded, ok := strings.Cut(src, ",")
		if !ok || !strings.HasSuffix(prefix, ";base64") || strings.ContainsAny(encoded, "\r\n") {
			return "", fmt.Errorf("email image must be base64 raster content")
		}
		mime := strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64")
		if len(encoded) > base64.StdEncoding.EncodedLen(1<<20) {
			return "", fmt.Errorf("email image exceeds 1 MiB")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("email image has invalid base64 content")
		}
		if err := EmailImageData(data, mime); err != nil {
			return "", err
		}
		total += len(data)
		if len(images) >= 8 || total > 1<<20 {
			return "", fmt.Errorf("use up to eight email images totaling 1 MiB")
		}
		cid := fmt.Sprintf("image-%d@memql", len(images)+1)
		seen[src] = cid
		images = append(images, EmailImage{ContentID: cid, MIMEType: mime, Data: data})
		return "cid:" + cid, nil
	})
	return body, images, err
}

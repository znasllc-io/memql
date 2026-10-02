package compose

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	pure "github.com/znasllc-io/memql/component/compose"
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

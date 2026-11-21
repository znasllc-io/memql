package calls

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/jpeg"
	"image/png"
	"net/url"
	"regexp"
	"strings"
)

const PortraitSourceBytes = 40 * 1024
const PortraitCanonicalBytes = 1024 * 1024
const PortraitMaxSide = 512

var portraitDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type PortraitInfo struct {
	ID       string `json:"id"`
	Digest   string `json:"digest"`
	MimeType string `json:"mimeType"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type PortraitImage struct {
	Portrait PortraitInfo `json:"portrait"`
	Data     []byte       `json:"data"` // JSON encodes this as base64; never a URL to fetch.
}

func ValidPortraitReference(id, digest string) bool {
	return id == "" && digest == "" || ValidWorkflowID(id) && portraitDigestPattern.MatchString(digest)
}

// CanonicalPortrait accepts only bounded raster data. Header dimensions are
// checked before decoding pixels, then re-encoding strips metadata and trailing
// payloads. This is active-content sanitation, not a visual-content classifier.
func CanonicalPortrait(encoded string) (PortraitImage, error) {
	var out PortraitImage
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(PortraitSourceBytes) || strings.ContainsAny(encoded, "\r\n") {
		return out, ErrInvalid
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > PortraitSourceBytes {
		return out, ErrInvalid
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > PortraitMaxSide || config.Height > PortraitMaxSide {
		return out, ErrInvalid
	}
	pixels, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil || pixels.Bounds().Dx() != config.Width || pixels.Bounds().Dy() != config.Height {
		return out, ErrInvalid
	}
	var canonical bytes.Buffer
	if format == "png" {
		err = png.Encode(&canonical, pixels)
	} else {
		err = jpeg.Encode(&canonical, pixels, &jpeg.Options{Quality: 85})
	}
	if err != nil || canonical.Len() > PortraitCanonicalBytes {
		return out, ErrInvalid
	}
	out.Data = canonical.Bytes()
	out.Portrait = PortraitInfo{Digest: PortraitContentDigest(out.Data), MimeType: "image/" + format, Width: config.Width, Height: config.Height}
	return out, nil
}

// SHA-256 is the public content-integrity contract for approved portrait bytes.
func PortraitContentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The session route forwards only this non-secret identifier. Unknown query
// keys still follow the ordinary session route's historical discard behavior;
// a request containing portrait must be exactly one validated selector.
func PortraitQuery(raw string) (string, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", ErrInvalid
	}
	values, found := q["portrait"]
	if !found {
		return "", nil
	}
	if len(raw) > 160 || len(q) != 1 || len(values) != 1 || !ValidWorkflowID(values[0]) {
		return "", ErrInvalid
	}
	return values[0], nil
}

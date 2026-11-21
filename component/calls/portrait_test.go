package calls

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPortraitDecodesBoundedRasterAndRemovesNonPixelPayloads(t *testing.T) {
	pixels := image.NewRGBA(image.Rect(0, 0, 32, 32))
	pixels.Set(1, 1, color.RGBA{200, 10, 40, 255})
	for _, format := range []string{"png", "jpeg"} {
		var raw bytes.Buffer
		if format == "png" {
			require.NoError(t, png.Encode(&raw, pixels))
		} else {
			require.NoError(t, jpeg.Encode(&raw, pixels, nil))
		}
		raw.WriteString("trailing-private-metadata")
		out, err := CanonicalPortrait(base64.StdEncoding.EncodeToString(raw.Bytes()))
		require.NoError(t, err)
		require.Equal(t, 32, out.Portrait.Width)
		require.Equal(t, "image/"+format, out.Portrait.MimeType)
		require.Equal(t, PortraitContentDigest(out.Data), out.Portrait.Digest)
		require.NotContains(t, string(out.Data), "trailing-private-metadata")
		_, decoded, err := image.Decode(bytes.NewReader(out.Data))
		require.NoError(t, err)
		require.Equal(t, format, decoded)
	}
}
func TestPortraitRejectsUnsupportedOversizedAndMalformedImages(t *testing.T) {
	var large, gifImage bytes.Buffer
	require.NoError(t, png.Encode(&large, image.NewRGBA(image.Rect(0, 0, 513, 1))))
	require.NoError(t, gif.Encode(&gifImage, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	for _, raw := range [][]byte{nil, []byte(`<svg onload="alert(1)"/>`), large.Bytes(), gifImage.Bytes(), []byte("broken png"), bytes.Repeat([]byte{1}, PortraitSourceBytes+1)} {
		_, err := CanonicalPortrait(base64.StdEncoding.EncodeToString(raw))
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, err := CanonicalPortrait("data:image/png;base64,AA==")
	require.ErrorIs(t, err, ErrInvalid)
}
func TestPortraitQueryForwardsOnlyOneBoundedNonSecretSelector(t *testing.T) {
	id, err := PortraitQuery("portrait=abc_123")
	require.NoError(t, err)
	require.Equal(t, "abc_123", id)
	for _, raw := range []string{"portrait=", "portrait=a&portrait=b", "portrait=a&token=secret", "portrait=..%2Fasset", "portrait=https%3A%2F%2Fevil.test", "portrait=" + strings.Repeat("a", 129)} {
		_, err = PortraitQuery(raw)
		require.ErrorIs(t, err, ErrInvalid)
	}
	id, err = PortraitQuery("token=discard-me")
	require.NoError(t, err)
	require.Empty(t, id)
}

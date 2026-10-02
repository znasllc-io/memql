package email

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func inlinePNG(t *testing.T) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	return data, "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func TestInlineImageWireCarriesReviewedBytesAndReadableAlternatives(t *testing.T) {
	data, uri := inlinePNG(t)
	msg := Message{To: "reader@example.test", Subject: "Client update", TextBody: "Welcome", HTMLBody: `<p>Welcome</p><img src="` + uri + `" alt="Client logo"><img src="` + uri + `" alt="Again">`}
	wire, err := RenderRFC5322("Client <news@client.test>", msg)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := mail.ReadMessage(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err := mime.ParseMediaType(envelope.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/related" {
		t.Fatal(kind, err)
	}
	parts := multipart.NewReader(envelope.Body, params["boundary"])
	alternatives, err := parts.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err = mime.ParseMediaType(alternatives.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/alternative" {
		t.Fatal(kind, err)
	}
	inner := multipart.NewReader(alternatives, params["boundary"])
	plain, err := inner.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(plain)
	if string(text) != "Welcome" {
		t.Fatalf("lost plain text: %s", text)
	}
	html, err := inner.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(html)
	if strings.Contains(string(body), "data:") || strings.Count(string(body), "cid:image-1@memql") != 2 {
		t.Fatalf("bad HTML: %s", body)
	}
	asset, err := parts.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if asset.Header.Get("Content-ID") != "<image-1@memql>" || asset.Header.Get("Content-Disposition") != "inline" {
		t.Fatal("lost inline identity")
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, asset))
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatal("image bytes changed", err)
	}
	if _, err = parts.NextPart(); err != io.EOF {
		t.Fatal("duplicate image was attached twice", err)
	}

}

func TestInlineImageRefusesUnresolvedOrActiveAssetBeforeSending(t *testing.T) {
	for _, src := range []string{"memql-asset:missing", "cid:missing", "data:image/svg+xml;base64,PHN2Zy8+", "data:image/png;base64,bm90YW5pbWFnZQ==", "data:text/html;base64,PHNjcmlwdD4="} {
		msg := Message{To: "reader@example.test", Subject: "Test", TextBody: "Test", HTMLBody: `<img src="` + src + `">`}
		if err := msg.Validate(); err == nil {
			t.Fatal("invalid image accepted", src)
		}
		if _, err := RenderRFC5322("news@client.test", msg); err == nil {
			t.Fatal("invalid image rendered", src)
		}
	}
}

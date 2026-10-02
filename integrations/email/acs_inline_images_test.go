package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestACSInlineImagesCarryReviewedBytes(t *testing.T) {
	data, uri := inlinePNG(t)
	msg := Message{To: "reader@example.test", Subject: "Client update", TextBody: "Welcome", HTMLBody: `<p>Welcome</p><img src="` + uri + `" alt="Client logo"><img src="` + uri + `" alt="Again">`}

	sender := acsFixture(t)
	sender.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Content struct {
				HTML string `json:"html"`
			} `json:"content"`
			Attachments []struct {
				ContentID string `json:"contentId"`
				Data      string `json:"contentInBase64"`
				MIME      string `json:"contentType"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Attachments) != 1 || payload.Attachments[0].ContentID != "image-1@memql" || payload.Attachments[0].MIME != "image/png" || payload.Attachments[0].Data != base64.StdEncoding.EncodeToString(data) || strings.Count(payload.Content.HTML, "cid:image-1@memql") != 2 {
			t.Fatal("ACS inline payload lost reviewed image")
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	if err := sender.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
		t.Fatal(err)
	}
}

func TestACSInlineImageRefusesUnresolvedOrActiveAssetBeforeSending(t *testing.T) {
	for _, src := range []string{"memql-asset:missing", "cid:missing", "data:image/svg+xml;base64,PHN2Zy8+", "data:image/png;base64,bm90YW5pbWFnZQ==", "data:text/html;base64,PHNjcmlwdD4="} {
		sender := acsFixture(t)
		sender.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("invalid image reached network"); return nil, nil })
		if err := sender.Send(context.Background(), Message{To: "reader@example.test", Subject: "Test", TextBody: "Test", HTMLBody: `<img src="` + src + `">`}, SendAs{AccountID: "client"}); err == nil {
			t.Fatal("invalid image accepted", src)
		}
	}
}

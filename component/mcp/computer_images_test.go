package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

type screenshotEngine struct {
	*delegatedEngine
	raw string
}

func TestComputerScreenshotPresentationBoundaries(t *testing.T) {
	raw, _ := screenshotFixture(t)
	failedEnvelope, err := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": raw}}, "isError": true})
	if err != nil {
		t.Fatal(err)
	}
	mixedEnvelope, err := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": raw}, {"type": "text", "text": "another observation"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, action, source string
	}{
		{"workbenchHost", "screenshot", raw},
		{"workerComputer", "click", raw},
		{"workerComputer", "screenshot", strings.Replace(raw, `"ok":true`, `"ok":false`, 1)},
		{"workerComputer", "screenshot", strings.Replace(raw, "integration:worker:result", "untrusted:document", 1)},
		{"workerComputer", "screenshot", strings.Replace(raw, "worker-call:test", "document:test", 1)},
		{"workerComputer", "screenshot", `{"content":[{"type":"image","data":"already-presented","mimeType":"image/png"}],"isError":false}`},
		{"workerComputer", "screenshot", "not JSON"},
		{"workerComputer", "screenshot", string(failedEnvelope)},
		{"workerComputer", "screenshot", string(mixedEnvelope)},
	} {
		_, receipt := presentExecutedToolResult(tc.name, map[string]any{"action": tc.action}, tc.source)
		if receipt != tc.source {
			t.Fatalf("unrelated or failed result was reinterpreted: %s/%s", tc.name, tc.action)
		}
	}
	result, _ := presentExecutedToolResult("worker.workerComputer", map[string]any{"action": "screenshot"}, raw)
	if result["content"].([]map[string]any)[1]["type"] != "image" {
		t.Fatal("namespaced native computer tool lost its image")
	}
}

func TestComputerScreenshotInvalidPixelsRemainExplicitEvidence(t *testing.T) {
	raw, _ := screenshotFixture(t)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing", func(o map[string]any) { delete(o, "bytesBase64") }},
		{"bad_base64", func(o map[string]any) { o["bytesBase64"] = "invalid!" }},
		{"not_image", func(o map[string]any) { o["bytesBase64"] = base64.StdEncoding.EncodeToString([]byte("not pixels")) }},
		{"too_large", func(o map[string]any) {
			o["bytesBase64"] = strings.Repeat("A", base64.StdEncoding.EncodedLen(maxComputerImageBytes)+4)
		}},
		{"wrong_dimensions", func(o map[string]any) { o["width"] = 4 }},
		{"wrong_size", func(o map[string]any) { o["sizeBytes"] = 1 }},
		{"wrong_format", func(o map[string]any) { o["format"] = "jpeg" }},
		{"pixel_limit", func(o map[string]any) {
			data, _ := base64.StdEncoding.DecodeString(o["bytesBase64"].(string))
			binary.BigEndian.PutUint32(data[16:20], 65536)
			binary.BigEndian.PutUint32(data[20:24], 65536)
			binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
			o["bytesBase64"] = base64.StdEncoding.EncodeToString(data)
			o["width"], o["height"] = 65536, 65536
		}},
		{"corrupt_pixels", func(o map[string]any) {
			data, _ := base64.StdEncoding.DecodeString(o["bytesBase64"].(string))
			data[len(data)-16] ^= 0xff
			o["bytesBase64"] = base64.StdEncoding.EncodeToString(data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var source map[string]any
			_ = json.Unmarshal([]byte(raw), &source)
			o := source["data"].([]any)[0].(map[string]any)["worker-call:test"].(map[string]any)["payload"].(map[string]any)["output"].(map[string]any)
			tc.change(o)
			changed, _ := json.Marshal(source)
			result, receipt := presentExecutedToolResult("workerComputer", map[string]any{"action": "screenshot"}, string(changed))
			content := result["content"].([]map[string]any)
			if isError(result) || content[0]["text"] != string(changed) || content[1]["type"] != "text" || !strings.Contains(content[1]["text"].(string), "delivery unavailable") {
				t.Fatal("presentation failure lost original evidence or claimed visible pixels")
			}
			delivered, _ := json.Marshal(result)
			if receipt != string(delivered) {
				t.Fatal("unavailable-delivery receipt diverged")
			}
		})
	}
}

func TestComputerScreenshotJPEG(t *testing.T) {
	var pixels bytes.Buffer
	if err := jpeg.Encode(&pixels, image.NewNRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"jpeg", "jpg"} {
		encoded := base64.StdEncoding.EncodeToString(pixels.Bytes())
		actual, mime, err := validateComputerImage(encoded, map[string]any{"format": format, "width": float64(3), "height": float64(2), "sizeBytes": float64(pixels.Len())})
		if err != nil || mime != "image/jpeg" || !bytes.Equal(actual, pixels.Bytes()) {
			t.Fatalf("valid %s pixels not preserved: %v", format, err)
		}
	}
}

func (e *screenshotEngine) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	if _, err := e.delegatedEngine.ExecuteToolByName(ctx, name, args); err != nil {
		return "", err
	}
	return e.raw, nil
}

func screenshotFixture(t *testing.T) (string, string) {
	t.Helper()
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(pixels.Bytes())
	raw, err := json.Marshal(map[string]any{"data": []any{map[string]any{
		"worker-call:test": map[string]any{"concept": "integration:worker:result", "payload": map[string]any{
			"ok": true, "outputPreview": "test screenshot", "output": map[string]any{
				"bytesBase64": encoded, "format": "png", "width": 3, "height": 2, "sizeBytes": pixels.Len(),
			},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), encoded
}

func TestComputerScreenshotDeliveredAsImageAndRecordedExactly(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "native_text_envelope"}[wrapped], func(t *testing.T) {
			for _, delegated := range []bool{false, true} {
				t.Run(map[bool]string{false: "ordinary", true: "delegated"}[delegated], func(t *testing.T) {
					raw, encoded := screenshotFixture(t)
					if wrapped {
						envelope, err := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": raw}}, "_meta": map[string]any{"receipt": "keep"}})
						if err != nil {
							t.Fatal(err)
						}
						raw = string(envelope)
					}
					fixture := delegatedFixture()
					fixture.reg.tools = append(fixture.reg.tools, tool("workerComputer", []string{"assistant", "owner"}))
					fixture.row["toolScope"].(map[string]any)["tools"] = []any{map[string]any{"name": "workerComputer"}}
					eng := &screenshotEngine{delegatedEngine: fixture, raw: raw}
					session := ""
					if delegated {
						session = "s"
					}
					result := callMCPTool(asPerson("owner"), eng, "owner", TierInline, session, "workerComputer", map[string]any{"action": "screenshot"})
					serialized, _ := json.Marshal(result)
					var wire struct {
						Content []struct{ Type, Text, Data, MimeType string }
						IsError bool
					}
					if err := json.Unmarshal(serialized, &wire); err != nil {
						t.Fatal(err)
					}
					if wire.IsError || len(wire.Content) != 2 || wire.Content[1].Type != "image" || wire.Content[1].Data != encoded || wire.Content[1].MimeType != "image/png" {
						t.Fatalf("screenshot not delivered as unchanged MCP image: %s", serialized)
					}
					if strings.Contains(wire.Content[0].Text, encoded) || !strings.Contains(wire.Content[0].Text, "sha256") || !strings.Contains(wire.Content[0].Text, "test screenshot") {
						t.Fatal("metadata lost or image bytes duplicated in text")
					}
					if delegated && eng.sourceResult != string(serialized) {
						t.Fatal("journal differs from the image-bearing response delivered to the app")
					}
					if wrapped && result["_meta"].(map[string]any)["receipt"] != "keep" {
						t.Fatal("native envelope metadata lost")
					}
				})
			}
		})
	}
}

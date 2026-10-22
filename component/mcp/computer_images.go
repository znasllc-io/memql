package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

const (
	maxComputerImageBytes  = 4 << 20
	maxComputerImagePixels = 16 << 20
)

// presentExecutedToolResult changes presentation only, after execution and all
// permission checks. Apps need MCP image content to see a native screenshot;
// a base64 field in a text block is not vision. The returned receipt is exactly
// what we deliver, so delegation recovery cannot claim a different observation.
// Existing MCP content, other actions and failed captures retain their format.
func presentExecutedToolResult(name string, args map[string]any, raw string) (map[string]any, string) {
	result := toolResultFromJSON(raw)
	if (name != "workerComputer" && !strings.HasSuffix(name, ".workerComputer")) || args["action"] != "screenshot" {
		return result, raw
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(raw), &envelope) != nil || envelope["content"] != nil {
		return result, raw
	}
	rows, _ := envelope["data"].([]any)
	if len(rows) != 1 {
		return result, raw
	}
	row, _ := rows[0].(map[string]any)
	if len(row) != 1 {
		return result, raw
	}
	var output map[string]any
	for key, value := range row {
		node, _ := value.(map[string]any)
		payload, _ := node["payload"].(map[string]any)
		if !strings.HasPrefix(key, "worker-call:") || node["concept"] != "integration:worker:result" || payload["ok"] != true {
			return result, raw
		}
		output, _ = payload["output"].(map[string]any)
	}
	encoded, _ := output["bytesBase64"].(string)
	imageBytes, mime, err := validateComputerImage(encoded, output)
	if err != nil {
		// The capture already happened: do not label presentation failure as an
		// execution error or retry it. Keep the original receipt, including bytes,
		// and explicitly distinguish unavailable vision from a successful capture.
		result = map[string]any{"content": []map[string]any{
			{"type": "text", "text": raw},
			{"type": "text", "text": "Screenshot image delivery unavailable: " + err.Error() + ". Original capture result retained; no capture was retried."},
		}, "isError": false}
	} else {
		digest := sha256.Sum256(imageBytes)
		delete(output, "bytesBase64")
		output["imageContent"] = map[string]any{"index": 1, "mimeType": mime, "sizeBytes": len(imageBytes), "sha256": hex.EncodeToString(digest[:])}
		metadata, _ := json.Marshal(envelope)
		// Keep pixels once, unchanged. Duplicating base64 in the metadata would
		// waste app context and can exceed the bounded delegated-source journal.
		result = map[string]any{"content": []map[string]any{
			{"type": "text", "text": string(metadata)},
			{"type": "image", "data": encoded, "mimeType": mime},
		}, "isError": false}
	}
	delivered, _ := json.Marshal(result)
	return result, string(delivered)
}

func validateComputerImage(encoded string, output map[string]any) ([]byte, string, error) {
	// Bound encoded bytes before allocating, then dimensions before decoding
	// pixels. Malformed/oversized outputs stay explicit text evidence; this
	// adapter never fetches URLs, changes bytes or bypasses capture consent.
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(maxComputerImageBytes) {
		return nil, "", fmt.Errorf("missing image or image exceeds %d bytes", maxComputerImageBytes)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > maxComputerImageBytes {
		return nil, "", fmt.Errorf("invalid or oversized base64 image")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, "", fmt.Errorf("image is not a readable PNG or JPEG")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxComputerImagePixels {
		return nil, "", fmt.Errorf("image dimensions exceed the pixel limit")
	}
	declared, _ := output["format"].(string)
	if declared == "jpg" {
		declared = "jpeg"
	}
	if declared != format || output["width"] != float64(config.Width) || output["height"] != float64(config.Height) || output["sizeBytes"] != float64(len(raw)) {
		return nil, "", fmt.Errorf("image bytes do not match capture metadata")
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return nil, "", fmt.Errorf("image pixels cannot be decoded")
	}
	return raw, "image/" + format, nil
}

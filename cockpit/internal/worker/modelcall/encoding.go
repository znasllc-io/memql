package modelcall

import "encoding/base64"

// base64Of and decodeBase64 are the one encoding both the image and
// audio paths use.
//
// STANDARD encoding with padding, which is what every runtime here
// emits and expects: a data URL's base64 is standard, Ollama's `images`
// array is standard, and the OpenAI `input_audio` field is standard.
// The URL-safe alphabet appears nowhere in this protocol and accepting
// it would mean accepting a string no runtime produces.
func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// decodeBase64 tolerates a missing pad, and only that. Some runtimes
// strip trailing '=' when they concatenate; nothing about the payload
// changes, and refusing it would fail a decode over punctuation.
func decodeBase64(s string) ([]byte, error) {
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return out, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

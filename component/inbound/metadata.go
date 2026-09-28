package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// deliveryHeaders copies only the source's declared routing metadata. A body
// HMAC does not authenticate these values; consumers must bind destructive
// operations to identity in the signed payload. Never persist credentials.
func deliveryHeaders(src SourceConfig, headers http.Header) (string, error) {
	out := map[string]string{}
	for _, name := range src.ForwardHeaders {
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "", "authorization", "proxy-authorization", "cookie", "set-cookie":
			continue
		}
		if strings.EqualFold(name, src.SignatureHeader) {
			continue
		}
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 || len(values[0]) > 1024 || !utf8.ValidString(values[0]) || strings.ContainsAny(values[0], "\x00\r\n") {
			return "", fmt.Errorf("invalid delivery header")
		}
		out[name] = values[0]
	}
	encoded, err := json.Marshal(out)
	return string(encoded), err
}

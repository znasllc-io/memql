package edge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/znasllc-io/memql/component/memql"
)

// Only an exact read-only report body avoids the authentication-attempt bucket.
// Duplicate keys, mixed account authority or a second JSON value fail closed.
// The native service separately verifies recipient and immutable expiry.
func callsAccountIsReportRead(r *http.Request) bool {
	if r.Body == nil || r.ContentLength < 0 || r.ContentLength > memql.CallsSmallBodyBytes {
		return false
	}
	body, e := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if e != nil {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	action := ""
	report := map[string]string{}
	for d.More() {
		tok, e = d.Token()
		key, ok := tok.(string)
		if e != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "action":
			if d.Decode(&action) != nil {
				return false
			}
		case "report":
			tok, e = d.Token()
			if e != nil || tok != json.Delim('{') {
				return false
			}
			for d.More() {
				tok, e = d.Token()
				k, ok := tok.(string)
				if e != nil || !ok || (k != "id" && k != "cursor") {
					return false
				}
				if _, seen := report[k]; seen {
					return false
				}
				var value string
				if d.Decode(&value) != nil {
					return false
				}
				report[k] = value
			}
			tok, e = d.Token()
			if e != nil || tok != json.Delim('}') {
				return false
			}
		default:
			return false
		}
	}
	tok, e = d.Token()
	if e != nil || tok != json.Delim('}') || !seen["action"] || !seen["report"] {
		return false
	}
	if _, e = d.Token(); e != io.EOF {
		return false
	}
	if action == "report_list" {
		_, hasID := report["id"]
		return !hasID && len(report["cursor"]) <= 4096
	}
	if action == "report_status" {
		_, hasCursor := report["cursor"]
		return !hasCursor && len(report["id"]) == 64
	}
	return false
}

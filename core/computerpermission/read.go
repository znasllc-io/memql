package computerpermission

import (
	"encoding/json"
	"fmt"
	"math"
)

// ReadContract advertises bounded text inspection on the existing fs_read
// operation. It does not grant access: directory/once permissions and machine
// policy still gate the exact request. Older workers must never ignore ranges.
const ReadContract = "workerHost.fs_read"
const ReadVersion = 2

func ExtendedRead(args map[string]any) bool {
	for key := range args {
		if key != "path" && key != "maxBytes" {
			return true
		}
	}
	return false
}

type ReadOptions struct {
	MaxBytes, OffsetBytes, StartLine, LineCount, MaxMatches, MaxScanBytes int
	Search                                                                string
}

func ParseReadOptions(args map[string]any) (ReadOptions, error) {
	o := ReadOptions{MaxBytes: 5 * 1024 * 1024, MaxMatches: 20, MaxScanBytes: 5 * 1024 * 1024}
	bounds := map[string]struct {
		target   *int
		min, max int
	}{
		"maxBytes": {&o.MaxBytes, 1, 5 * 1024 * 1024}, "offsetBytes": {&o.OffsetBytes, 0, 1 << 50},
		"startLine": {&o.StartLine, 1, 10000000}, "lineCount": {&o.LineCount, 1, 10000},
		"maxMatches": {&o.MaxMatches, 1, 100}, "maxScanBytes": {&o.MaxScanBytes, 1, 16 * 1024 * 1024},
	}
	for key, value := range args {
		if key == "path" {
			if _, ok := value.(string); !ok {
				return o, fmt.Errorf("path must be a string")
			}
			continue
		}
		if key == "search" {
			s, ok := value.(string)
			if !ok || s == "" || len(s) > 1024 {
				return o, fmt.Errorf("search must be a nonempty literal of at most 1024 bytes")
			}
			o.Search = s
			continue
		}
		b, ok := bounds[key]
		if !ok {
			return o, fmt.Errorf("unsupported fs_read argument %q", key)
		}
		var n float64
		switch v := value.(type) {
		case int:
			n = float64(v)
		case int64:
			n = float64(v)
		case float64:
			n = v
		case json.Number:
			var err error
			n, err = v.Float64()
			if err != nil {
				return o, fmt.Errorf("%s must be an integer", key)
			}
		default:
			return o, fmt.Errorf("%s must be an integer", key)
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < float64(b.min) || n > float64(b.max) {
			return o, fmt.Errorf("%s must be an integer between %d and %d", key, b.min, b.max)
		}
		*b.target = int(n)
	}
	_, offset := args["offsetBytes"]
	_, line := args["startLine"]
	if offset && (line || o.LineCount > 0 || o.Search != "") {
		return o, fmt.Errorf("offsetBytes cannot be combined with line ranges or search")
	}
	if o.Search != "" && (line || o.LineCount > 0) {
		return o, fmt.Errorf("search cannot be combined with a line range")
	}
	if o.LineCount > 0 && !line {
		return o, fmt.Errorf("lineCount requires startLine")
	}
	if line && o.LineCount == 0 {
		return o, fmt.Errorf("startLine requires lineCount")
	}
	if _, ok := args["maxMatches"]; ok && o.Search == "" {
		return o, fmt.Errorf("maxMatches requires search")
	}
	if _, ok := args["maxScanBytes"]; ok && o.Search == "" && !line {
		return o, fmt.Errorf("maxScanBytes requires search or a line range")
	}
	return o, nil
}

package common

import "strings"

type KeySegment string

func CompositeKey(segments ...KeySegment) string {
	safeSegments := make([]string, 0, len(segments))
	for _, seg := range segments {
		clean := strings.TrimSpace(string(seg))
		clean = strings.ReplaceAll(clean, ":", "_")
		if clean == "" {
			continue
		}
		safeSegments = append(safeSegments, clean)
	}
	return strings.Join(safeSegments, ":")
}

package work

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const unfinishedStepMarker = "work_step_unfinished_v1:"

// This native control receipt survives the function/mesh string transport.
// It is produced only from a validated typed review, never by searching an
// assistant's prose for disclaimers. The encoded gap cannot mimic other codes.
func UnfinishedStepError(gap string) error {
	raw, _ := json.Marshal(struct {
		Gap string `json:"gap"`
	}{gap})
	return fmt.Errorf("%s%s", unfinishedStepMarker, base64.RawURLEncoding.EncodeToString(raw))
}

func UnfinishedStepGap(message string) (string, bool) {
	_, tail, ok := strings.Cut(message, unfinishedStepMarker)
	if !ok {
		return "", false
	}
	end := strings.IndexFunc(tail, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
	if end >= 0 {
		tail = tail[:end]
	}
	if len(tail) > 2048 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(tail)
	if err != nil {
		return "", false
	}
	var value struct {
		Gap string `json:"gap"`
	}
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value.Gap) == "" || len(value.Gap) > 1200 {
		return "", false
	}
	return value.Gap, true
}

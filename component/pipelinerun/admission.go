package pipelinerun

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const admissionMaxBytes = 4096

// FileRunAdmission reads an operator-owned, short-lived admission decision on
// EVERY claim. A projected ConfigMap directory is suitable; a subPath mount or
// an environment variable containing the decision would freeze it at startup.
// No decision is cached in a replica. An unset path preserves normal admission;
// a configured but unreadable, malformed or expired decision fails closed.
// This gate controls new pipeline runs, never recovery or the serving runtime.
func FileRunAdmission(path string) func(time.Time) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return func(now time.Time) (bool, error) {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("read admission decision: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > admissionMaxBytes {
			return false, fmt.Errorf("admission decision must be a regular file of at most %d bytes", admissionMaxBytes)
		}
		f, err := os.Open(path)
		if err != nil {
			return false, fmt.Errorf("open admission decision: %w", err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, admissionMaxBytes+1))
		if err != nil || len(data) > admissionMaxBytes {
			return false, fmt.Errorf("admission decision could not be read within its size limit")
		}
		var decision struct {
			AllowNewRuns *bool     `json:"allowNewRuns"`
			ValidUntil   time.Time `json:"validUntil"`
			Reason       string    `json:"reason"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&decision); err != nil {
			return false, fmt.Errorf("invalid admission decision: %w", err)
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return false, fmt.Errorf("admission decision must contain exactly one JSON object")
		}
		if decision.AllowNewRuns == nil || !decision.ValidUntil.After(now) || decision.ValidUntil.After(now.Add(2*time.Hour)) {
			return false, fmt.Errorf("admission decision needs allowNewRuns and a future validUntil no more than two hours away")
		}
		return *decision.AllowNewRuns, nil
	}
}

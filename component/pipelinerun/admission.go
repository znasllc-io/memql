package pipelinerun

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

const admissionMaxBytes = 4096

// FileRunAdmission reads an operator-owned, short-lived admission decision on
// EVERY claim. A projected ConfigMap directory is suitable; a subPath mount or
// an environment variable containing the decision would freeze it at startup.
// No decision is cached in a replica. An unset path preserves normal admission;
// a configured but unreadable, malformed or expired decision fails closed.
// This gate controls new pipeline runs, never recovery or the serving runtime.
//
// TRAIN RUNS HAVE AN ALLOWANCE OF THEIR OWN (release trains, design record
// 2026-10-27, section 5.3): an optional allowTrainRuns admits a train run while
// allowNewRuns holds every other one, because trains are scheduled and the cost
// guard budgets them apart. A decision without the field admits a train run
// exactly as it admits any run. The reader refuses unknown fields, so the
// engine that knows this field ships before any guard writes it. The train
// allowance never widens the separate live cloud spending decision.
func FileRunAdmission(path string) func(time.Time, bool) (bool, error) {
	return fileAdmission(path, false)
}

// FileCloudAdmission gates new cloud Jobs, including isolation probes. A cost
// hold must not stop Fleet work, but neither a captured policy nor an already
// admitted run can authorize spending after this live decision closes. Legacy
// decisions retain their global meaning; v2 requires both global and cloud
// admission. Adoption and cleanup of existing work do not call this gate.
func FileCloudAdmission(path string) func(time.Time) (bool, error) {
	gate := fileAdmission(path, true)
	if gate == nil {
		return nil
	}
	return func(now time.Time) (bool, error) { return gate(now, false) }
}

func fileAdmission(path string, cloud bool) func(time.Time, bool) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return func(now time.Time, train bool) (bool, error) {
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
			Version        json.RawMessage `json:"version"`
			AllowNewRuns   *bool           `json:"allowNewRuns"`
			AllowTrainRuns *bool           `json:"allowTrainRuns"`
			AllowCloud     json.RawMessage `json:"allowCloud"`
			ValidUntil     time.Time       `json:"validUntil"`
			Reason         string          `json:"reason"`
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
		allowRun := *decision.AllowNewRuns || (train && decision.AllowTrainRuns != nil && *decision.AllowTrainRuns)
		if len(decision.Version) == 0 {
			if len(decision.AllowCloud) != 0 {
				return false, fmt.Errorf("separate cloud admission requires version 2")
			}
			return allowRun, nil
		}
		var version int
		var allowCloud *bool
		cloudErr := json.Unmarshal(decision.AllowCloud, &allowCloud)
		if err := json.Unmarshal(decision.Version, &version); err != nil || version != 2 || cloudErr != nil || allowCloud == nil {
			return false, fmt.Errorf("version 2 admission requires allowNewRuns and allowCloud booleans")
		}
		if cloud {
			return *decision.AllowNewRuns && *allowCloud, nil
		}
		return allowRun, nil
	}
}

// IsTrainRun reports whether a run is a release train's: the pull request run
// of a train/ branch, which only a train coordinator creates (a person with
// write access could name a branch so, and would be admitted on the train
// allowance; write access is already trusted with the repository).
func IsTrainRun(r Run) bool {
	return r.Event == pipelines.EventPullRequest && strings.HasPrefix(r.HeadBranch, pipelines.TrainBranchPrefix)
}

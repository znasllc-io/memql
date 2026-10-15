package agent

import (
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// Read evidence is an observation, not a completion oracle or recovery policy.
// A repeated answer to differently worded queries is useful evidence of a
// possible stall, but may be a legitimate verification. It never pauses work.
// The existing recovery checkpoint owns persistence and the batch watermark.
type workReadEvidence struct {
	Recent   []readEvidenceReceipt `json:"recent,omitempty"`
	Repeated int                   `json:"repeated"`
	Novel    int                   `json:"novel"`
	Signal   string                `json:"signal,omitempty"`
}
type readEvidenceReceipt struct {
	Tool         string `json:"tool"`
	ResultDigest string `json:"resultDigest"`
	CallID       string `json:"callId"`
}

func observedRead(name string, args any) bool {
	switch name {
	case "recallWorkHistory":
		return true
	case "workerHost":
		m, _ := args.(map[string]any)
		return m["action"] == "fs_read" || m["action"] == "fs_list" || m["action"] == "fs_stat"
	}
	return false // Unknown effects are not classified from prose or a name prefix.
}

func (s *workReadEvidence) observe(name, callID string, args any, result string) {
	if !observedRead(name, args) || result == common.UnexecutedWorkToolResult {
		return
	}
	var value any
	if json.Unmarshal([]byte(result), &value) != nil {
		value = result
	}
	// Only compare the returned evidence, not the search wording or transport ID.
	// No arbitrary JSON keys are stripped: changed source/version metadata matters.
	digest := recoveryDigest(value)
	repeated := false
	for _, old := range s.Recent {
		if old.Tool == name && old.ResultDigest == digest {
			repeated = true
			break
		}
	}
	if repeated {
		s.Repeated = min(s.Repeated+1, 100)
		s.Signal = "repeated_read_result"
	} else {
		s.Novel = min(s.Novel+1, 100)
		s.Repeated = 0
		s.Signal = "new_read_result"
	}
	// Call IDs are public source references; bound adversarial provider output.
	if len(callID) > 128 {
		callID = recoveryDigest(callID)
	}
	s.Recent = append(s.Recent, readEvidenceReceipt{Tool: strings.TrimSpace(name), ResultDigest: digest, CallID: callID})
	if len(s.Recent) > 8 {
		s.Recent = s.Recent[len(s.Recent)-8:]
	}
}

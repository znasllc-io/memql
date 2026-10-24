package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// WriteWorkerYAML upserts one home into ~/.memql/workers.yaml
// (and mirrors that home into path as legacy worker.yaml).
//
// ADDITIVE: pairing a second cluster does not clobber an existing
// home. Pass force via UpsertHome when replacing one home's token.
// The install scripts call the same UpsertHome path through
// write_worker_yaml in lib.sh.
func WriteWorkerYAML(path, clusterURL, token, name string) error {
	workersPath := DefaultWorkersPath()
	if path != "" && path != DefaultConfigPath() {
		// Tests / advanced callers that point --config at a temp
		// dir get workers.yaml beside that legacy path.
		workersPath = filepath.Join(filepath.Dir(path), "workers.yaml")
	}
	_, err := UpsertHome(UpsertHomeOptions{
		WorkersPath:  workersPath,
		LegacyPath:   path,
		ClusterURL:   clusterURL,
		Token:        token,
		Name:         name,
		Capabilities: capabilitiesForBuildTag(),
	})
	return err
}

// capabilitiesForBuildTag returns the capabilities the running
// cockpit binary can advertise. The computeruse-tagged sibling
// (capabilities_computeruse.go) overrides to include "COMPUTERUSE"; the default
// build is HEADLESS-only.
func capabilitiesForBuildTag() []string {
	return capabilitiesForBuildTagImpl()
}

// WizardState is the persistent slice of wizard state needed to
// resume after a self-exec restart. Spilled to ~/.memql/wizard-
// state.json before exec; read back on startup. Contains nothing
// secret -- the worker token is already in worker.yaml at the
// point we'd ever spill state.
type WizardState struct {
	// Step the wizard was on at exec time. The new process resumes
	// at this step instead of starting from "paste a code".
	Step string `json:"step"`
	// PairingCode the user originally pasted (canonical XXXX-XXXX).
	// Empty when the wizard never had a code (e.g. running in
	// pure-TCC mode).
	PairingCode string `json:"pairing_code,omitempty"`
	// IdentityURL the wizard used for the redeem POST. Saved so the
	// resumed wizard knows which identity service to talk to without
	// re-prompting the user.
	IdentityURL string `json:"identity_url,omitempty"`
	// ClusterURL resolved from the redeem response. Saved so the
	// resumed wizard doesn't need to re-redeem (which would fail
	// since pairing codes are single-use).
	ClusterURL string `json:"cluster_url,omitempty"`
	// Token redeemed from the pairing code. Already persisted to
	// worker.yaml at this point; we just keep it here so the
	// resumed wizard knows the redeem step is done.
	Token string `json:"token,omitempty"`
	// IdentityId of the worker_token row. Informational; helps the
	// resumed wizard render audit context.
	IdentityId string `json:"identity_id,omitempty"`
	// SpilledAt timestamp; resume rejects state files older than
	// 5 minutes since stale state is more dangerous than no state.
	SpilledAt time.Time `json:"spilled_at"`
	// Reason the wizard chose to self-exec. Surfaced to the user
	// in the resumed wizard's status line.
	Reason string `json:"reason,omitempty"`
}

// stateFilePath returns the canonical path for the wizard's state
// spill.
func stateFilePath() string {
	dir := filepath.Dir(DefaultConfigPath())
	return filepath.Join(dir, "wizard-state.json")
}

// SaveWizardState writes the state file. Returns an error if the
// caller can't recover from -- the state will be lost on exec
// otherwise.
func SaveWizardState(s WizardState) error {
	if s.SpilledAt.IsZero() {
		s.SpilledAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("wizard state: marshal: %w", err)
	}
	path := stateFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("wizard state: mkdir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("wizard state: write: %w", err)
	}
	return nil
}

// LoadWizardState reads + clears the state file. Returns nil when
// no state exists (fresh wizard run) or when the state is too old
// to trust (5+ minute age suggests the exec never happened or the
// user lost interest).
func LoadWizardState() *WizardState {
	path := stateFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Always remove on read -- state is one-shot. Do this even on
	// parse failure so a malformed file doesn't loop forever.
	defer os.Remove(path)
	var s WizardState
	if err := yaml.Unmarshal(data, &s); err != nil {
		// Try JSON; we wrote JSON above but defending against
		// hand-edits is cheap.
		if jsonErr := json.Unmarshal(data, &s); jsonErr != nil {
			return nil
		}
	}
	if s.SpilledAt.IsZero() || time.Since(s.SpilledAt) > 5*time.Minute {
		return nil
	}
	return &s
}

// ClearWizardState removes the state file unconditionally. Safe
// to call when no state exists.
func ClearWizardState() {
	_ = os.Remove(stateFilePath())
}

package consent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type directoryPauseState struct {
	Version int    `json:"version"`
	Home    string `json:"home"`
	Paused  *bool  `json:"paused"`
}

func (m *Manager) loadDirectoryPause(dir, home string) {
	m.statePath = filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(home))))
	body, err := os.ReadFile(m.statePath)
	if os.IsNotExist(err) {
		return // No local stop has been recorded. This is not a directory grant.
	}
	var state directoryPauseState
	if err == nil {
		err = json.Unmarshal(body, &state)
	}
	if err == nil && (state.Version != 1 || state.Home != home || state.Paused == nil) {
		err = fmt.Errorf("invalid saved consent state")
	}
	if err != nil {
		m.directoryReadsPaused, m.stateErr = true, err
		return
	}
	m.directoryReadsPaused = *state.Paused
}

// Called under m.mu. Persist before acknowledging a local decision. A
// partial write cannot turn a stop into permission on the next startup.
func (m *Manager) saveDirectoryPause(paused bool) error {
	if m.statePath == "" {
		return nil // Isolated, in-memory managers used by tests.
	}
	dir := filepath.Dir(m.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	body, err := json.Marshal(directoryPauseState{Version: 1, Home: m.home, Paused: &paused})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".consent-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), m.statePath); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// machineIDFile is the persistent install id for this physical machine.
// The engine uses label machineId on Register to reclaim an existing
// registration when the operator re-pairs with a new worker token, instead of
// minting a duplicate row for the same Mac.
const machineIDFile = "machine-id"

// LabelMachineId matches component/worker.LabelMachineId on the engine.
const LabelMachineId = "machineId"

// ONE MACHINE, ONE ID, WHICHEVER WAY THE WORKER WAS STARTED
// (memql-cockpit#430).
//
// The id exists so a re-pair reclaims this machine's registration instead
// of minting a second row for it -- and that only works if every path that
// can register this machine sends the same id. There are three:
//
//   - the fleet (workers.yaml), whose per-home state dir is
//     <root>/homes/<id> (ConfigForHome);
//   - the single-home path (`worker run --cluster/--token-file`), whose
//     state dir is <root> itself;
//   - the legacy mirror (worker.yaml), which UpsertHome and the installers
//     write with the per-home state dir, <root>/homes/<id>.
//
// Keying the file on the state dir as given put it in three places, so
// switching modes minted the duplicate row ef64aa3 exists to prevent. The
// file now lives at the machine's state ROOT, and machineStateRoot is the
// one function that finds that root from any of the three.

// machineStateRoot returns the machine-level state directory a (possibly
// per-home) state dir belongs to: <root>/homes/<id> answers <root>, and
// anything else answers itself.
func machineStateRoot(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return defaultStateDir()
	}
	clean := filepath.Clean(stateDir)
	if filepath.Base(filepath.Dir(clean)) == homesDirName {
		return filepath.Dir(filepath.Dir(clean))
	}
	return clean
}

// homesDirName is the directory ConfigForHome namespaces each home under.
const homesDirName = "homes"

// machineIDFor is the id cfg registers with: the one the caller resolved
// for the whole machine (the fleet resolves it once, before any home
// connects, so two homes cannot race each other into minting two), or the
// one at cfg's machine root.
func machineIDFor(cfg Config) (string, error) {
	if id := strings.TrimSpace(cfg.MachineID); id != "" {
		return id, nil
	}
	return ResolveMachineID(machineStateRoot(cfg.StateDir))
}

// ResolveMachineID returns this machine's id from its state root, minting
// one on first use.
//
// A build before memql-cockpit#430 kept the id per home, under
// <root>/homes/<id>/machine-id. The first of those (in name order, so the
// choice is stable) is ADOPTED rather than replaced: it is the id at least
// one cluster already holds for this machine, and replacing it would move
// every such registration off its reclaim key for nothing. A home whose
// old id differs re-registers under the adopted one on its next connect,
// and the engine refreshes that label on the row it already has -- the
// token, not the id, is what finds the row on an ordinary reconnect.
func ResolveMachineID(root string) (string, error) {
	// One resolution at a time in this process, so two homes that reach
	// Register together cannot each mint an id and register the machine
	// under two.
	machineIDMu.Lock()
	defer machineIDMu.Unlock()
	if strings.TrimSpace(root) == "" {
		root = defaultStateDir()
	}
	if id, err := readMachineID(filepath.Join(root, machineIDFile)); err != nil || id != "" {
		return id, err
	}
	if id := adoptHomeMachineID(root); id != "" {
		if err := writeMachineID(root, id); err != nil {
			return "", err
		}
		return id, nil
	}
	return EnsureMachineID(root)
}

// machineIDMu serializes ResolveMachineID within the process.
var machineIDMu sync.Mutex

// peekMachineID is ResolveMachineID without the writes: what this machine
// WOULD register as, for `memql worker config`. Printing a config must not
// be what mints an identity.
func peekMachineID(root string) string {
	if strings.TrimSpace(root) == "" {
		root = defaultStateDir()
	}
	if id, _ := readMachineID(filepath.Join(root, machineIDFile)); id != "" {
		return id
	}
	return adoptHomeMachineID(root)
}

// adoptHomeMachineID is the first per-home id a pre-#430 build left under
// <root>/homes, or "".
func adoptHomeMachineID(root string) string {
	entries, err := os.ReadDir(filepath.Join(root, homesDirName))
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if id, _ := readMachineID(filepath.Join(root, homesDirName, name, machineIDFile)); id != "" {
			return id
		}
	}
	return ""
}

// readMachineID reads one id file: the id, "" when the file is absent or
// empty, or an error when it exists and cannot be read.
func readMachineID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("machine-id: read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// EnsureMachineID returns a stable id for this install, creating
// <stateDir>/machine-id on first use. Empty stateDir falls back to the default.
func EnsureMachineID(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = defaultStateDir()
	}
	if id, err := readMachineID(filepath.Join(stateDir, machineIDFile)); err != nil || id != "" {
		return id, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("machine-id: rand: %w", err)
	}
	id := hex.EncodeToString(raw)
	if err := writeMachineID(stateDir, id); err != nil {
		return "", err
	}
	return id, nil
}

// writeMachineID persists id at <dir>/machine-id atomically.
func writeMachineID(dir, id string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("machine-id: mkdir: %w", err)
	}
	path := filepath.Join(dir, machineIDFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("machine-id: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("machine-id: rename: %w", err)
	}
	return nil
}

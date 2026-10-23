package worker

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// UnpairURLResult intentionally contains no tokens or configuration contents.
// The installer uses the remaining count to decide whether shared files stay.
type UnpairURLResult struct {
	Removed   int  `json:"removed"`
	Remaining int  `json:"remaining"`
	Changed   bool `json:"changed"`
	DryRun    bool `json:"dry_run"`
}

// RemoveHomesByURL uses the same cluster identity rule as enrollment, including
// duplicate homes. Preview never migrates a legacy file or writes either file.
func RemoveHomesByURL(workersPath, legacyPath, clusterURL string, dryRun bool) (UnpairURLResult, error) {
	result := UnpairURLResult{DryRun: dryRun}
	target, err := url.Parse(strings.TrimSpace(clusterURL))
	if err != nil || target.Hostname() == "" || (target.Scheme != "https" && target.Scheme != "http") || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return result, errors.New("unpair: cluster URL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	// Refuse aliases: an uninstall must not rewrite credentials outside its scope.
	for _, path := range []string{workersPath, legacyPath} {
		if info, e := os.Lstat(path); e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return result, errors.New("unpair: enrollment files must be regular files")
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return result, e
		}
	}
	w := emptyWorkers()
	registryPresent := false
	if _, err = os.Stat(workersPath); err == nil {
		registryPresent = true
		w, err = loadWorkersFile(workersPath)
	} else if errors.Is(err, os.ErrNotExist) {
		var legacy Config
		legacy, err = LoadFile(legacyPath)
		if err == nil && (strings.TrimSpace(legacy.ClusterURL) != "" || strings.TrimSpace(legacy.Token) != "") {
			w = workersFromLegacy(legacy)
		}
	}
	if err != nil {
		return result, err
	}
	if err = w.Validate(); err != nil {
		return result, fmt.Errorf("unpair: invalid enrollment registry: %w", err)
	}
	kept := make([]Home, 0, len(w.Homes))
	for _, h := range w.Homes {
		if sameClusterURL(h.ClusterURL, clusterURL) {
			result.Removed++
		} else {
			kept = append(kept, h)
		}
	}
	result.Remaining = len(kept)
	// Validate the mirror before changing the authoritative registry. A broken
	// mirror must not turn this into a half-completed, falsely successful removal.
	if _, err = LoadFile(legacyPath); err != nil {
		return result, err
	}
	if dryRun {
		return result, nil
	}
	w.Homes = kept
	if result.Removed > 0 {
		if err = SaveWorkers(workersPath, w); err != nil {
			return result, err
		}
		result.Changed = true
	}
	// Also repair a stale mirror on an idempotent retry. Never promote its
	// removed token when an authoritative (even empty) registry already exists.
	if result.Removed > 0 || registryPresent {
		outcome, _, e := syncLegacyMirror(legacyPath, w)
		if e != nil {
			return result, e
		}
		result.Changed = result.Changed || outcome == MirrorDeleted || outcome == MirrorRewritten
	}
	return result, nil
}

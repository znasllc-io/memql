package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The sweep: what is at the origin right now, and which of it has changed.
//
// ===========================================================================
// A SCHEDULED WALK, NOT A FILE WATCHER, AND THAT IS THE DESIGN
// ===========================================================================
// The obvious reach is fsnotify. It is the wrong tool for a backup, for three
// reasons that all point the same way:
//
//   - A BACKUP MUST RECONCILE, NOT REACT. Everything that happened while the
//     process was down produced no event, and an event-only design never
//     learns about any of it. The sweep IS the answer to "what changed while
//     I was not running", and once it exists the events are an optimisation.
//   - THE VERIFY LANE IS ALREADY A SWEEP. The cockpit is the only party that
//     can see the origin, so it has to look on a schedule anyway to answer
//     `stale` and `origin_gone`. One mechanism serving both is one mechanism
//     to get right.
//   - Recursive watches do not exist portably. Linux needs one inotify watch
//     PER DIRECTORY against a per-user cap, and a deep tree exhausts it with
//     ENOSPC -- which presents as a backup that silently stops noticing.
//
// It also adds no dependency, and the repo has none for this today.
//
// fsnotify remains a sensible ACCELERATOR later -- a debounced trigger for an
// early sweep -- but the sweep would still be the source of truth.

// Stamp is what makes a file recognisable between sweeps.
//
// Size and modification time are the CHEAP half and are checked first: a
// folder of client video is tens of gigabytes, and hashing all of it every few
// minutes would keep a laptop's disk busy forever to learn nothing. The digest
// is computed only when the cheap half moved, and it is what decides whether
// bytes are actually sent -- a touched file whose contents did not change
// costs one read and no upload.
type Stamp struct {
	Identity string `json:"identity,omitempty"`
	Size     int64  `json:"size"`
	ModUnix  int64  `json:"modUnix"`
	ModNano  int64  `json:"modNano,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// Unchanged reports whether the cheap half matches, so no digest is needed.
func (s Stamp) Unchanged(other Stamp) bool {
	return s.Size == other.Size && s.ModUnix == other.ModUnix && s.ModNano == other.ModNano && s.Identity == other.Identity
}

// Entry is one file the sweep found.
type Entry struct {
	// Path is absolute, as this machine spells it. It is half of the
	// (machine, path) key the engine versions re-pushes on, so it must be
	// stable between sweeps -- which is why it is never cleaned into a
	// different spelling than the one that was first reported.
	Path  string
	Rel   string
	Stamp Stamp
}

// ScanResult is one pass over one watched folder.
type ScanResult struct {
	RootIdentity string
	Entries      []Entry
	Bytes        int64
	// Skipped counts entries the walk could not stat or read. They are NOT
	// silently dropped: a folder half of which is unreadable is a different
	// answer from a folder that is fine, and the caller reports it.
	Skipped int
}

// defaultExcludes are the directories no backup wants and every developer
// machine has. Applied before the watch's own patterns.
//
// A FIXED LIST rather than a policy knob, because it is not a preference: a
// node_modules tree is derived bytes that regenerate from a lockfile, and
// somebody who genuinely wants one backed up can say so with a watch pointed
// inside it. Reusing the appsession package's list would have coupled two
// features that happen to agree today.
var defaultExcludes = []string{
	".git", "node_modules", "vendor", "target", "dist", "build",
	".next", ".cache", ".venv", "venv", "__pycache__", ".terraform",
	".gradle", ".idea", ".DS_Store",
}

// Scan walks root and returns every file that should be backed up.
//
// ORDER IS DETERMINISTIC. filepath.WalkDir already walks lexically, and the
// result is sorted again so a caller comparing two sweeps is comparing the
// same sequence. A push order that varied between runs would make a partial
// sweep resume somewhere different every time.
//
// A DIRECTORY IT CANNOT READ IS COUNTED, NOT FATAL. One unreadable subfolder
// must not stop the other nine hundred from being backed up, and the count is
// what lets the caller say so honestly rather than reporting a clean pass.
// `deny` is this machine's own veto, applied to EVERY directory and file --
// not just to the watched root. Checking only the root left the stated
// invariant ("a directory somebody marked as never-touch is not a directory to
// upload either") true of one path and false of everything beneath it: a root
// of `~` with hidden entries included would have walked `~/.ssh` straight into
// the Library. A nil deny denies nothing, which is only correct for a caller
// that has already decided (the tests); the sweeper always passes one.
func Scan(root string, excludes []string, includeHidden bool, deny func(string) error) (ScanResult, error) {
	return scanFolder(root, excludes, includeHidden, deny, true)
}
func scanFolder(root string, excludes []string, includeHidden bool, deny func(string) error, defaults bool) (ScanResult, error) {
	info, err := os.Stat(root)
	if err != nil {
		return ScanResult{}, err
	}
	if !info.IsDir() {
		return ScanResult{}, errors.New("not a folder")
	}

	out := ScanResult{RootIdentity: fileIdentity(info)}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be opened, or an entry that vanished
			// mid-walk. Both are ordinary on a live filesystem.
			out.Skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if defaults && isExcludedName(name) {
				return fs.SkipDir
			}
			if deny != nil && deny(path) != nil {
				// Pruned rather than skipped per-file: a denied directory has
				// nothing beneath it worth walking, and descending would list
				// the names of files the policy says not to touch.
				out.Skipped++
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks, sockets, devices. A backup copies FILES; following a
			// link would let a link inside a watched folder pull in anything
			// on the machine, which is the veto's whole point defeated from
			// the inside.
			return nil
		}
		// The excluded-NAME list applies to files too. `.DS_Store` is on it and
		// is a file, so with hidden entries included it was backed up despite
		// being named as something no backup wants.
		if defaults && isExcludedName(name) {
			return nil
		}
		if deny != nil && deny(path) != nil {
			out.Skipped++
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			out.Skipped++
			return nil
		}
		rel = filepath.ToSlash(rel)
		if matchesAny(rel, name, excludes) {
			return nil
		}
		fi, statErr := d.Info()
		if statErr != nil {
			out.Skipped++
			return nil
		}
		out.Entries = append(out.Entries, Entry{
			Path: path,
			Rel:  rel,
			Stamp: Stamp{
				Identity: fileIdentity(fi), Size: fi.Size(),
				ModUnix: fi.ModTime().UTC().Unix(), ModNano: fi.ModTime().UnixNano(),
			},
		})
		out.Bytes += fi.Size()
		return nil
	})
	if walkErr != nil {
		return out, walkErr
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Rel < out.Entries[j].Rel })
	return out, nil
}

func isExcludedName(name string) bool {
	for _, skip := range defaultExcludes {
		if name == skip {
			return true
		}
	}
	return false
}

// matchesAny applies the watch's own patterns to a path relative to the
// watched folder.
//
// Two forms, and the second is why this is not a bare filepath.Match:
//
//   - `*.tmp` matches the BASE NAME anywhere in the tree, which is what
//     somebody typing an extension means.
//   - `drafts/**` matches everything under a subfolder, which filepath.Match
//     cannot express at all (its `*` does not cross separators).
//
// Anything else is matched against the relative path as written. An
// unparseable pattern matches NOTHING rather than everything: a typo that
// silently excluded the whole folder would report a successful backup of no
// files.
func matchesAny(rel, base string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		// `**` on its own, and the `/**` spelling of it, both mean everything
		// beneath the watched folder. Without this, `/**` cut down to an empty
		// prefix that matched nothing at all -- so somebody typing the most
		// emphatic form of "exclude everything" excluded nothing.
		if pattern == "**" || pattern == "/**" {
			return true
		}
		if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
			continue
		}
		if !strings.ContainsAny(pattern, "/\\") {
			if ok, err := filepath.Match(pattern, base); err == nil && ok {
				return true
			}
			continue
		}
		if ok, err := filepath.Match(pattern, rel); err == nil && ok {
			return true
		}
	}
	return false
}

// Digest is the SHA-256 of a file, hex-encoded.
//
// STREAMED, never read whole. The files this feature exists for are client
// video; reading one into memory to hash it would take the machine down.
func Digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// statOf takes one file's cheap stamp, now.
//
// Separate from the walk's own stat because the two happen at very different
// moments: the walk builds the whole picture up front, and a push may reach a
// given file much later. Anything that DECLARES a size to the cluster has to
// declare the one it is about to read.
func statOf(path string) (Stamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Stamp{}, err
	}
	return Stamp{Identity: fileIdentity(fi), Size: fi.Size(), ModUnix: fi.ModTime().UTC().Unix(), ModNano: fi.ModTime().UnixNano()}, nil
}

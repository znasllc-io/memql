package worker

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// WorkersFile is the multi-home registry at ~/.memql/workers.yaml.
//
// One supervisor process (the LaunchAgent / systemd unit) connects every
// enabled home in parallel. Shared machine-side singletons stay single
// (consent socket, metrics :9100, policy, Ollama/inference); each home
// owns its own WorkerService.Stream.
//
// version is required so a future layout can migrate without guessing.
type WorkersFile struct {
	Version      int               `yaml:"version"`
	WorkerName   string            `yaml:"worker_name"`
	Labels       map[string]string `yaml:"labels,omitempty"`
	Concurrency  map[string]uint32 `yaml:"concurrency,omitempty"`
	StateDir     string            `yaml:"state_dir,omitempty"`
	LogLevel     string            `yaml:"log_level,omitempty"`
	Capabilities []string          `yaml:"capabilities,omitempty"`
	Homes        []Home            `yaml:"homes"`
}

// Home is one cluster enrollment on this machine.
type Home struct {
	ID         string `yaml:"id"`
	ClusterURL string `yaml:"cluster_url"`
	OSURL      string `yaml:"os_url,omitempty"`
	Token      string `yaml:"token"`
	Enabled    *bool  `yaml:"enabled,omitempty"`
}

// IsEnabled reports whether the home should be connected. Absent
// enabled: is treated as true so a hand-edited entry that only sets
// id/url/token still comes up.
func (h Home) IsEnabled() bool {
	if h.Enabled == nil {
		return true
	}
	return *h.Enabled
}

// DefaultWorkersPath returns ~/.memql/workers.yaml.
func DefaultWorkersPath() string {
	home := homeDir()
	if home == "" {
		return "workers.yaml"
	}
	return filepath.Join(home, ".memql", "workers.yaml")
}

// HomeIDFromURL derives a stable home id from a cluster URL's host.
// Used when migrating legacy worker.yaml and when an upsert does not
// name an id. Hosts are lowercased; a missing host falls back to
// "default".
func HomeIDFromURL(clusterURL string) string {
	raw := strings.TrimSpace(clusterURL)
	if raw == "" {
		return "default"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// url.Parse accepts bare hosts without a scheme as paths; try
		// again with a scheme so "api.example.com" still yields a host.
		u, err = url.Parse("https://" + raw)
		if err != nil || u.Host == "" {
			return "default"
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "default"
	}
	return host
}

// SuggestHomeID prefers a matching clusters.yaml slot name when one
// exists (so `worker config` and `cluster list` agree), otherwise the
// URL host. clusters may be nil.
func SuggestHomeID(clusterURL string, clusters *config.ClustersFile) string {
	hostID := HomeIDFromURL(clusterURL)
	if clusters == nil {
		return hostID
	}
	wantHost := strings.ToLower(hostOf(clusterURL))
	for _, c := range clusters.Clusters {
		candidates := []string{c.Endpoint, c.Domain, c.Name, c.DisplayName}
		for _, cand := range candidates {
			if cand == "" {
				continue
			}
			if strings.EqualFold(cand, wantHost) || strings.Contains(strings.ToLower(cand), wantHost) ||
				strings.Contains(wantHost, strings.ToLower(hostOf(cand))) {
				name := strings.TrimSpace(c.Name)
				if name != "" {
					return name
				}
			}
		}
	}
	return hostID
}

// LoadWorkers loads the multi-home registry.
//
// Preference order:
//  1. workers.yaml at workersPath when present
//  2. migrate legacy worker.yaml at legacyPath into workers.yaml
//     (leave the legacy file in place; never wipe tokens)
//  3. empty defaults (no homes) when neither file exists
//
// workersPath / legacyPath empty → DefaultWorkersPath / DefaultConfigPath.
func LoadWorkers(workersPath, legacyPath string) (WorkersFile, error) {
	if workersPath == "" {
		workersPath = DefaultWorkersPath()
	}
	if legacyPath == "" {
		legacyPath = DefaultConfigPath()
	}
	if _, err := os.Stat(workersPath); err == nil {
		return loadWorkersFile(workersPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return WorkersFile{}, fmt.Errorf("workers config: stat %s: %w", workersPath, err)
	}

	legacy, err := LoadFile(legacyPath)
	if err != nil {
		return WorkersFile{}, err
	}
	if strings.TrimSpace(legacy.ClusterURL) == "" && strings.TrimSpace(legacy.Token) == "" {
		return emptyWorkers(), nil
	}
	migrated := workersFromLegacy(legacy)
	if err := SaveWorkers(workersPath, migrated); err != nil {
		return WorkersFile{}, fmt.Errorf("workers config: migrate from %s: %w", legacyPath, err)
	}
	return migrated, nil
}

func emptyWorkers() WorkersFile {
	d := Defaults()
	return WorkersFile{
		Version:      1,
		WorkerName:   d.Name,
		Labels:       d.Labels,
		Concurrency:  d.Concurrency,
		StateDir:     d.StateDir,
		LogLevel:     d.LogLevel,
		Capabilities: d.Capabilities,
		Homes:        nil,
	}
}

func workersFromLegacy(legacy Config) WorkersFile {
	enabled := true
	// Spec: stable id from URL host. UpsertHome may later rename via
	// SuggestHomeID when the operator pairs with an explicit home id.
	id := HomeIDFromURL(legacy.ClusterURL)
	w := WorkersFile{
		Version:      1,
		WorkerName:   legacy.Name,
		Labels:       legacy.Labels,
		Concurrency:  legacy.Concurrency,
		StateDir:     legacy.StateDir,
		LogLevel:     legacy.LogLevel,
		Capabilities: legacy.Capabilities,
		Homes: []Home{{
			ID:         id,
			ClusterURL: legacy.ClusterURL,
			Token:      legacy.Token,
			Enabled:    &enabled,
		}},
	}
	return normalizeWorkers(w)
}

func loadWorkersFile(path string) (WorkersFile, error) {
	if err := config.VerifyCredentialFileMode(path); err != nil {
		return WorkersFile{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkersFile{}, fmt.Errorf("workers config: read %s: %w", path, err)
	}
	var w WorkersFile
	if err := yaml.Unmarshal(data, &w); err != nil {
		return WorkersFile{}, fmt.Errorf("workers config: parse %s: %w", path, err)
	}
	w = normalizeWorkers(w)
	return w, nil
}

func normalizeWorkers(w WorkersFile) WorkersFile {
	d := Defaults()
	if w.Version == 0 {
		w.Version = 1
	}
	if w.WorkerName == "" {
		w.WorkerName = d.Name
	}
	if w.Labels == nil {
		w.Labels = d.Labels
	}
	if w.Concurrency == nil {
		w.Concurrency = d.Concurrency
	}
	if w.StateDir == "" {
		w.StateDir = d.StateDir
	}
	if w.LogLevel == "" {
		w.LogLevel = d.LogLevel
	}
	if len(w.Capabilities) == 0 {
		w.Capabilities = d.Capabilities
	}
	return w
}

// SaveWorkers writes workers.yaml at 0600. Never truncates tokens: the
// caller is responsible for merging before save.
func SaveWorkers(path string, w WorkersFile) error {
	if path == "" {
		path = DefaultWorkersPath()
	}
	w = normalizeWorkers(w)
	if w.Version == 0 {
		w.Version = 1
	}
	if err := w.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("workers.yaml: mkdir: %w", err)
	}
	body, err := yaml.Marshal(w)
	if err != nil {
		return fmt.Errorf("workers.yaml: marshal: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("workers.yaml: write: %w", err)
	}
	return nil
}

// Validate checks the registry shape. An empty homes list is allowed
// (fresh machine); ValidateRun refuses to start with zero enabled homes.
//
// Two homes on one cluster are NOT a shape error: runnableHomes decides
// which of them connects. Refusing the file here would refuse it to
// SaveWorkers too, and then `worker pair` and `worker unpair` -- the two
// commands that fix it -- would fail on exactly the machines that need
// them.
func (w WorkersFile) Validate() error {
	if w.Version != 1 {
		return fmt.Errorf("workers config: unsupported version %d (want 1)", w.Version)
	}
	if strings.TrimSpace(w.WorkerName) == "" {
		return errors.New("workers config: worker_name required")
	}
	hasHeadless := false
	for _, cap := range w.Capabilities {
		if cap == "HEADLESS" {
			hasHeadless = true
			break
		}
	}
	if !hasHeadless {
		return errors.New("workers config: HEADLESS capability is mandatory")
	}
	seen := map[string]struct{}{}
	for i, h := range w.Homes {
		id := strings.TrimSpace(h.ID)
		if id == "" {
			return fmt.Errorf("workers config: homes[%d]: id required", i)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("workers config: duplicate home id %q", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(h.ClusterURL) == "" {
			return fmt.Errorf("workers config: home %q: cluster_url required", id)
		}
		tok := strings.TrimSpace(h.Token)
		if tok == "" {
			return fmt.Errorf("workers config: home %q: token required", id)
		}
		if !strings.HasPrefix(tok, "mql_wkr_") {
			return fmt.Errorf("workers config: home %q: token must start with mql_wkr_", id)
		}
	}
	return nil
}

// duplicateHome is an enabled home that does not connect, because a later
// enabled home points at the same cluster.
type duplicateHome struct {
	Home     Home
	Connects string // the id of the home that connects instead
}

// runnableHomes is the enabled homes this machine connects: ONE PER
// CLUSTER (memql-cockpit#433).
//
// Two streams from one machine to one cluster register the same machine
// twice, under the same machine id, and the cluster's row for it flaps
// between them forever. So when two enabled homes point at one cluster
// (sameClusterURL), one of them connects and the other is reported rather
// than dialled -- the LATER entry, because UpsertHome and the installers
// both append a new enrollment at the end, so the later entry holds the
// token somebody issued most recently. A duplicate comes from a hand edit,
// or from an installer before it matched on the host, and the sentence the
// fleet logs and `worker config` prints says which entry to remove.
func (w WorkersFile) runnableHomes() ([]Home, []duplicateHome) {
	enabled := w.EnabledHomes()
	var run []Home
	var skipped []duplicateHome
	for i, h := range enabled {
		winner := i
		for j := len(enabled) - 1; j > i; j-- {
			if sameClusterURL(enabled[j].ClusterURL, h.ClusterURL) {
				winner = j
				break
			}
		}
		if winner == i {
			run = append(run, h)
			continue
		}
		skipped = append(skipped, duplicateHome{Home: h, Connects: enabled[winner].ID})
	}
	return run, skipped
}

// duplicateHomeSentence is what the fleet logs, once, for a home it does
// not connect.
func duplicateHomeSentence(d duplicateHome) string {
	return fmt.Sprintf("homes %q and %q both point at %s; one machine holds one stream per cluster, so only %q connects -- remove %q from workers.yaml, or set enabled: false on it",
		d.Home.ID, d.Connects, d.Home.ClusterURL, d.Connects, d.Home.ID)
}

// ValidateRun accepts paused enrollments so the local control service can resume them.
func (w WorkersFile) ValidateRun() error {
	if err := w.Validate(); err != nil {
		return err
	}
	if len(w.Homes) == 0 {
		return errors.New("workers config: no homes (pair a cluster)")
	}
	return nil
}

// EnabledHomes returns homes with enabled unset or true.
func (w WorkersFile) EnabledHomes() []Home {
	out := make([]Home, 0, len(w.Homes))
	for _, h := range w.Homes {
		if h.IsEnabled() {
			out = append(out, h)
		}
	}
	return out
}

// ConfigForHome projects shared defaults + one home into the single-home
// Config the Runner already understands.
//
// StateDir is namespaced per home as <state_dir>/homes/<sanitized-id>
// so backup registration files, appsession ledgers, and other per-home
// artifacts cannot clobber siblings. Existing single-home content that
// lived directly under state_dir is left in place; the next RegisterAck
// / session write populates the namespaced path. sanitizeHomeID keeps
// the id filesystem-safe (home ids are usually DNS hosts).
func (w WorkersFile) ConfigForHome(h Home) Config {
	stateDir := w.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	stateDir = filepath.Join(stateDir, homesDirName, sanitizeHomeID(h.ID))
	return Config{
		ClusterURL:   h.ClusterURL,
		Token:        h.Token,
		Name:         w.WorkerName,
		Labels:       cloneStringMap(w.Labels),
		Concurrency:  cloneUint32Map(w.Concurrency),
		StateDir:     stateDir,
		LogLevel:     w.LogLevel,
		Capabilities: append([]string(nil), w.Capabilities...),
		Home:         h.ID,
	}
}

// machineRoot is the machine-level state directory every home's own state
// dir sits under -- where this machine's id lives (machineid.go).
func (w WorkersFile) machineRoot() string {
	if strings.TrimSpace(w.StateDir) == "" {
		return defaultStateDir()
	}
	return filepath.Clean(w.StateDir)
}

// sanitizeHomeID maps a home id to a single path segment. Empty becomes
// "default"; path separators and ".." are rejected so a hand-edited id
// cannot escape the homes/ tree.
func sanitizeHomeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "default"
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "default"
	}
	return out
}

// UpsertHomeOptions controls UpsertHome.
type UpsertHomeOptions struct {
	WorkersPath  string
	LegacyPath   string
	ID           string // empty → SuggestHomeID / HomeIDFromURL
	ClusterURL   string
	Token        string
	Name         string // updates worker_name when non-empty
	Force        bool   // remap same home id onto a different cluster_url; not required for same-URL refresh
	Capabilities []string
}

// UpsertHome adds or updates one home in workers.yaml without clobbering
// siblings.
//
// Same cluster_url OR same home id with the same URL refreshes the token
// without --force (and preserves the enrolled id when the suggested id
// differs — e.g. install derives the URL host while pair --home-id uses
// a clusters.yaml slot name). --force is required only to remap an
// existing home id onto a *different* cluster_url.
//
// Match order: cluster_url first (host-normalized), else explicit id.
func UpsertHome(opts UpsertHomeOptions) (WorkersFile, error) {
	workersPath := opts.WorkersPath
	if workersPath == "" {
		workersPath = DefaultWorkersPath()
	}
	legacyPath := opts.LegacyPath
	if legacyPath == "" {
		legacyPath = DefaultConfigPath()
	}
	clusterURL := strings.TrimSpace(opts.ClusterURL)
	token := strings.TrimSpace(opts.Token)
	if clusterURL == "" {
		return WorkersFile{}, errors.New("upsert home: cluster_url required")
	}
	if token == "" {
		return WorkersFile{}, errors.New("upsert home: token required")
	}
	if !strings.HasPrefix(token, "mql_wkr_") {
		return WorkersFile{}, errors.New("upsert home: token must start with mql_wkr_")
	}

	w, err := LoadWorkers(workersPath, legacyPath)
	if err != nil {
		return WorkersFile{}, err
	}
	// LoadWorkers migrates legacy → workers.yaml when needed; if both
	// were absent we get empty defaults.
	if _, statErr := os.Stat(workersPath); errors.Is(statErr, os.ErrNotExist) && len(w.Homes) == 0 {
		w = emptyWorkers()
	}

	id := strings.TrimSpace(opts.ID)
	if id == "" {
		if clusters, cerr := config.LoadClusters(); cerr == nil {
			id = SuggestHomeID(clusterURL, clusters)
		} else {
			id = HomeIDFromURL(clusterURL)
		}
	}

	enabled := true
	newHome := Home{
		ID:         id,
		ClusterURL: clusterURL,
		Token:      token,
		Enabled:    &enabled,
	}

	// Match by id first, else by cluster_url. Same URL under a
	// different enrolled id (install host-id vs pair --home-id local)
	// refreshes in place without --force and preserves the enrolled id.
	idxByID, idxByURL := -1, -1
	for i, h := range w.Homes {
		if h.ID == id {
			idxByID = i
		}
		if sameClusterURL(h.ClusterURL, clusterURL) {
			idxByURL = i
		}
	}
	switch {
	case idxByURL >= 0:
		// Same cluster_url → token refresh. Preserve enrolled id unless
		// --force + explicit ID renames the slot.
		prev := w.Homes[idxByURL]
		newHome.ID = prev.ID
		if opts.Force && strings.TrimSpace(opts.ID) != "" {
			newHome.ID = id
		}
		w.Homes[idxByURL] = newHome
		newHome = w.Homes[idxByURL]
	case idxByID >= 0:
		// Same id, different cluster_url → cluster identity remap.
		prev := w.Homes[idxByID]
		if !sameClusterURL(prev.ClusterURL, clusterURL) && !opts.Force {
			return WorkersFile{}, fmt.Errorf("upsert home: home id %q already exists for %s; pass --force to remap that home to %s (siblings are preserved)", id, prev.ClusterURL, clusterURL)
		}
		w.Homes[idxByID] = newHome
	default:
		w.Homes = append(w.Homes, newHome)
	}

	if name := strings.TrimSpace(opts.Name); name != "" {
		w.WorkerName = name
	}
	if len(opts.Capabilities) > 0 {
		w.Capabilities = append([]string(nil), opts.Capabilities...)
	} else if len(w.Capabilities) == 0 {
		w.Capabilities = capabilitiesForBuildTag()
	}
	if w.Labels == nil {
		hostname, _ := os.Hostname()
		w.Labels = map[string]string{
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"hostname": hostname,
		}
	}

	if err := SaveWorkers(workersPath, w); err != nil {
		return WorkersFile{}, err
	}
	// Keep legacy worker.yaml as a mirror of the upserted home so older
	// tooling and hand-rolled LaunchAgent overrides that still read it
	// keep working during the transition. Never delete sibling state.
	_ = WriteLegacyWorkerYAML(legacyPath, w.ConfigForHome(newHome))
	return w, nil
}

// MirrorOutcome is what RemoveHome did to the legacy worker.yaml mirror.
type MirrorOutcome int

const (
	// MirrorAbsent: there is no worker.yaml.
	MirrorAbsent MirrorOutcome = iota
	// MirrorUntouched: worker.yaml already names an enabled home, with the
	// token that home holds.
	MirrorUntouched
	// MirrorRewritten: worker.yaml named no enabled home, and now names
	// MirrorHome.
	MirrorRewritten
	// MirrorDeleted: worker.yaml named no enabled home, and none is left
	// for it to name.
	MirrorDeleted
)

// RemoveHomeResult is what unpair did, for the sentences the CLI prints.
type RemoveHomeResult struct {
	Workers WorkersFile
	Removed Home
	Mirror  MirrorOutcome
	// MirrorHome is the home worker.yaml names after MirrorRewritten.
	MirrorHome string
}

// RemoveHome deletes a home by id (or disables when disableOnly), and
// keeps the legacy worker.yaml mirror from naming it (memql-cockpit#429).
//
// THE MIRROR IS PART OF UNPAIRING. UpsertHome copies each paired home's
// token into worker.yaml for older tooling, so removing a home from
// workers.yaml alone left its token one file over -- and a worker that
// fell back to that file brought the cluster straight back after the
// person had unpaired it. See syncLegacyMirror.
//
// legacyPath empty means DefaultConfigPath(). If workers.yaml is saved and
// the mirror cannot be put right, the error says so: the result is still
// returned, and the token the person meant to remove is still on disk.
func RemoveHome(workersPath, legacyPath, id string, disableOnly bool) (RemoveHomeResult, error) {
	if workersPath == "" {
		workersPath = DefaultWorkersPath()
	}
	if legacyPath == "" {
		legacyPath = DefaultConfigPath()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return RemoveHomeResult{}, errors.New("unpair: home id required")
	}
	w, err := LoadWorkers(workersPath, legacyPath)
	if err != nil {
		return RemoveHomeResult{}, err
	}
	idx := -1
	for i, h := range w.Homes {
		if h.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return RemoveHomeResult{}, fmt.Errorf("unpair: home %q not found", id)
	}
	removed := w.Homes[idx]
	if disableOnly {
		off := false
		w.Homes[idx].Enabled = &off
	} else {
		w.Homes = append(w.Homes[:idx], w.Homes[idx+1:]...)
	}
	if err := SaveWorkers(workersPath, w); err != nil {
		return RemoveHomeResult{}, err
	}
	res := RemoveHomeResult{Workers: w, Removed: removed}
	res.Mirror, res.MirrorHome, err = syncLegacyMirror(legacyPath, w)
	if err != nil {
		return res, fmt.Errorf("unpair: %s is out of workers.yaml, but the legacy mirror %s still holds its token: %w", removed.ID, legacyPath, err)
	}
	return res, nil
}

// mirrorPathFor is where UpsertHome and the installers keep the legacy
// mirror of the registry at workersPath: the worker.yaml beside it.
func mirrorPathFor(workersPath string) string {
	return filepath.Join(filepath.Dir(workersPath), "worker.yaml")
}

// isMirrorOf reports whether configPath IS that mirror -- the only
// worker.yaml anything here may rewrite or delete. A worker.yaml somewhere
// else was named by a person (`worker run --config`), and it is theirs.
func isMirrorOf(configPath, workersPath string) bool {
	a, errA := filepath.Abs(configPath)
	b, errB := filepath.Abs(mirrorPathFor(workersPath))
	return errA == nil && errB == nil && a == b
}

// syncLegacyMirror keeps worker.yaml naming a home that is enabled, with
// the token that home holds now -- or gone.
//
// A mirror that matches an enabled home is left exactly as it is. One that
// names anything else -- a home just removed or disabled, a home an older
// build unpaired and left behind, a token since replaced -- is rewritten
// to the first home that connects, or deleted when none does. Disabling
// counts as going: a disabled home must not be connectable from a file
// nobody is looking at. `worker run` calls this at startup too, so a
// token an earlier unpair left one file over does not stay on disk
// forever.
func syncLegacyMirror(legacyPath string, w WorkersFile) (MirrorOutcome, string, error) {
	if _, err := os.Stat(legacyPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MirrorAbsent, "", nil
		}
		return MirrorUntouched, "", err
	}
	mirror, err := LoadFile(legacyPath)
	if err != nil {
		return MirrorUntouched, "", err
	}
	for _, h := range w.EnabledHomes() {
		if strings.TrimSpace(mirror.Token) == strings.TrimSpace(h.Token) && sameClusterURL(mirror.ClusterURL, h.ClusterURL) {
			return MirrorUntouched, "", nil
		}
	}
	if run, _ := w.runnableHomes(); len(run) > 0 {
		if err := WriteLegacyWorkerYAML(legacyPath, w.ConfigForHome(run[0])); err != nil {
			return MirrorUntouched, "", err
		}
		return MirrorRewritten, run[0].ID, nil
	}
	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MirrorUntouched, "", err
	}
	return MirrorDeleted, "", nil
}

// WriteLegacyWorkerYAML writes the single-home worker.yaml shape.
func WriteLegacyWorkerYAML(path string, cfg Config) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("worker.yaml: mkdir: %w", err)
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("worker.yaml: marshal: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("worker.yaml: write: %w", err)
	}
	return nil
}

func sameClusterURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/")) ||
		strings.EqualFold(HomeIDFromURL(a), HomeIDFromURL(b)) && HomeIDFromURL(a) != "default"
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneUint32Map(in map[string]uint32) map[string]uint32 {
	if in == nil {
		return nil
	}
	out := make(map[string]uint32, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

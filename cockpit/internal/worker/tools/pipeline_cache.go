package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type pipelineCacheHomeKey struct{}

var pipelineCacheHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Retention budget between builds, not a filesystem quota during a command.
// The host build lock serializes pruning across processes: only the process
// holding it runs steps, and it holds it while any slot is held
// (pipeline_capacity_unix.go). Within that process pipelineCaches serializes
// it and counts each cache directory's users.
var pipelineCacheRetainedBytes int64 = 10 << 30

// pipelineCaches counts the running steps mounting each cache directory, by
// path. A machine running several steps (pipelines.max_concurrent_steps) may
// prune while another step's container writes into its cache: a directory with
// users is neither measured nor removed, and two steps of the same cache scope
// share one directory, as cluster pods share theirs under the shared users lock
// (integrations/pipelinesteps/wrapper.go). The Go and npm caches are built for
// concurrent writers. This process is the only one running steps while it
// holds the host build lock, so this count is every user the machine has.
var pipelineCaches = struct {
	sync.Mutex
	users map[string]int
}{users: map[string]int{}}

var pipelineCacheEnvironment = map[string]map[string]string{
	"go":  {"GOMODCACHE": "/cache/go/mod", "GOCACHE": "/cache/go/build"},
	"npm": {"npm_config_cache": "/cache/npm"},
}

func parsePipelineCaches(args map[string]any, req *pipelineStepRequest) error {
	raw, present := args["caches"]
	if !present {
		return nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) > 2 {
		return errors.New("pipeline_step: caches must list go and/or npm")
	}
	for _, value := range list {
		name, ok := value.(string)
		if !ok || pipelineCacheEnvironment[name] == nil {
			return errors.New("pipeline_step: unknown cache")
		}
		for _, existing := range req.caches {
			if name == existing {
				return errors.New("pipeline_step: repeated cache")
			}
		}
		req.caches = append(req.caches, name)
	}
	if len(req.caches) == 0 {
		return nil
	}
	if req.execution != "container" {
		return errors.New("pipeline_step: managed caches require container execution")
	}
	req.cacheScope = argString(args, "cacheScope")
	if !pipelineCacheHash.MatchString(req.cacheScope) {
		return errors.New("pipeline_step: caches require the runner's owner/repository/trust scope")
	}
	return nil
}

func pipelineCacheIdentity(req pipelineStepRequest) string {
	// The engine scope separates owner, repository and event trust. Local
	// enrollment binding prevents two clusters from selecting each other's
	// scope; clone host, architecture, image and UID/GID prevent incompatible
	// toolchains or repository hosts from sharing one writable tree.
	data, _ := json.Marshal([]string{req.cacheHome, req.cacheScope, strings.ToLower(req.cloneURL), req.platform, req.image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func pipelineCachesRoot() (string, error) {
	root, err := pipelineCapacityRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "caches"), nil
}

func ensurePipelineCacheDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("pipeline cache path is not a real directory")
	}
	return nil
}

// preparePipelineCache selects req's cache directory and counts the step as
// one of its users until release is called. The directory is counted before
// pipelineCaches is unlocked, so no prune between here and the container
// mounting it can remove it.
func preparePipelineCache(req *pipelineStepRequest) (release func(), err error) {
	root, err := pipelineCachesRoot()
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(root, ",\n\r") {
		return nil, errors.New("pipeline cache path contains an unsupported Docker delimiter")
	}
	pipelineCaches.Lock()
	defer pipelineCaches.Unlock()
	if err = ensurePipelineCacheDirectory(root); err != nil {
		return nil, err
	}
	if err = prunePipelineCachesLocked(); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, pipelineCacheIdentity(*req))
	if err = ensurePipelineCacheDirectory(dir); err != nil {
		return nil, err
	}
	now := time.Now()
	if err = os.Chtimes(dir, now, now); err != nil {
		return nil, err
	}
	req.cacheDir = dir
	pipelineCaches.users[dir]++
	var once sync.Once
	return func() {
		once.Do(func() {
			pipelineCaches.Lock()
			defer pipelineCaches.Unlock()
			if pipelineCaches.users[dir]--; pipelineCaches.users[dir] <= 0 {
				delete(pipelineCaches.users, dir)
			}
		})
	}, nil
}

func prunePipelineCaches() error {
	pipelineCaches.Lock()
	defer pipelineCaches.Unlock()
	return prunePipelineCachesLocked()
}

// prunePipelineCachesLocked keeps the retained caches within their budget,
// oldest first, leaving alone every directory a running step mounts. It does
// not run while a slot holds an interrupted attempt: that attempt's container
// may still be writing into a cache nobody counts any more. The caller holds
// pipelineCaches' mutex.
func prunePipelineCachesLocked() error {
	if pipelineInterruptedAttempt() {
		return nil
	}
	root, err := pipelineCachesRoot()
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("pipeline cache root is not a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type cache struct {
		path     string
		bytes    int64
		modified time.Time
	}
	var caches []cache
	var total int64
	for _, entry := range entries {
		if !pipelineCacheHash.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected entry in pipeline cache root; operator inspection required")
		}
		item := cache{path: filepath.Join(root, entry.Name())}
		if pipelineCaches.users[item.path] > 0 {
			// A running step mounts it: its size is changing under the walk
			// and it is not this prune's to remove. A later prune, once it is
			// idle, counts it against the budget.
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item.modified = info.ModTime()
		err = filepath.WalkDir(item.path, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type().IsRegular() {
				stat, err := d.Info()
				if err != nil {
					return err
				}
				item.bytes += stat.Size()
			}
			return nil // never follow links created inside a container
		})
		if err != nil {
			return err
		}
		total += item.bytes
		caches = append(caches, item)
	}
	sort.Slice(caches, func(i, j int) bool { return caches[i].modified.Before(caches[j].modified) })
	for _, item := range caches {
		if total <= pipelineCacheRetainedBytes {
			break
		}
		removeStepDir(item.path)
		if _, err := os.Lstat(item.path); !os.IsNotExist(err) {
			return errors.New("cannot remove an expired pipeline cache")
		}
		total -= item.bytes
	}
	return nil
}

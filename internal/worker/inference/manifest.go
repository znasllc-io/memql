package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifest.go answers the disk preflight's one question before a pull
// starts: how many bytes of this model are still missing here
// (memql-cockpit#524).
//
// The check used to run mid-pull, against a free-space figure gathered
// before the first model, adding up every layer's full size -- so a
// resumed pull whose layers were already on disk was refused for space it
// did not need, and a second model was judged against space the first had
// already used. The registry states a model's layers and sizes before a
// byte moves, and the runtime's blobs directory says which are already
// complete, so the difference is knowable up front.
//
// Every failure answers "not known", which refuses nothing: a preflight
// that blocked a pull because a registry was slow would be worse than the
// mid-pull check it replaces, which still runs.

// registryBase is the Ollama library's registry. A variable so the tests
// can serve a fake one over loopback; nothing else assigns it.
var registryBase = "https://registry.ollama.ai"

// manifestTimeout bounds the one request, for the reason probeTimeout
// bounds a probe: a person is waiting in front of it.
const manifestTimeout = 5 * time.Second

// manifestURL names the manifest for a model id, or "" for one this
// cannot resolve. Ollama's own spellings: `name[:tag]` is the library,
// `ns/name[:tag]` a user namespace, `host/ns/name[:tag]` another registry
// (hf.co among them). An absent tag is `latest`.
func manifestURL(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, " \t\r\n?#") {
		return ""
	}
	name, tag := id, "latest"
	if i := strings.LastIndex(id, ":"); i > strings.LastIndex(id, "/") {
		name, tag = id[:i], id[i+1:]
	}
	if name == "" || tag == "" {
		return ""
	}
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 1:
		return registryBase + "/v2/library/" + name + "/manifests/" + tag
	case len(parts) >= 3 && strings.Contains(parts[0], "."):
		return "https://" + parts[0] + "/v2/" + strings.Join(parts[1:], "/") + "/manifests/" + tag
	case len(parts) == 2:
		return registryBase + "/v2/" + name + "/manifests/" + tag
	}
	return ""
}

type manifestBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// MissingBytes is how many bytes of model id are not yet complete under
// modelsDir (OLLAMA_MODELS: the directory holding blobs/ and
// manifests/), and whether that could be established at all.
//
// A blob counts as present only when the complete file is there. A
// partial download counts as missing in full: the preflight errs toward
// asking for slightly more room, never toward a pull that runs out of
// disk halfway.
func MissingBytes(ctx context.Context, modelsDir, id string) (uint64, bool) {
	url := manifestURL(id)
	if url == "" || strings.TrimSpace(modelsDir) == "" {
		return 0, false
	}
	blobs := filepath.Join(modelsDir, "blobs")
	// A blobs directory that exists and cannot be read (a container's
	// root-owned volume) says nothing about what is on it.
	if _, err := os.ReadDir(blobs); err != nil && !os.IsNotExist(err) {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var m struct {
		Config manifestBlob   `json:"config"`
		Layers []manifestBlob `json:"layers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil || len(m.Layers) == 0 {
		return 0, false
	}
	var missing uint64
	for _, b := range append([]manifestBlob{m.Config}, m.Layers...) {
		if b.Size <= 0 || b.Digest == "" {
			continue
		}
		hex, ok := strings.CutPrefix(b.Digest, "sha256:")
		if !ok || strings.ContainsAny(hex, "/\\.") {
			return 0, false
		}
		if fi, err := os.Stat(filepath.Join(blobs, "sha256-"+hex)); err == nil && fi.Size() == b.Size {
			continue
		}
		missing += uint64(b.Size)
	}
	return missing, true
}

// DiskMargin is the room kept free beyond the missing bytes: a gigabyte,
// or a twentieth of the download when that is larger. The runtime writes
// manifests and temporary chunks beside the blobs, and a volume filled to
// the last byte stops the machine, not just the pull.
func DiskMargin(missing uint64) uint64 {
	const gb = 1_000_000_000
	if m := missing / 20; m > gb {
		return m
	}
	return gb
}

// DiskShortfall is the preflight's sentence, or "" when the room is there
// or cannot be established. It names both numbers, as the mid-pull
// refusal does.
func DiskShortfall(missing, free uint64, where string) string {
	if missing == 0 || free == 0 {
		return ""
	}
	need := missing + DiskMargin(missing)
	if need <= free {
		return ""
	}
	return fmt.Sprintf("There is not enough disk for these models: they need about %s more, counting room to spare, and %s is free where this machine keeps models (%s).",
		humanGBDecimal(need), humanGBDecimal(free), where)
}

func humanGBDecimal(n uint64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

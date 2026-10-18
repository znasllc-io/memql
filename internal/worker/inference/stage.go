package inference

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// stage.go is the half of a native Linux install that is not a command
// line: the vendor's release archive, its checksum, its unpacking, and
// the unit file that keeps it running. install.go runs it after the same
// consent and before the first command, through a seam, so the whole
// flow is exercisable on a runner with no network and no GPU.
//
// WHY THIS EXISTS AT ALL. The first Linux path was the ollama/ollama
// container with the GPU passed through (design D1 of the 2026-09-06
// record). It stopped on every fresh machine at the same place: Docker
// cannot see an NVIDIA GPU without the container toolkit, the toolkit is
// a root install, on Pop!_OS 24.04 it is not in any configured repository
// at all, and NVIDIA's own steps end in a Docker restart -- which on a
// developer machine restarts the k3d cluster running beside it. The
// cockpit never runs sudo, so the command printed three root commands
// and stopped, and "install end to end" was not something it could do.
//
// Ollama's Linux release is a self-contained archive: one binary and its
// CUDA and ROCm libraries. Unpacked under the person's own home and kept
// running by a USER systemd unit, it reaches the GPU through the same
// device nodes nvidia-smi already used to pass the hardware floor, and
// needs nothing from root: no toolkit, no daemon restart, no package
// repository, and no Docker. That is what the 2026-09-08 record rules
// (its D1), and this file is the mechanism.
//
// NEVER `curl | sh`, STILL. Ollama's own install.sh does the same
// download, as root into /usr/local, after installing the NVIDIA driver
// if it feels like it. This fetches the archive it names, checks it
// against the release's own sha256sum.txt, and unpacks it into a
// directory this process may already write -- and every path in the
// archive is checked to land inside that directory before it is
// created. A release archive is data; a script is not.

// NativeFacts is what the Linux platform file observed about running
// Ollama as this user, and where it would go. Assembled by Gather on a
// real machine and by DefaultNativeFacts in the tests.
type NativeFacts struct {
	// Vendor is whose GPU the native runtime would drive, by the same
	// probes the Docker facts use.
	Vendor GPUVendor
	// Devices reports that this user can open the vendor's device nodes:
	// /dev/nvidiactl, or /dev/kfd and a /dev/dri render node. A user
	// outside the render group has a working GPU and no way to reach
	// it, and the refusal says so rather than letting Ollama fall back
	// to the CPU -- which is what the hardware floor exists to prevent.
	Devices bool
	// DevicesReason is why Devices is false, as a complete sentence.
	DevicesReason string
	// Installed reports that RuntimeDir already holds bin/ollama from an
	// earlier run, so the archives need not be fetched again.
	Installed bool
	// RuntimeDir, ModelsDir, LogPath and UnitPath are where the runtime
	// lands. See DefaultNativeFacts.
	RuntimeDir, ModelsDir, LogPath, UnitPath string
	// FreeDisk is bytes available on RuntimeDir's volume, for the
	// archive and its unpacked contents. Zero means "not established".
	FreeDisk uint64
}

// DefaultNativeFacts is the layout under one home directory: everything
// inside the ~/.memql fence the uninstaller already owns, except the unit
// file, which lives where systemd looks for user units.
//
//	~/.memql/ollama/runtime   bin/ollama, lib/ollama -- replaced on unpack
//	~/.memql/ollama/models    OLLAMA_MODELS, beside the runtime, never under it
//	~/.memql/state/ollama.log
//	~/.config/systemd/user/memql-ollama.service
//
// OLLAMA_MODELS in the environment wins for the models, because a person
// with a small internal disk and a large external one has set it for
// exactly this, and reading the fence anyway would fill the wrong disk.
func DefaultNativeFacts(home string, modelsOverride string) NativeFacts {
	models := filepath.Join(home, ".memql", "ollama", "models")
	if strings.TrimSpace(modelsOverride) != "" {
		models = modelsOverride
	}
	return NativeFacts{
		RuntimeDir: filepath.Join(home, ".memql", "ollama", "runtime"),
		ModelsDir:  models,
		LogPath:    filepath.Join(home, ".memql", "state", "ollama.log"),
		UnitPath:   filepath.Join(home, ".config", "systemd", "user", OllamaUnitName),
	}
}

// Stage is what a native install does BESIDE its commands, shown to the
// person as sentences above them and run after the same yes.
type Stage struct {
	// Archives are fetched and unpacked in order into RuntimeDir: the
	// base archive, then the ROCm add-on on an AMD machine. Empty when
	// Reuse is set.
	Archives []Archive
	// Reuse says the runtime is already unpacked at RuntimeDir from an
	// earlier run that did not finish, so the archives are not fetched
	// again; only the unit is written and the commands run.
	Reuse bool
	// RuntimeDir is where bin/ollama and lib/ollama land. It is REPLACED
	// on a fresh unpack, which is why the models live beside it and not
	// under it.
	RuntimeDir string
	// ModelsDir is OLLAMA_MODELS for the unit: inside the ~/.memql fence,
	// so the uninstaller's --purge takes the models with the runtime and
	// nothing outside the fence is ever touched.
	ModelsDir string
	// LogPath is where the unit appends Ollama's own output.
	LogPath string
	// UnitPath is the user unit this writes.
	UnitPath string
	// Listen is the address the unit binds. Always the loopback: Ollama
	// has no authentication and nothing off-box needs the port -- the
	// cluster reaches this machine over the worker's outbound stream.
	Listen string
	// Parallel is the requests Ollama takes at once per model, written as
	// OLLAMA_NUM_PARALLEL. Zero and one write nothing: one is Ollama's own
	// default, and a unit that says it again would only be a second place
	// for the number to be wrong. See DefaultParallel.
	Parallel int
}

// Archive is one release asset.
type Archive struct {
	// Name is the asset's file name, which is also its line in
	// sha256sum.txt.
	Name string
	// Release is the tag the URLs are pinned to, or "latest" when the
	// stager resolves it at run time. A pinned tag is what keeps the
	// archive and the checksum file from coming from two different
	// releases published minutes apart.
	Release string
}

// The vendor's release layout, verified on 2026-09-08 against
// github.com/ollama/ollama v0.33.3: `ollama-linux-<arch>.tar.zst` carrying
// bin/ollama and lib/ollama, `ollama-linux-amd64-rocm.tar.zst` carrying
// the ROCm libraries to unpack over it, and `sha256sum.txt` listing every
// asset as `<hex>  ./<name>`. The `.tgz` names an earlier record used are
// gone -- https://ollama.com/download/ollama-linux-amd64.tgz answers 404.
const (
	ollamaLatestAPI    = "https://api.github.com/repos/ollama/ollama/releases/latest"
	ollamaChecksumFile = "sha256sum.txt"

	// OllamaUnitName is the user unit that keeps the native runtime up.
	// The uninstaller stops and removes it by this name.
	OllamaUnitName = "memql-ollama.service"

	// nativeListen is the only address the unit ever binds.
	nativeListen = "127.0.0.1:11434"
)

// ollamaReleases is the release base every archive URL is built on. A
// variable rather than a constant so the tests can point it at a fake
// release served over loopback; nothing outside the tests assigns it.
var ollamaReleases = "https://github.com/ollama/ollama/releases"

// NativeArchiveName is the base asset for a GOARCH, or "" for one the
// vendor does not ship.
func NativeArchiveName(goarch string) string {
	switch goarch {
	case "amd64", "arm64":
		return "ollama-linux-" + goarch + ".tar.zst"
	}
	return ""
}

// NativeROCmArchiveName is the ROCm add-on for a GOARCH, or "" where
// there is none -- the vendor ships it for amd64 only.
func NativeROCmArchiveName(goarch string) string {
	if goarch == "amd64" {
		return "ollama-linux-amd64-rocm.tar.zst"
	}
	return ""
}

// URL is where the archive is fetched from.
func (a Archive) URL() string { return releaseAssetURL(a.Release, a.Name) }

// ChecksumURL is the release's sha256sum.txt.
func (a Archive) ChecksumURL() string { return releaseAssetURL(a.Release, ollamaChecksumFile) }

func releaseAssetURL(release, name string) string {
	if release == "" || release == "latest" {
		return ollamaReleases + "/latest/download/" + name
	}
	return ollamaReleases + "/download/" + release + "/" + name
}

// Lines are the sentences printed above the commands before the question
// is asked. They say what will be fetched, from where, into where, and
// what will be written -- the same contract the command lines keep:
// nothing runs that was not on the screen.
func (s Stage) Lines() []string {
	var out []string
	if s.Reuse {
		out = append(out, "reuse the Ollama already unpacked at "+s.RuntimeDir)
	}
	for _, a := range s.Archives {
		out = append(out, "download "+a.Name+" from "+strings.TrimSuffix(a.URL(), "/"+a.Name)+
			" and unpack it into "+s.RuntimeDir+", checked against the release's "+ollamaChecksumFile)
	}
	detail := "Ollama as your user, listening on " + s.Listen + ", models in " + s.ModelsDir
	if s.Parallel > 1 {
		detail += ", " + strconv.Itoa(s.Parallel) + " requests at a time per model"
	}
	out = append(out, "write "+s.UnitPath+" ("+detail+")")
	return out
}

// Unit is the unit file, in full. Pure, so the test can pin every line
// that matters: the loopback bind, the models directory inside the
// fence, and the log path a refusal names.
func (s Stage) Unit() string {
	return "[Unit]\n" +
		"Description=Ollama for MemQL local models\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		// systemd restricts characters in the executable itself even when
		// quoted. A fixed launcher keeps the operator path in an ordinary
		// argv slot; it is absolute, so env does no PATH lookup and no shell.
		"ExecStart=/usr/bin/env " + unitWord(strings.ReplaceAll(filepath.Join(s.RuntimeDir, "bin", "ollama"), "$", "$$")) + " serve\n" +
		"Environment=OLLAMA_HOST=" + s.Listen + "\n" +
		"Environment=" + unitWord("OLLAMA_MODELS="+s.ModelsDir) + "\n" +
		// The cache at eight bits: half the memory of f16 for a loss the
		// runtime's own docs call very small, and on a 24 GB card the
		// difference between the 27B and the embedder staying resident
		// together and a reload on every embed-then-chat pair. Flash
		// attention, which it needs, is the runtime's default where the
		// device supports it (2026-09-08 record, D9).
		"Environment=OLLAMA_KV_CACHE_TYPE=q8_0\n" +
		s.parallelEnvironment() +
		"Restart=on-failure\n" +
		"RestartSec=3\n" +
		"StandardOutput=append:" + strings.ReplaceAll(s.LogPath, "%", "%%") + "\n" +
		"StandardError=append:" + strings.ReplaceAll(s.LogPath, "%", "%%") + "\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

// unitWord protects paths from systemd's word splitting, C escapes and
// specifier expansion. This is systemd syntax, not shell quoting.
func unitWord(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	if !strings.ContainsAny(value, " \t\r\n\\\"'") {
		return value
	}
	return strconv.Quote(value)
}

// Stager runs a Stage. A seam for the same reason Runner is: the tests
// assert what would be fetched and written, on a runner with no network.
type Stager func(ctx context.Context, s Stage, onProgress func(Progress)) error

// The named errors a stage can end in. Each is a different sentence for
// the person, so each is a different value.
var (
	// ErrChecksumMismatch is an archive whose bytes are not the release's.
	ErrChecksumMismatch = errors.New("the downloaded archive does not match the release's checksum")
	// ErrArchiveEscapes is an archive entry that would land outside the
	// runtime directory. Refused before anything is written.
	ErrArchiveEscapes = errors.New("the archive names a path outside the runtime directory")
	// ErrNotEnoughDisk is a volume that cannot hold the archive and its
	// unpacked contents, refused before the first byte is fetched.
	ErrNotEnoughDisk = errors.New("not enough free disk for the runtime")
)

// stageDiskFactor is the free space required, as a multiple of the
// archive's size: the archive itself, its unpacked contents at the
// vendor's 1.6-to-1 ratio, and margin.
const stageDiskFactor = 3

// stageFreeBytes reads free space on a volume. A variable so the test can
// model a full disk without one; every platform file provides
// availableBytes, with zero meaning "not established", which refuses
// nothing.
var stageFreeBytes = func(path string) uint64 { return availableBytes(path) }

func humanGB(n uint64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

// StageRuntime is the Stager that actually fetches, checks, unpacks and
// writes. onProgress carries the download as a Progress whose Model is
// the archive name, so the command can draw it with the same display a
// pull gets.
func StageRuntime(ctx context.Context, s Stage, onProgress func(Progress)) error {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	if !s.Reuse {
		if err := fetchAndUnpack(ctx, s, onProgress); err != nil {
			return err
		}
	}
	for _, dir := range []string{s.ModelsDir, filepath.Dir(s.LogPath), filepath.Dir(s.UnitPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// 0644: a unit is not a secret, and systemd reads it as the user.
	if err := os.WriteFile(s.UnitPath, []byte(s.Unit()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", s.UnitPath, err)
	}
	return nil
}

// fetchAndUnpack downloads every archive to a temporary file beside the
// runtime directory, verifies each, and only then unpacks into a fresh
// directory that replaces RuntimeDir at the end -- so an interrupted run
// leaves either the old runtime or the new one, never half of each.
func fetchAndUnpack(ctx context.Context, s Stage, onProgress func(Progress)) error {
	parent := filepath.Dir(s.RuntimeDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", parent, err)
	}
	fresh := s.RuntimeDir + ".new"
	if err := os.RemoveAll(fresh); err != nil {
		return err
	}
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		return err
	}
	// Removed on every exit, success included: a verified archive is
	// worth nothing once unpacked and costs a gigabyte and a half. The
	// staging directory goes too unless it was renamed into place -- a
	// failed unpack must not leave gigabytes beside the runtime.
	var downloaded []string
	renamed := false
	defer func() {
		for _, p := range downloaded {
			_ = os.Remove(p)
		}
		if !renamed {
			_ = os.RemoveAll(fresh)
		}
	}()

	release := ""
	for _, a := range s.Archives {
		if a.Release == "" || a.Release == "latest" {
			// Resolved ONCE for every archive in the stage, so the base
			// archive and the ROCm add-on cannot come from two releases.
			if release == "" {
				release = resolveLatestRelease(ctx)
			}
			a.Release = release
		}
		path, err := downloadVerified(ctx, a, parent, onProgress)
		if err != nil {
			return err
		}
		downloaded = append(downloaded, path)
		if err := unpackTarZst(path, fresh); err != nil {
			return fmt.Errorf("unpacking %s: %w", a.Name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fresh, "bin", "ollama")); err != nil {
		return fmt.Errorf("the archive unpacked without a bin/ollama; the release layout has changed and this cockpit needs updating")
	}
	if err := os.RemoveAll(s.RuntimeDir); err != nil {
		return err
	}
	if err := os.Rename(fresh, s.RuntimeDir); err != nil {
		return err
	}
	renamed = true
	return nil
}

// resolveLatestRelease asks GitHub which tag is latest, so the archive
// and the checksum file are fetched from ONE release. On any failure it
// answers "latest", and the two `latest/download` URLs are used -- which
// is only wrong across the minutes a new release is being published, and
// then the checksum refuses and the person runs the command again.
func resolveLatestRelease(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaLatestAPI, nil)
	if err != nil {
		return "latest"
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "latest"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "latest"
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "latest"
	}
	if tag := strings.TrimSpace(body.Tag); tag != "" && !strings.ContainsAny(tag, "/ \t\n") {
		return tag
	}
	return "latest"
}

// downloadVerified streams the archive to a temporary file in dir while
// hashing it, then compares against the release's sha256sum.txt. The
// checksum file is read FIRST: a download that cannot be verified is not
// worth the bandwidth.
func downloadVerified(ctx context.Context, a Archive, dir string, onProgress func(Progress)) (string, error) {
	want, err := expectedChecksum(ctx, a)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s answered %s", a.Name, a.URL(), resp.Status)
	}
	total := uint64(0)
	if resp.ContentLength > 0 {
		total = uint64(resp.ContentLength)
	}
	// The archive and what it unpacks to share one volume, and the
	// vendor's compresses at about 1.6 to 1 (v0.33.3: 1.43 GB of
	// archive, 2.26 GB unpacked). Three times the archive is the room
	// both need with a margin; a download that could not be unpacked is
	// a gigabyte and a half spent to arrive at the same refusal later.
	if free := stageFreeBytes(dir); total > 0 && free > 0 && free < stageDiskFactor*total {
		return "", fmt.Errorf("%w: %s needs about %s free on the volume holding %s and it has %s",
			ErrNotEnoughDisk, a.Name, humanGB(stageDiskFactor*total), dir, humanGB(free))
	}

	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	hash := sha256.New()
	done := uint64(0)
	status := "downloading " + a.Name
	onProgress(Progress{Model: a.Name, Status: status, Completed: 0, Total: total})
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				os.Remove(path)
				return "", werr
			}
			hash.Write(buf[:n])
			done += uint64(n)
			onProgress(Progress{Model: a.Name, Status: status, Completed: done, Total: total})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			os.Remove(path)
			return "", fmt.Errorf("downloading %s: %w", a.Name, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		os.Remove(path)
		return "", fmt.Errorf("%w: %s is %s, the release says %s", ErrChecksumMismatch, a.Name, got, want)
	}
	return path, nil
}

// expectedChecksum reads the archive's line out of the release's
// sha256sum.txt. Both spellings the vendor has used -- `./name` and a
// bare `name` -- are accepted; an archive the file does not list at all
// is refused, because "unverified" and "verified" must not look the same.
func expectedChecksum(ctx context.Context, a Archive) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.ChecksumURL(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", ollamaChecksumFile, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: %s answered %s", ollamaChecksumFile, a.ChecksumURL(), resp.Status)
	}
	return parseChecksum(io.LimitReader(resp.Body, 1<<20), a.Name)
}

func parseChecksum(r io.Reader, name string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		file := strings.TrimPrefix(fields[len(fields)-1], "./")
		if file == name && len(fields[0]) == sha256.Size*2 {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s does not list %s, so the download cannot be verified", ollamaChecksumFile, name)
}

// unpackTarZst unpacks a zstd-compressed tar into dest, refusing any
// entry that would land outside it. Symlinks are kept only when their
// target stays inside dest too -- the release uses them for shared
// library version names, and nothing else.
func unpackTarZst(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(zr)
	var links []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// Check the completed chain, including links never followed
			// while unpacking. Otherwise a later library load could follow
			// an escape that the rooted writes never had to resolve.
			for _, name := range links {
				if _, err := root.Stat(name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w: resolving %s: %v", ErrArchiveEscapes, name, err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		target, err := insideDir(dest, hdr.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		name, err := filepath.Rel(dest, target)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Relative, and resolving inside dest from where the link
			// sits. Join cleans the `..` segments, so a link that climbs
			// out is one whose cleaned path leaves the prefix.
			resolved := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if filepath.IsAbs(hdr.Linkname) || !strings.HasPrefix(resolved, dest+string(filepath.Separator)) {
				return fmt.Errorf("%w: %s -> %s", ErrArchiveEscapes, hdr.Name, hdr.Linkname)
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			_ = root.Remove(name)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			links = append(links, name)
		case tar.TypeLink:
			source, err := insideDir(dest, hdr.Linkname)
			if err != nil || source == "" {
				return fmt.Errorf("%w: %s -> %s", ErrArchiveEscapes, hdr.Name, hdr.Linkname)
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			_ = root.Remove(name)
			sourceName, err := filepath.Rel(dest, source)
			if err != nil {
				return err
			}
			if err := root.Link(sourceName, name); err != nil {
				return err
			}
			// A hardlink to a symlink relocates its relative target. Check
			// that new location too before the runtime can follow it.
			links = append(links, name)
		default:
			// Character devices, fifos and the rest have no business in
			// a runtime archive; skipped rather than refused, since a
			// pax header record is one of them.
		}
	}
}

// insideDir resolves an archive entry name under dest, or refuses. An
// empty result with no error is an entry that names dest itself.
func insideDir(dest, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == "." || clean == "" {
		return "", nil
	}
	// An absolute name and a name that starts by climbing are refused on
	// their SPELLING, before any join: Join would fold `../evil` under
	// dest and make it look harmless, and an archive that names a path
	// like that is not one to be generous with.
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrArchiveEscapes, name)
	}
	target := filepath.Join(dest, clean)
	if !strings.HasPrefix(target, dest+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrArchiveEscapes, name)
	}
	return target, nil
}

// WaitReady polls the runtime's version route until it answers, or the
// deadline passes. An install command that exited zero says the SERVICE
// was asked to start, not that anything is listening -- and the very next
// step pulls against that socket.
func WaitReady(ctx context.Context, base string, within time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	url := strings.TrimRight(base, "/") + "/api/version"
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			var version struct {
				Version string `json:"version"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&version)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && strings.TrimSpace(version.Version) != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("nothing answered at %s within %s: %w", base, within, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

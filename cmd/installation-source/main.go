// installation-source captures a pinned checkout's verified installation
// inputs for transport as a private Workbench artifact. It publishes nothing
// and cannot approve, prepare or apply an installation update.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/znasllc-io/memql/integrations/argocd"
)

type captureResult struct {
	Digest string `json:"sourceDigest"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var repository, specFile, output string
	flags := flag.NewFlagSet("installation-source", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&repository, "repository", "", "path to the Job's pinned Git checkout")
	flags.StringVar(&specFile, "spec", "", "native render specification JSON file")
	flags.StringVar(&output, "output", "", "private source archive file; parent directory must exist")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Capture verified installation source objects from a pinned checkout.\nUsage: installation-source --repository=PATH --spec=PATH --output=PATH")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil || flags.NArg() != 0 || repository == "" || specFile == "" || output == "" {
		return emitResult(stdout, 2, false, captureResult{}, "repository, spec and output flags are required; positional arguments are not accepted")
	}
	specBody, err := readBoundedFile(specFile, 512<<10)
	if err != nil {
		return emitResult(stdout, 2, false, captureResult{}, "render specification could not be read within its bound")
	}
	spec, err := argocd.DecodeRenderSpec(specBody)
	if err != nil {
		return emitResult(stdout, 2, false, captureResult{}, "render specification is invalid")
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return emitResult(stdout, 2, false, captureResult{}, "checkout path is invalid")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return emitResult(stdout, 4, false, captureResult{}, "Git is required in the collector image")
	}
	result, changed, err := capture(ctx, checkoutObjects{repository: repository, git: git}, spec, output)
	if err != nil {
		return emitResult(stdout, 5, changed, result, err.Error())
	}
	return emitResult(stdout, 0, changed, result, "")
}

func emitResult(output io.Writer, code int, changed bool, result captureResult, message string) int {
	var failure any
	if code != 0 {
		failure = struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{code, message}
	}
	envelope := struct {
		OK         bool          `json:"ok"`
		Capability string        `json:"capability"`
		Changed    bool          `json:"changed"`
		Result     captureResult `json:"result"`
		Error      any           `json:"error"`
	}{code == 0, "installation.source.capture", changed, result, failure}
	if json.NewEncoder(output).Encode(envelope) != nil {
		return 5
	}
	return code
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	// Kubernetes projected configuration uses symlinks. The resolved input
	// must be a regular file; FIFOs/devices must not block a bounded read.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("input must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBounded(f, limit)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("file exceeds its read bound")
	}
	return body, nil
}

func capture(ctx context.Context, objects argocd.GitObjects, spec argocd.RenderSpec, output string) (result captureResult, changed bool, err error) {
	full, err := filepath.Abs(output)
	if err != nil {
		return captureResult{}, false, errors.New("source archive output path is invalid")
	}
	root, err := os.OpenRoot(filepath.Dir(full))
	if err != nil {
		return captureResult{}, false, errors.New("source archive output directory is unavailable")
	}
	defer root.Close()
	name := filepath.Base(full)
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return captureResult{}, false, errors.New("existing source archive is not a regular file")
		}
		result, err := verifyExisting(ctx, root, name, spec)
		return result, false, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return captureResult{}, false, errors.New("source archive output could not be checked")
	}
	temporary := ".memql-source-" + rand.Text() + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return captureResult{}, false, errors.New("source archive temporary file could not be created")
	}
	defer func() {
		if removeErr := root.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.New("source archive temporary file cleanup failed")
		}
	}()
	defer file.Close()
	closed, err := argocd.CaptureSourceArchive(ctx, objects, spec, file)
	if err != nil {
		return captureResult{}, false, err
	}
	info, err := file.Stat()
	if err != nil || file.Sync() != nil || file.Close() != nil {
		return captureResult{}, false, errors.New("source archive could not be finalized")
	}
	if err := ctx.Err(); err != nil {
		return captureResult{}, false, err
	}
	// A same-directory hard link publishes the completed file atomically and
	// refuses to replace an existing path, even if another collector wins.
	if err := root.Link(temporary, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			result, err := verifyExisting(ctx, root, name, spec)
			return result, false, err
		}
		return captureResult{}, false, errors.New("source archive could not be installed without replacing a file")
	}
	return captureResult{closed.Digest(), len(closed.Files()), info.Size()}, true, nil
}

func verifyExisting(ctx context.Context, root *os.Root, name string, spec argocd.RenderSpec) (captureResult, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return captureResult{}, errors.New("existing source archive is not a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return captureResult{}, errors.New("existing source archive could not be opened")
	}
	defer file.Close()
	body, err := readBounded(file, argocd.MaxSourceArchiveBytes)
	if err != nil {
		return captureResult{}, errors.New("existing source archive exceeds its bound")
	}
	closed, err := argocd.VerifySourceArchive(ctx, body, spec)
	if err != nil {
		return captureResult{}, errors.New("existing output does not verify against the requested source; choose a different output path")
	}
	return captureResult{closed.Digest(), len(closed.Files()), int64(len(body))}, nil
}

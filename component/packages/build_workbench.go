package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// build_workbench.go BINDS Deps.Builder in production (epic memql#4900, task
// memql#4901). builder.go, beside it, records what the seam is and why it was
// empty; this is what fills it.
//
// ===========================================================================
// THE ISOLATION IS THE FEATURE, NOT THE BUILD
// ===========================================================================
// A package's build script is somebody else's code, and `npm ci` runs whatever
// its dependencies put in a postinstall hook. So the command never runs in this
// process: it runs on a workbench node, in a directory that exists for the
// length of one build, under an environment this cluster CONSTRUCTS rather than
// inherits. What crosses the boundary is a tarball in and a tarball out --
// which is also why this file talks to the workbench through its Go entry
// (workbench.Integration.RunBuild) rather than through a DSL builtin: there is
// no construct for a model to name.
//
// ===========================================================================
// WHAT THIS FILE OWNS, AND WHAT IT DOES NOT
// ===========================================================================
// It owns the TRANSLATION: the snapshot tree becomes a tar.gz, the manifest's
// command and output directory become a request, the answer becomes an
// edge.Bundle or a typed refusal. Every bound -- the file caps, the timeout,
// the log tail -- is passed IN from the pipeline's own Limits and the env
// registry, so the workbench never invents a limit the pipeline did not ask
// for and the two cannot drift.
//
// It does not own the affinity: the pipeline carries the node the last
// deployable of this run built on and hands it back, because the run is what
// knows there was a previous deployable.

// workbenchRunner is the slice of the workbench integration this package
// needs. An interface, so the pipeline suite drives the whole binding against
// a recorder with no workbench, no directory and no shell.
type workbenchRunner interface {
	RunBuild(ctx context.Context, req workbench.BuildRequest, pinnedNodeId string) workbench.BuildResult
}

// workbenchBuilder is the production Builder.
type workbenchBuilder struct {
	runner workbenchRunner
	logger *slog.Logger
	// timeout bounds one deployable's command. From the env registry, so an
	// operator whose build genuinely takes longer than the default has a knob
	// rather than a fork.
	timeout int
}

// NewWorkbenchBuilder wires the seam. Returns nil when there is no workbench
// to run on, which is what keeps `Deps.Builder == nil` -- and therefore the
// existing "this cluster has no build surface configured" refusal -- the
// answer on a node that cannot reach one, rather than a nil-pointer three
// stages later.
func NewWorkbenchBuilder(runner workbenchRunner, logger *slog.Logger) Builder {
	if runner == nil {
		return nil
	}
	return &workbenchBuilder{runner: runner, logger: logger, timeout: BuildTimeoutSeconds()}
}

// Build runs one deployable's build on the workbench and returns its bundle.
func (b *workbenchBuilder) Build(ctx context.Context, run BuildRun, snapshot *SourceSnapshot, dep DeployableReport) (BuildResult, error) {
	limits := run.Limits.normalized()
	source, err := tarGzFromTree(snapshot.Tree, limits)
	if err != nil {
		return BuildResult{}, refuseScoped(CodeSourceUnreadable, dep.Name,
			"the source snapshot for %q could not be packed for the build surface: %v", dep.Name, err)
	}

	res := b.runner.RunBuild(ctx, workbench.BuildRequest{
		DeploymentId:   run.DeploymentId,
		DeployableName: dep.Name,
		OwnerUserId:    run.OwnerUserId,
		Path:           dep.Path,
		Command:        dep.Command,
		Output:         dep.Output,
		Source:         workbench.BuildBytes{Inline: source},
		TimeoutSec:     b.timeout,
		Limits: workbench.BuildLimits{
			MaxSourceBytes: limits.MaxSourceBytes,
			MaxOutputBytes: limits.MaxSourceBytes,
			MaxFileBytes:   limits.MaxFileBytes,
			MaxFileCount:   limits.MaxFileCount,
		},
	}, run.PinnedNodeId)

	out := BuildResult{
		LogTail: res.LogTail,
		BuiltOn: BuiltOn{Surface: SurfaceWorkbench, NodeId: res.NodeId},
	}
	if !res.OK {
		return out, buildRefusalFor(dep, res)
	}
	bundle, berr := bundleFromTarGz(res.Output.Inline, limits)
	if berr != nil {
		return out, refuseScoped(CodeDeployableBuildFailed, dep.Name,
			"the build for %q finished and its output could not be read back: %v", dep.Name, berr)
	}
	out.Bundle = bundle
	return out, nil
}

// buildRefusalFor turns the workbench's typed answer into this package's own,
// so the OS keys on a code from the catalogue rather than on one from a
// different layer's vocabulary.
//
// The mapping is deliberately not one-to-one: three of the workbench's codes
// mean "your build failed" and say so in the sentence, and one --
// no_workbench_peer -- is an operator fact about this cluster that a package
// author can do nothing about, so it keeps its own code all the way to the
// Build stop.
func buildRefusalFor(dep DeployableReport, res workbench.BuildResult) error {
	msg := strings.TrimSpace(res.ErrorMessage)
	if msg == "" {
		msg = fmt.Sprintf("the build for %q did not finish", dep.Name)
	}
	if tail := strings.TrimSpace(res.LogTail); tail != "" {
		msg += "\n\n" + tail
	}
	switch res.ErrorCode {
	case workbench.BuildCodeNoPeer:
		return refuseScoped(CodeNoWorkbenchPeer, dep.Name, "%s", msg)
	case workbench.BuildCodeTimeout:
		return refuseScoped(CodeDeployableBuildTimeout, dep.Name, "%s", msg)
	case workbench.BuildCodeSourceTooLarge:
		// The source could not travel to the build surface. Same code the
		// fetch uses for a tree over the caps, scoped to the app, because the
		// repair is the same: a smaller tree, or a cluster that can hold it.
		return refuseScoped(CodeSourceTooLarge, dep.Name, "%s", msg)
	default:
		return refuseScoped(CodeDeployableBuildFailed, dep.Name, "%s", msg)
	}
}

// ---------------------------------------------------------------------------
// The two translations
// ---------------------------------------------------------------------------

// tarGzFromTree packs the whole snapshot tree, from whichever source form it
// arrived in, as the one shape the build surface reads.
//
// FROM THE TREE rather than from the bytes as fetched, and that is what makes
// a zip-sourced package and a repo-sourced one build identically: the fetch
// already normalised both into one fs.FS with the manifest at its root (D1),
// so packing from there is the last place the two forms could have differed
// and they do not.
//
// Sorted, and with no timestamps, so the same tree packs to the same bytes
// twice.
func tarGzFromTree(tree fs.FS, limits Limits) ([]byte, error) {
	var names []string
	err := fs.WalkDir(tree, ".", func(p string, entry fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	if len(names) > limits.MaxFileCount {
		return nil, fmt.Errorf("the source holds more than %d files", limits.MaxFileCount)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var total int64
	for _, name := range names {
		info, serr := fs.Stat(tree, name)
		if serr != nil {
			return nil, serr
		}
		if info.Size() > limits.MaxFileBytes {
			return nil, fmt.Errorf("%q is larger than %d bytes", name, limits.MaxFileBytes)
		}
		if total+info.Size() > limits.MaxSourceBytes {
			return nil, fmt.Errorf("the source expands to more than %d bytes", limits.MaxSourceBytes)
		}
		data, rerr := fs.ReadFile(tree, name)
		if rerr != nil {
			return nil, rerr
		}
		mode := int64(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if werr := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			// One synthesized top-level directory, which the extractor on the
			// far side strips exactly as it strips GitHub's -- so what the
			// build sees is the tree the author wrote, at the root.
			Name:   path.Join("src", name),
			Mode:   mode,
			Size:   int64(len(data)),
			Format: tar.FormatPAX,
		}); werr != nil {
			return nil, werr
		}
		if _, werr := tw.Write(data); werr != nil {
			return nil, werr
		}
		total += int64(len(data))
	}
	if cerr := tw.Close(); cerr != nil {
		return nil, cerr
	}
	if cerr := gz.Close(); cerr != nil {
		return nil, cerr
	}
	return buf.Bytes(), nil
}

// bundleFromTarGz reads the built output back into the flat path -> content
// map the publisher wants, under the same limits bundleFromTree enforces on
// the prebuilt path -- so a built bundle and a committed one are bounded
// identically and neither can be the way past the other's cap.
func bundleFromTarGz(raw []byte, limits Limits) (edge.Bundle, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("the build surface returned no output")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the built output is not a gzip archive: %w", err)
	}
	defer gz.Close()

	bundle := edge.Bundle{}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, nerr := tr.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			return nil, nerr
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean, perr := safeArchivePath(hdr.Name)
		if perr != nil {
			return nil, perr
		}
		if clean == "" {
			continue
		}
		// The far side packs under one top-level directory, for the same
		// reason the source carries one. Strip it here rather than asking the
		// far side not to write it: an archive with no root is indistinguishable
		// from one whose root happened to be the first file's directory.
		if _, rest, ok := strings.Cut(clean, "/"); ok {
			clean = rest
		} else {
			continue
		}
		if len(bundle) >= limits.MaxFileCount {
			return nil, refuse(CodeSourceTooLarge, "the built output holds more than %d files", limits.MaxFileCount)
		}
		if hdr.Size > limits.MaxFileBytes {
			return nil, refuse(CodeSourceTooLarge, "%q is larger than %d bytes", clean, limits.MaxFileBytes)
		}
		total += hdr.Size
		if total > limits.MaxSourceBytes {
			return nil, refuse(CodeSourceTooLarge, "the built output exceeds %d bytes", limits.MaxSourceBytes)
		}
		data := make([]byte, hdr.Size)
		// io.ReadFull rather than a loop: a tar entry shorter than its header
		// claims is a TRUNCATED archive, and ErrUnexpectedEOF says exactly
		// that. A hand-rolled read that treated the short answer as the end
		// would publish a half-written file as though it were the whole one.
		if _, rerr := io.ReadFull(tr, data); rerr != nil {
			return nil, rerr
		}
		bundle[clean] = data
	}
	if len(bundle) == 0 {
		return nil, fmt.Errorf("the built output holds no files")
	}
	return bundle, nil
}

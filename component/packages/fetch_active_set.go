package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/integrations/azureblob"
)

// FetchResult is what one fetcher-mode run did.
type FetchResult struct {
	// PointerAbsent distinguishes "no packages are deployed" from "the
	// pointer said nothing". The first is the ordinary state of a cluster
	// that has never deployed a package; the second would be a fault.
	PointerAbsent bool
	Domains       []string
	Files         int
}

// ErrNoBlobContainer is what an unconfigured fetcher answers, and it is an
// ERROR rather than a third state of FetchResult (memql#4933).
//
// It used to answer PointerAbsent -- "this cluster cannot be hosting packages,
// so there is nothing to fetch and nothing wrong" -- which is a reasonable
// reading of an engine process and the wrong one for an init container. The
// component that runs this ships `envFrom: memql-secrets` and
// MEMQL_AZURE_BLOB_CONTAINER is not in that Secret anywhere in this repo, so
// the fetcher saw an empty container name on every instance that applied the
// component as shipped, exited 0, and the node booted healthy with no package
// DSL. That is exactly the outcome the exit-code contract's `1` exists to
// prevent, reached through its `0`.
//
// "Not configured" and "configured, and nothing is deployed" are different
// answers and only one of them is ordinary.
var ErrNoBlobContainer = errors.New(
	"no object storage container is configured (MEMQL_AZURE_BLOB_CONTAINER is unset), " +
		"so the package pointer cannot be read. A node that boots without it serves none of " +
		"the DSL its packages deployed, and answers \"function not found\" to every call that " +
		"used to work. Set it on this container -- the dsl-packages component reads it from " +
		"the memql-storage ConfigMap")

// FetchActiveSet copies every package DSL tree named by the active-set pointer
// into root, which is the node's MEMQL_DSL_PATH.
//
// Called from the `dsl-fetch` subcommand, which runs as an init container
// before the node boots (D5). Kept here rather than in main so the copy rules
// live beside the staging rules that produced the layout -- the two halves of
// one content-addressed convention.
func FetchActiveSet(ctx context.Context, root string) (FetchResult, error) {
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return FetchResult{}, ErrNoBlobContainer
	}
	client, err := azureblob.New(ctx)
	if err != nil {
		return FetchResult{}, fmt.Errorf("opening object storage: %w", err)
	}

	raw, err := client.Download(ctx, container, ActiveSetPath)
	if err != nil || len(raw) == 0 {
		return FetchResult{PointerAbsent: true}, nil
	}

	set := map[string]string{}
	if err := json.Unmarshal(raw, &set); err != nil {
		// A pointer that EXISTS and cannot be read is a hard failure. Booting
		// anyway would bring the node up with silently-missing product DSL.
		return FetchResult{}, fmt.Errorf("the package pointer at %s is not readable: %w", ActiveSetPath, err)
	}
	if len(set) == 0 {
		return FetchResult{PointerAbsent: true}, nil
	}

	domains := make([]string, 0, len(set))
	for d := range set {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	total := 0
	for _, domain := range domains {
		prefix := set[domain]
		if !safeDomainName(domain) || !safePrefix(prefix) {
			return FetchResult{}, fmt.Errorf(
				"the package pointer names a domain or prefix this fetcher will not read (%q -> %q)", domain, prefix)
		}
		n, cerr := copyPrefix(ctx, client, container, prefix, filepath.Join(root, domain))
		if cerr != nil {
			return FetchResult{}, fmt.Errorf("copying domain %q: %w", domain, cerr)
		}
		total += n
	}
	return FetchResult{Domains: domains, Files: total}, nil
}

// safeDomainName bounds what a pointer may name as a directory under root.
//
// The pointer is written by this cluster's own pipeline, so this is a
// belt-and-braces check rather than the primary defence -- but the primary
// defence is a graph write and this is a filesystem write into the node's boot
// path, and those deserve different levels of trust.
func safeDomainName(d string) bool {
	if d == "" || d == "." || d == ".." || len(d) > 64 {
		return false
	}
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func safePrefix(p string) bool {
	return strings.HasPrefix(p, "packages/") && !strings.Contains(p, "..") && strings.HasSuffix(p, "/")
}

// copyPrefix writes every blob under prefix into dest, preserving the relative
// layout the stager wrote.
func copyPrefix(ctx context.Context, client *azureblob.AzureBlobUploader, container, prefix, dest string) (int, error) {
	names, err := client.ListPrefix(ctx, container, prefix)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	count := 0
	for _, name := range names {
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		data, derr := client.Download(ctx, container, name)
		if derr != nil {
			return count, derr
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return count, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

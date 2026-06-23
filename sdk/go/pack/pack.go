// Package pack is the Go SDK surface for the memQL pack browser -- the
// read-only enumeration of a node's embedded + plugin-registered .memql
// tree. It powers the cockpit's "browse packs" view: list the DSL domains
// a node carries, list the .memql files inside a domain, and fetch a
// single file's source.
//
// The package mirrors sdk/go/sense in shape -- a Client that wraps a
// client.Dispatcher, typed methods that take and return SDK-owned values
// (not raw protobuf), and errors that bubble up instead of being silently
// swallowed.
//
// There is no write path: a pack subtree is owned by its registering Go
// module and the embedded tree is immutable. This surface is read-only by
// construction.
//
// Types are SDK-owned. No memqlv1.* import leaks out of this package's
// exported surface -- callers depend on pack.Domain / pack.File / etc.,
// not on protobuf.
//
// See sdk/go/CLAUDE.md for the broader SDK rules (named-primitive
// contract, opaque types, generated-code-is-read-only).
package pack

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the three read-only pack-browsing operations over a
// Dispatcher. Safe to reuse across goroutines once constructed; the
// underlying Dispatcher is the multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires a pack client to the supplied dispatcher. The
// dispatcher must already be Run(); Client.* methods just dispatch
// SendAndWait calls through it.
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Domain is one browsable DSL namespace on a node. Origin is "embedded"
// for a core baked-in domain or "pack:<name>" for a domain contributed at
// init() time by an external Go module (a "pack"). FileCount is the
// recursive count of browsable files (.memql + .tmpl) under the domain.
type Domain struct {
	Name      string
	Origin    string
	FileCount int
}

// File is one browsable file inside a domain. Path is relative to the
// domain root (e.g. "queries.memql", "prompts/agentReply.tmpl"); Size is
// the byte length.
type File struct {
	Path string
	Size int64
}

// FileSource is the result of ReadFile -- a file's source plus its origin
// label. Origin is "embedded" | "pack:<domain>" | "disk:<path>" (the last
// when a MEMQL_DSL_PATH override is active for the construct type). Found
// is false when the requested file does not exist; Source is then empty.
type FileSource struct {
	Domain string
	Path   string
	Source string
	Origin string
	Found  bool
}

// ListDomains returns every browsable DSL domain the node carries --
// embedded core domains plus plugin-registered pack domains -- sorted by
// name.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ListPackDomains{
			ListPackDomains: &memqlv1.ListPackDomainsMsg{},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("pack.listDomains: %w", err)
	}
	result := resp.GetListPackDomainsResult()
	if result == nil {
		return nil, nil
	}
	out := make([]Domain, 0, len(result.GetDomains()))
	for _, d := range result.GetDomains() {
		out = append(out, Domain{
			Name:      d.GetName(),
			Origin:    d.GetOrigin(),
			FileCount: int(d.GetFileCount()),
		})
	}
	return out, nil
}

// ListFiles returns the browsable files under a single domain, sorted by
// relative path. An unknown domain yields an empty slice (not an error);
// callers distinguish "no such domain" via ListDomains.
func (c *Client) ListFiles(ctx context.Context, domain string) ([]File, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ListPackFiles{
			ListPackFiles: &memqlv1.ListPackFilesMsg{Domain: domain},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("pack.listFiles: %w", err)
	}
	result := resp.GetListPackFilesResult()
	if result == nil {
		return nil, nil
	}
	out := make([]File, 0, len(result.GetFiles()))
	for _, f := range result.GetFiles() {
		out = append(out, File{
			Path: f.GetPath(),
			Size: f.GetSize(),
		})
	}
	return out, nil
}

// ReadFile fetches a single file's source. relPath is relative to the
// domain root (as returned by ListFiles). The Go error return is reserved
// for wire-level failures; a missing file is reported as a FileSource with
// Found=false (not an error), mirroring the engine's read-only semantics.
func (c *Client) ReadFile(ctx context.Context, domain, relPath string) (*FileSource, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ReadPackFile{
			ReadPackFile: &memqlv1.ReadPackFileMsg{
				Domain: domain,
				Path:   relPath,
			},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("pack.readFile: %w", err)
	}
	result := resp.GetReadPackFileResult()
	if result == nil {
		return nil, nil
	}
	return &FileSource{
		Domain: result.GetDomain(),
		Path:   result.GetPath(),
		Source: result.GetSource(),
		Origin: result.GetOrigin(),
		Found:  result.GetFound(),
	}, nil
}

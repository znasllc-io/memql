package memql

import (
	"path/filepath"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// Pack browser handlers (memql#2127 / B1).
//
// Read-only enumeration of the embedded + plugin-registered .memql tree
// so the cockpit can browse the packs a node carries: list domains, list
// a domain's files, fetch a single file's source. The enumerator lives in
// the dsl package (which owns the unified Tree + plugin registry); these
// handlers are the thin gRPC adapter, mirroring sense_handlers.go.
//
// No write path -- the embedded tree is immutable and a pack subtree is
// owned by its registering module.

// handleListPackDomains handles ListPackDomainsMsg requests.
func (s *streamSession) handleListPackDomains(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ListPackDomainsMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "list_pack_domains: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	domains := dsl.ListPackDomains()
	protoDomains := make([]*memqlv1.PackDomain, len(domains))
	for i, d := range domains {
		protoDomains[i] = &memqlv1.PackDomain{
			Name:      d.Name,
			Origin:    d.Origin,
			FileCount: int32(d.FileCount),
		}
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ListPackDomainsResult{
			ListPackDomainsResult: &memqlv1.ListPackDomainsResult{
				RequestId: requestId,
				Domains:   protoDomains,
			},
		},
	})
}

// handleListPackFiles handles ListPackFilesMsg requests.
func (s *streamSession) handleListPackFiles(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ListPackFilesMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "list_pack_files: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	domain := msg.GetDomain()

	files := dsl.ListPackFiles(domain)
	protoFiles := make([]*memqlv1.PackFile, len(files))
	for i, f := range files {
		protoFiles[i] = &memqlv1.PackFile{
			Path: f.Path,
			Size: f.Size,
		}
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ListPackFilesResult{
			ListPackFilesResult: &memqlv1.ListPackFilesResult{
				RequestId: requestId,
				Domain:    domain,
				Files:     protoFiles,
			},
		},
	})
}

// handleReadPackFile handles ReadPackFileMsg requests.
func (s *streamSession) handleReadPackFile(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ReadPackFileMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "read_pack_file: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	domain := msg.GetDomain()
	relPath := msg.GetPath()

	source, origin, err := dsl.ReadPackFile(domain, relPath)
	if err != nil {
		// A missing file is a normal, non-fatal answer (found=false), not
		// a wire error -- mirrors Sense's "nothing to say" pattern.
		return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ReadPackFileResult{
				ReadPackFileResult: &memqlv1.ReadPackFileResult{
					RequestId: requestId,
					Domain:    domain,
					Path:      relPath,
					Found:     false,
				},
			},
		})
	}

	// Layer the MEMQL_DSL_PATH disk-overlay label on top of the dsl
	// enumerator's embedded/pack label. The overlay is per-construct-type
	// (concepts / queries / prompts / ...), resolved here because the
	// dsl package deliberately stays free of the component/memql/dslfs
	// dependency. When the construct type derived from the file name has
	// an on-disk override active, report "disk:<root>".
	origin = packFileOrigin(relPath, origin)

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ReadPackFileResult{
			ReadPackFileResult: &memqlv1.ReadPackFileResult{
				RequestId: requestId,
				Domain:    domain,
				Path:      relPath,
				Source:    string(source),
				Origin:    origin,
				Found:     true,
			},
		},
	})
}

// packFileOrigin upgrades the dsl-layer origin ("embedded"/"pack:<d>") to
// the dslfs disk-overlay label when MEMQL_DSL_PATH provides an on-disk
// override for the construct type this file belongs to. The construct
// type is the file's base name without extension (e.g. "queries.memql" ->
// "queries"); files that don't map to a known construct type (e.g. a
// prompt template under prompts/) keep their embedded/pack label.
func packFileOrigin(relPath, base string) string {
	typeName := constructTypeFromPath(relPath)
	if typeName == "" {
		return base
	}
	origin := dslfs.OriginFor(typeName)
	if origin == "embedded" {
		// No disk override for this construct type -- keep the dsl-layer
		// label (which distinguishes embedded vs pack).
		return base
	}
	return origin
}

// constructTypeFromPath maps a domain-relative path to the dslfs construct
// type name. Only top-level "<type>.memql" files participate in the
// MEMQL_DSL_PATH overlay; nested template files (prompts/*.tmpl) do not.
func constructTypeFromPath(relPath string) string {
	if filepath.Dir(relPath) != "." {
		return ""
	}
	name := filepath.Base(relPath)
	ext := filepath.Ext(name)
	if ext != ".memql" {
		return ""
	}
	return name[:len(name)-len(ext)]
}

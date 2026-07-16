package main

import (
	"sync"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// documentStore holds the authoritative full text of every open document,
// keyed by URI. MemQL Sense operates on full source strings, so the server
// keeps the whole buffer per document and folds incremental didChange edits
// into it rather than diffing. Safe for concurrent use.
type documentStore struct {
	mu   sync.RWMutex
	docs map[protocol.DocumentUri]string
}

func newDocumentStore() *documentStore {
	return &documentStore{docs: make(map[protocol.DocumentUri]string)}
}

// open records (or replaces) a document's full text.
func (s *documentStore) open(uri protocol.DocumentUri, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[uri] = text
}

// closeDoc drops a document from the store.
func (s *documentStore) closeDoc(uri protocol.DocumentUri) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, uri)
}

// get returns a document's current full text.
func (s *documentStore) get(uri protocol.DocumentUri) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, ok := s.docs[uri]
	return text, ok
}

// uris returns the URIs of every open document.
func (s *documentStore) uris() []protocol.DocumentUri {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]protocol.DocumentUri, 0, len(s.docs))
	for uri := range s.docs {
		out = append(out, uri)
	}
	return out
}

// applyChanges folds a didChange notification's content changes into the stored
// buffer in order, returning the new full text. A whole-document change replaces
// the buffer; a ranged change splices its text over the range's byte span.
// position.ByteOffsetForLSP resolves each endpoint's UTF-16 column to a byte
// offset, clamping out-of-range positions to the line/document end (unlike
// glsp's Range.IndexesIn, whose 0 sentinel for an over-long final-line character
// would silently corrupt the buffer). Returns ok=false when the URI is not open.
func (s *documentStore) applyChanges(uri protocol.DocumentUri, changes []any) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.docs[uri]
	if !ok {
		return "", false
	}
	for _, change := range changes {
		switch c := change.(type) {
		case protocol.TextDocumentContentChangeEvent:
			start := position.ByteOffsetForLSP(text, c.Range.Start)
			end := position.ByteOffsetForLSP(text, c.Range.End)
			if start > end {
				start, end = end, start
			}
			text = text[:start] + c.Text + text[end:]
		case protocol.TextDocumentContentChangeEventWhole:
			text = c.Text
		}
	}
	s.docs[uri] = text
	return text, true
}

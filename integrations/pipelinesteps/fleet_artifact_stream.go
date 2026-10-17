//go:build agent

package pipelinesteps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"sync"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

const fleetArtifactChunkBytes = 256 << 10
const fleetArtifactArchiveBytes int64 = 256 << 20

// One call carries one gzip archive on the existing authenticated data stream.
// The terminal result binds its size and digest. Neither a partial stream nor
// a result from another call can become a Library artifact. Spooling is private
// and bounded; stdout/stderr retain their separate masking and log path.
type fleetArtifactStream struct {
	mu     sync.Mutex
	file   *os.File
	hash   hash.Hash
	bytes  int64
	limit  int64
	err    error
	closed bool
}

func newFleetArtifactStream(path string, limit int64) (*fleetArtifactStream, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &fleetArtifactStream{file: f, hash: sha256.New(), limit: min(limit, fleetArtifactArchiveBytes)}, nil
}

func (s *fleetArtifactStream) chunk(message *nodev1.WorkerForwardStream) {
	data, ok := message.GetPayload().(*nodev1.WorkerForwardStream_DataChunk)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.err != nil {
		return
	}
	if len(data.DataChunk) == 0 || len(data.DataChunk) > fleetArtifactChunkBytes {
		s.err = errors.New("the machine sent an invalid artifact chunk size")
		return
	}
	if int64(len(data.DataChunk)) > s.limit-s.bytes {
		s.err = ErrArtifactsTooLarge
		return
	}
	n, err := s.file.Write(data.DataChunk)
	if err != nil || n != len(data.DataChunk) {
		s.err = errors.New("the machine's artifact stream could not be retained")
		return
	}
	s.hash.Write(data.DataChunk)
	s.bytes += int64(n)
}

func (s *fleetArtifactStream) seal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *fleetArtifactStream) verified(size int64, digest string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		return nil, errors.New("the machine's artifact stream has not finished")
	}
	if s.err != nil {
		return nil, s.err
	}
	if size <= 0 || size != s.bytes || digest != "sha256:"+hex.EncodeToString(s.hash.Sum(nil)) {
		return nil, errors.New("the machine's artifact stream does not match its terminal size and digest")
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("the machine's artifact stream could not be read: %w", err)
	}
	return s.file, nil
}

func (s *fleetArtifactStream) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.file.Close()
}

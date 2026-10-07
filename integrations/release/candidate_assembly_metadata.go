package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// Discovery reads no mutable Library fields and publishes nothing. Its small
// metadata claim still has to pass whole-image verification after durable pins.
// Retirement racing discovery makes later preparation refuse, never ready.
func (s *candidateAssemblyScope) imageDigest(ctx context.Context, want pipelinesteps.StoredFileReceipt) (_ string, err error) {
	if want.Size <= 0 || want.Size > 256<<10 {
		return "", errors.New("OCI producer metadata exceeds its 256 KiB bound")
	}
	got, body, err := s.p.library.OpenRunFileReceipt(auth.ContextWithInternalOrigin(ctx), receiptScope(want), want.IntentID)
	if err != nil {
		return "", err
	}
	if body == nil {
		return "", errors.New("OCI producer metadata stream is unavailable")
	}
	defer func() { err = errors.Join(err, body.Close()) }()
	if got != want {
		return "", errors.New("OCI producer metadata changed during discovery")
	}
	data, err := io.ReadAll(io.LimitReader(&candidateContextReader{ctx: ctx, r: body}, want.Size+1))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
		return "", errors.New("OCI metadata bytes differ from their execution receipt")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := candidateUniqueJSON(decoder, 0); err != nil {
		return "", errors.New("OCI metadata contains ambiguous JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("OCI metadata contains trailing data")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", errors.New("OCI metadata must be an object")
	}
	var digest string
	if err := json.Unmarshal(metadata["containerimage.digest"], &digest); err != nil || !strings.HasPrefix(digest, "sha256:") || !candidateArtifactDigest(strings.TrimPrefix(digest, "sha256:")) {
		return "", errors.New("OCI metadata requires one exact container image digest")
	}
	return digest, nil
}

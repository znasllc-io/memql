package releasecatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotOwnsTrustAndUsesFreshAuthenticatedStreams(t *testing.T) {
	f, s, candidate, digest := newFixture(t)
	body, err := json.Marshal(configuration{FormatVersion: 1, Sources: []source{s}})
	require.NoError(t, err)
	first, err := NewSnapshot(readerContext(), body, "fixture-private-token")
	require.NoError(t, err)
	second, err := NewSnapshot(readerContext(), body, "fixture-private-token")
	require.NoError(t, err)
	for i := range body {
		body[i] = 'x'
	}
	for _, r := range []*Reader{first, second} {
		proof, err := r.Get(readerContext(), s.ID, candidate, digest)
		require.NoError(t, err)
		require.Equal(t, digest, proof.Digest())
		_, err = r.Get(context.Background(), s.ID, candidate, digest)
		require.Error(t, err)
		require.NotContains(t, fmt.Sprintf("%+v %#v", r, r), "fixture-private-token")
	}
	require.EqualValues(t, 2, f.queries.Load())
}
func TestSnapshotRejectsUnboundedOrMultipleSources(t *testing.T) {
	_, s, _, _ := newFixture(t)
	for _, sources := range [][]source{nil, {s, s}} {
		body, err := json.Marshal(configuration{FormatVersion: 1, Sources: sources})
		require.NoError(t, err)
		_, err = NewSnapshot(readerContext(), body, "fixture-private-token")
		require.Error(t, err)
	}
	body, err := json.Marshal(configuration{FormatVersion: 1, Sources: []source{s}})
	require.NoError(t, err)
	for _, token := range []string{"", "with space", "line\nbreak"} {
		_, err := NewSnapshot(readerContext(), body, token)
		require.Error(t, err)
	}
}

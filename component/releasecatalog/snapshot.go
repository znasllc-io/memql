package releasecatalog

import (
	"context"
	"errors"
	"strings"
)

// NewSnapshot is a native receiving-adapter port, not a public capability. The
// caller authenticates and versions the configuration and credential resource
// before construction, and reobserves those versions before acting on results.
// This reader owns one frozen source and credential, retaining the same SDK,
// role gate, TLS verification and signed-envelope verification as discovery.
func NewSnapshot(ctx context.Context, configuration []byte, credential string) (*Reader, error) {
	if len(configuration) > 256<<10 || credential == "" || len(credential) > 16<<10 || strings.ContainsAny(credential, "\r\n\t ") {
		return nil, errors.New("release snapshot requires bounded source configuration and credentials")
	}
	raw := string(configuration)
	// The scope has exactly one credential. Its name is private configuration,
	// and the exact match below prevents a later source from choosing another.
	var name string
	r := New(func(context.Context, string) (string, error) { return raw, nil }, func(_ context.Context, requested string) (string, error) {
		if requested != name {
			return "", errors.New("release credential is outside the frozen source")
		}
		return credential, nil
	})
	cfg, err := r.configuration(ctx)
	if err != nil {
		return nil, err
	}
	if len(cfg.Sources) != 1 {
		return nil, errors.New("release snapshot requires exactly one source")
	}
	name = cfg.Sources[0].CredentialSecret
	return r, nil
}

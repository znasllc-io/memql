package server

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/database/storagequota"
)

// Shared physical admission is authoritative in production. The row-footprint
// fallback is for provider-independent test adapters which do not own bytes.
func CheckStorageAdmission(ctx context.Context, uploader any, footprint func(context.Context) (int64, int64, int64, error), size, legacyLimit int64) error {
	if p, ok := uploader.(interface {
		StorageUsage(context.Context) (storagequota.Usage, error)
	}); ok {
		usage, err := p.StorageUsage(ctx)
		if err != nil {
			return err
		}
		if size < 0 || size+storagequota.ObjectOverhead > usage.Available {
			return storagequota.ErrLimit
		}
		return nil
	}
	files, versions, sessions, err := footprint(ctx)
	if err != nil {
		return err
	}
	if size < 0 || files+versions+sessions+size > legacyLimit {
		return fmt.Errorf("%w: %d bytes would exceed the quota of %d bytes (%s)", storagequota.ErrLimit, files+versions+sessions+size, legacyLimit, LibraryUserQuotaBytesEnv)
	}
	return nil
}

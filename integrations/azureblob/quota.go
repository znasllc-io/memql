package azureblob

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"golang.org/x/sync/errgroup"
)

var capacityStore atomic.Pointer[storagequota.Store]

// ConfigureCapacity is called by database bootstrap on every node. New clients
// resolve it lazily; a missing/unreachable journal refuses writes, never bypasses.
func ConfigureCapacity(db, gateDB func() *sql.DB) {
	capacityStore.Store(&storagequota.Store{DB: db, GateDB: gateDB})
}
func CapacityStore() *storagequota.Store { return capacityStore.Load() }

func quotaOwner(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Synthetic || actsAsEngine(ctx, ac) {
		return ""
	}
	return storagequota.OwnerID(ac.UserId)
}

// actsAsEngine reports a node acting for itself: the actor was bound from a
// verified SYSTEM-class forwarded assertion (a build forwarded to the
// workbench, a pipeline step), whose subject is a node's name and never a
// person. What it writes belongs to the cluster, exactly as a Synthetic
// actor's does; looking the name up as a user refuses the write ("cannot
// resolve storage owner"), which is how an oversized site build failed to
// store its output. The subject must be the actor's own, so a person's
// context never qualifies. Synthetic is not set for these actors on purpose:
// about forty call sites read it to skip checks.
func actsAsEngine(ctx context.Context, ac *auth.AccessContext) bool {
	fa, ok := auth.ForwardedAuthorityFromContext(ctx)
	return ok && fa.Kind == auth.ForwardedPrincipalSystem && fa.CredentialClass == auth.ForwardedClassSystem &&
		strings.TrimSpace(fa.Subject) == strings.TrimSpace(ac.UserId)
}

func (u *AzureBlobUploader) capacity(ctx context.Context) (*storagequota.Store, error) {
	s := capacityStore.Load()
	if s == nil {
		return nil, errors.New("storage accounting is not configured")
	}
	err := s.EnsureInitialized(ctx, u.client.URL(), u.inventory)
	return s, err
}

func (u *AzureBlobUploader) inventory(ctx context.Context, add func(string, string, int64) error) error {
	var emit sync.Mutex
	// Paginate the whole account. Historical versions and deleted/snapshot
	// payloads count too. Nothing is downloaded during inventory.
	containers := u.client.ServiceClient().NewListContainersPager(nil)
	for containers.More() {
		page, err := containers.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, c := range page.ContainerItems {
			if c.Name == nil {
				continue
			}
			blobs := u.client.NewListBlobsFlatPager(*c.Name, &azblob.ListBlobsFlatOptions{Include: azblob.ListBlobsInclude{Snapshots: true, Versions: true, Deleted: true, UncommittedBlobs: true}})
			for blobs.More() {
				p, err := blobs.NextPage(ctx)
				if err != nil {
					return err
				}
				group, scanCtx := errgroup.WithContext(ctx)
				group.SetLimit(16)
				for _, b := range p.Segment.BlobItems {
					if b.Name == nil {
						continue
					}
					group.Go(func() error {
						bc, err := u.blockClient(*c.Name, *b.Name)
						if err != nil {
							return err
						}
						key := bc.URL()
						var size int64
						if b.Properties != nil && b.Properties.ContentLength != nil {
							size = *b.Properties.ContentLength
						}
						if b.Snapshot != nil && *b.Snapshot != "" {
							key += "?snapshot=" + *b.Snapshot
						} else if b.VersionID != nil && *b.VersionID != "" && (b.IsCurrentVersion == nil || !*b.IsCurrentVersion) {
							key += "?versionid=" + *b.VersionID
						}
						if key == bc.URL() {
							staged, err := u.UncommittedBlocks(scanCtx, *c.Name, *b.Name)
							if err != nil {
								return err
							}
							for _, n := range staged {
								if n > storagequota.ClusterBytes-size {
									return storagequota.ErrLimit
								}
								size += n
							}
						}
						owner := ""
						parts := strings.SplitN(*b.Name, "/", 4)
						if len(parts) >= 3 && (parts[0] == "library" || parts[0] == "backups" || parts[0] == "objects") {
							owner = storagequota.OwnerID(parts[1])
						}
						emit.Lock()
						defer emit.Unlock()
						return add(key, owner, size)
					})
				}
				if err := group.Wait(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// beginCapacity is intentionally private. SDK-only protocol fixtures construct
// an unmetered client directly; every production constructor enables accounting.
func (u *AzureBlobUploader) beginCapacity(ctx context.Context, container, object string) (*storagequota.Guard, error) {
	if !u.meterWrites {
		return nil, nil
	}
	owner, err := storageOwner(ctx, object)
	if err != nil {
		return nil, err
	}
	if err := u.checkRetention(ctx, nil); err != nil {
		return nil, err
	}
	s, err := u.capacity(ctx)
	if err != nil {
		return nil, err
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return nil, err
	}
	g, err := s.Guard(ctx, u.client.URL(), bc.URL(), owner)
	if err != nil {
		return nil, err
	}
	props, err := bc.GetProperties(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		g.Close()
		return nil, err
	}
	if err == nil {
		if err = u.checkRetention(ctx, props.VersionID); err != nil {
			g.Close()
			return nil, err
		}
	}
	return g, nil
}

// Storage paths may contain a canonical user ID while the verified request
// carries its bare ID. Compare the same identity without changing object keys.
func storageOwner(ctx context.Context, object string) (string, error) {
	owner := quotaOwner(ctx)
	parts := strings.SplitN(object, "/", 4)
	if len(parts) >= 3 && (parts[0] == "library" || parts[0] == "backups" || parts[0] == "objects") {
		destination := storagequota.OwnerID(parts[1])
		if owner != "" && owner != destination {
			return "", errors.New("storage destination belongs to another user")
		}
		owner = destination
	}
	return owner, nil
}

func (u *AzureBlobUploader) ReserveStorage(ctx context.Context, container, object string, size int64) error {
	g, err := u.beginCapacity(ctx, container, object)
	if err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	defer g.Close()
	return g.Plan(ctx, size)
}
func (u *AzureBlobUploader) StorageUsage(ctx context.Context) (storagequota.Usage, error) {
	s, err := u.capacity(ctx)
	if err != nil {
		return storagequota.Usage{}, err
	}
	return s.Usage(ctx, quotaOwner(ctx))
}

func (u *AzureBlobUploader) commitCapacity(ctx context.Context, container, object string, ids []string) (*storagequota.Guard, error) {
	g, err := u.beginCapacity(ctx, container, object)
	if err != nil || g == nil {
		return g, err
	}
	if _, err = g.BlockListSize(ctx, ids); errors.Is(err, storagequota.ErrUnreservedBlock) {
		// Existing resumable sessions predate the ledger. Their bytes were
		// inventoried, but their provider block ids must also be adopted.
		bc, e := u.blockClient(container, object)
		if e == nil {
			inventory, readErr := bc.GetBlockList(ctx, blockblob.BlockListTypeAll, nil)
			e = readErr
			if e == nil {
				committed, staged := map[string]int64{}, map[string]int64{}
				for _, b := range inventory.CommittedBlocks {
					if b == nil || b.Name == nil || b.Size == nil {
						e = errors.New("incomplete block inventory")
						break
					}
					committed[*b.Name] = *b.Size
				}
				for _, b := range inventory.UncommittedBlocks {
					if b == nil || b.Name == nil || b.Size == nil {
						e = errors.New("incomplete block inventory")
						break
					}
					staged[*b.Name] = *b.Size
				}
				if e == nil {
					e = g.ImportBlocks(ctx, committed, staged)
				}
			}
		}
		if e == nil {
			_, e = g.BlockListSize(ctx, ids)
		}
		err = e
	}
	if err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

// Managed storage requires provider history to be off: the application's Bin
// and revision ledger own recovery. Never release quota based on an overwrite
// receipt while the provider may also retain hidden copies. Account changes are
// operator changes; this adapter observes them without changing those settings.
func (u *AzureBlobUploader) checkRetention(ctx context.Context, version *string) error {
	if !u.meterWrites {
		return nil
	}
	if version != nil && *version != "" {
		return u.blockRetention(ctx, "Azure versioning was detected. Managed writes are stopped; reconcile retained history before reopening storage.")
	}
	props, err := u.client.ServiceClient().GetProperties(ctx, nil)
	if err != nil {
		return errors.New("storage retention settings could not be verified; capacity is retained")
	}
	if props.DeleteRetentionPolicy != nil && props.DeleteRetentionPolicy.Enabled != nil && *props.DeleteRetentionPolicy.Enabled {
		return u.blockRetention(ctx, "Azure soft deletion was detected. Managed writes are stopped; reconcile retained history before reopening storage.")
	}
	return nil
}

func (u *AzureBlobUploader) blockRetention(ctx context.Context, reason string) error {
	if s := capacityStore.Load(); s != nil {
		if e := s.BlockAccount(ctx, u.client.URL(), reason); e != nil {
			return errors.Join(errors.New(reason), e)
		}
	}
	return errors.New(reason)
}

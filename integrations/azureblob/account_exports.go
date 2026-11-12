package azureblob

// account_exports.go -- the two narrow paths the account export run uses
// (accounts program, piece 2; design section B "Bytes" and section F "Where it
// is stored").
//
//   - AccountExports writes and reads finished export files, and nothing but
//     keys under exports/accounts/ in the installation's container.
//   - AccountBytes reads the bytes an account's rows point at, so the run can
//     copy them into the file. It never writes.
//
// Both act with the node's storage credential, which is why neither takes a
// container from its caller: the container is the one this client was
// configured with (MEMQL_AZURE_BLOB_CONTAINER), and every reference is resolved
// and judged against it before storage is asked anything. Neither is a DSL
// capability (integrations/CLAUDE.md, azureblob): the export run is the only
// caller, and it decides which account and which rows.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

const (
	// accountExportPrefix is the one place export files live.
	accountExportPrefix = "exports/accounts/"
	// exportBlockBytes is the most an export stream holds in memory: one block.
	exportBlockBytes = 8 << 20
	// maxExportBlocks is Azure's limit on committed blocks per blob, and the
	// capacity ledger's limit on one block list (storagequota.selected).
	maxExportBlocks = 50000
	// clusterOwner is the capacity ledger's owner for the cluster's own bytes.
	// An export belongs to the cluster, not to the person who started it
	// (design section F), so it is never charged to the caller's allowance.
	clusterOwner = ""
)

// ErrNotAccountExport refuses a key outside exports/accounts/, before storage
// is asked anything.
var ErrNotAccountExport = errors.New("storage key is not an account export")

// ErrBytesReferenceRefused is how AccountBytesReader refuses a reference that
// resolves outside the place it was given or below its own floor: a refusal of
// the reference, which the export records and goes on, never a failure of
// storage, which fails the run. Callers test it with errors.Is.
var ErrBytesReferenceRefused = errors.New("bytes reference refused")

// AccountExportStore is the one writer and reader of exports/accounts/.
type AccountExportStore struct{ u *AzureBlobUploader }

// AccountExports is the one writer and reader of exports/accounts/. Any other key is refused.
func (u *AzureBlobUploader) AccountExports() *AccountExportStore {
	return &AccountExportStore{u: u}
}

func (s *AccountExportStore) target(object string) (string, error) {
	if !accountExportKey(object) {
		return "", fmt.Errorf("%w: %q", ErrNotAccountExport, object)
	}
	return configuredContainer(s.u)
}

// accountExportKey reports whether object is a file under exports/accounts/.
// fs.ValidPath refuses `..`, `.`, empty segments and a leading or trailing
// slash, so a key that would escape the prefix after the service normalizes it
// is refused here rather than rewritten.
func accountExportKey(object string) bool {
	rest, ok := strings.CutPrefix(object, accountExportPrefix)
	return ok && rest != "" && plainObjectKey(object)
}

// plainObjectKey is a container-relative object name written the way storage
// will read it: no surrounding space, no scheme, no backslash or control
// separator, and no segment that names a different place.
func plainObjectKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && !strings.Contains(key, "://") &&
		!strings.ContainsAny(key, "\\\x00\r\n") && fs.ValidPath(key) && key != "."
}

func configuredContainer(u *AzureBlobUploader) (string, error) {
	if u == nil || u.client == nil {
		return "", errors.New("azure blob uploader not initialized")
	}
	if u.libraryContainer == "" {
		return "", errors.New("no storage container is configured")
	}
	return u.libraryContainer, nil
}

// ExportStream writes one export file as it is produced. It holds one block in
// memory, stages it when full, and commits the block list once.
type ExportStream struct {
	store     *AccountExportStore
	ctx       context.Context
	container string
	object    string
	attempt   string
	buf       []byte
	blocks    []string
	size      int64
	hash      hash.Hash
	state     exportStreamState
	err       error
}

type exportStreamState int

const (
	exportStreamOpen exportStreamState = iota
	exportStreamCommitted
	exportStreamAborted
)

// Create opens a stream to object. Nothing is visible at object until Commit.
//
// Each attempt writes to a key of its own. A failed or cancelled attempt ends
// with Abort, which leaves the key writable; Delete is only for a person's
// removal of a finished file, and retires the key for good, so a later Create
// of the same key is refused at its first block.
//
// ctx governs every block the stream stages from Write, which has no context of
// its own (io.Writer). Commit and Abort take their own.
func (s *AccountExportStore) Create(ctx context.Context, object string) (*ExportStream, error) {
	container, err := s.target(object)
	if err != nil {
		return nil, err
	}
	return &ExportStream{
		store: s, ctx: ctx, container: container, object: object,
		// Block ids are unique to this attempt, so a retried run on another
		// replica never commits a block an earlier attempt staged. rand.Text is
		// fixed-length, which keeps every id the same length as Azure requires.
		attempt: rand.Text(),
		buf:     make([]byte, 0, exportBlockBytes),
		hash:    sha256.New(),
	}, nil
}

// Write stages a block every 8 MiB. A failed stage is sticky: every later call,
// Commit included, answers the same error.
func (w *ExportStream) Write(p []byte) (int, error) {
	if w.state != exportStreamOpen {
		return 0, errors.New("export stream is closed")
	}
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), exportBlockBytes-len(w.buf))
		w.buf = append(w.buf, p[:n]...)
		_, _ = w.hash.Write(p[:n])
		w.size += int64(n)
		written += n
		p = p[n:]
		if len(w.buf) == exportBlockBytes {
			if err := w.stage(w.ctx); err != nil {
				w.err = err
				return written, err
			}
		}
	}
	return written, nil
}

func (w *ExportStream) stage(ctx context.Context) error {
	if len(w.buf) == 0 {
		return nil
	}
	if len(w.blocks) >= maxExportBlocks {
		return fmt.Errorf("export stream exceeds %d blocks", maxExportBlocks)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	u := w.store.u
	id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%08d", w.attempt, len(w.blocks)))
	bc, err := u.blockClient(w.container, w.object)
	if err != nil {
		return err
	}
	capacity, err := u.beginCapacityForOwner(ctx, w.container, w.object, clusterOwner)
	if err != nil {
		return err
	}
	if capacity != nil {
		defer capacity.Close()
		if err := capacity.Stage(ctx, id, int64(len(w.buf))); err != nil {
			return err
		}
	}
	if _, err := bc.StageBlock(ctx, id, streaming.NopCloser(bytes.NewReader(w.buf)), nil); err != nil {
		return fmt.Errorf("stage export block: %w", err)
	}
	w.blocks = append(w.blocks, id)
	w.buf = w.buf[:0]
	return nil
}

// Commit stages what is left and commits the block list, answering the file's
// size and lowercase hex SHA-256, both computed as the bytes passed through
// Write. An empty stream is refused: an export always holds its manifest.
func (w *ExportStream) Commit(ctx context.Context) (size int64, sha256 string, err error) {
	if w.state != exportStreamOpen {
		return 0, "", errors.New("export stream is closed")
	}
	if w.err != nil {
		return 0, "", w.err
	}
	if err := w.stage(ctx); err != nil {
		w.err = err
		return 0, "", err
	}
	if len(w.blocks) == 0 {
		return 0, "", errors.New("export stream is empty; nothing was committed")
	}
	u := w.store.u
	bc, err := u.blockClient(w.container, w.object)
	if err != nil {
		return 0, "", err
	}
	if u.meterWrites {
		capacity, err := u.commitCapacityForOwner(ctx, w.container, w.object, clusterOwner, w.blocks)
		if err != nil {
			return 0, "", err
		}
		defer capacity.Close()
		response, err := bc.CommitBlockList(ctx, w.blocks, nil)
		if err != nil {
			return 0, "", fmt.Errorf("commit export block list: %w", err)
		}
		if err := u.checkRetention(ctx, response.VersionID); err != nil {
			return 0, "", err
		}
		if err := capacity.BlocksCommitted(ctx, w.blocks, true); err != nil {
			return 0, "", err
		}
	} else if _, err := bc.CommitBlockList(ctx, w.blocks, nil); err != nil {
		return 0, "", fmt.Errorf("commit export block list: %w", err)
	}
	w.state, w.buf = exportStreamCommitted, nil
	return w.size, hex.EncodeToString(w.hash.Sum(nil)), nil
}

// Abort ends the stream with nothing committed and removes what it staged.
//
// Azure has no call that discards staged blocks alone, so Abort commits an
// EMPTY block list, which discards them, and deletes the empty object that
// leaves. Waiting for the service to expire them (about a week) would keep
// their bytes charged to the cluster and leave a cancelled run's staging in
// place, where the design says a cancel removes it. The key is this export's
// own, so whatever an earlier attempt committed there goes too; the run starts
// again from the beginning in any case (design section F, "If the node dies").
func (w *ExportStream) Abort(ctx context.Context) error {
	if w.state == exportStreamCommitted {
		return errors.New("export stream is committed; delete the file instead")
	}
	w.state, w.buf = exportStreamAborted, nil
	if len(w.blocks) == 0 {
		return nil
	}
	u := w.store.u
	bc, err := u.blockClient(w.container, w.object)
	if err != nil {
		return err
	}
	capacity, err := u.beginCapacityForOwner(ctx, w.container, w.object, clusterOwner)
	if err != nil {
		return err
	}
	if capacity != nil {
		defer capacity.Close()
	}
	if _, err := bc.CommitBlockList(ctx, []string{}, nil); err != nil {
		return fmt.Errorf("discard staged export blocks: %w", err)
	}
	if capacity != nil {
		if err := capacity.BlocksCommitted(ctx, nil, true); err != nil {
			return err
		}
	}
	if _, err := bc.Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("remove aborted export: %w", err)
	}
	w.blocks = nil
	return nil
}

// Open streams a finished export. A missing file answers an error IsBlobAbsent
// recognizes.
func (s *AccountExportStore) Open(ctx context.Context, object string) (io.ReadCloser, error) {
	container, err := s.target(object)
	if err != nil {
		return nil, err
	}
	return openObject(ctx, s.u, container, object, nil)
}

// OpenRange streams count bytes of a finished export starting at offset.
func (s *AccountExportStore) OpenRange(ctx context.Context, object string, offset, count int64) (io.ReadCloser, error) {
	container, err := s.target(object)
	if err != nil {
		return nil, err
	}
	if offset < 0 || count <= 0 {
		return nil, fmt.Errorf("invalid export range: offset=%d count=%d", offset, count)
	}
	return openObject(ctx, s.u, container, object, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: offset, Count: count}})
}

// Delete removes a finished export. Deleting one that is already gone succeeds.
//
// Its ledger entry is retired, not released: a run on another replica that
// lost its claim may still hold a stream to the same key, and a retired key
// refuses that stream's next block instead of letting it bring the file back
// (storagequota.Guard.Retired). The key stays retired for good: any later
// write to it is refused. So a caller writes each attempt to a key of its own,
// ends a failed or cancelled attempt with Abort, and calls Delete only when a
// person removes a finished file.
func (s *AccountExportStore) Delete(ctx context.Context, object string) error {
	container, err := s.target(object)
	if err != nil {
		return err
	}
	bc, err := s.u.blockClient(container, object)
	if err != nil {
		return err
	}
	capacity, err := s.u.beginCapacityForOwner(ctx, container, object, clusterOwner)
	if err != nil {
		return err
	}
	if capacity != nil {
		defer capacity.Close()
	}
	if _, err := bc.Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("delete export: %w", err)
	}
	if capacity != nil {
		// Absence is confirmed before the ledger lets go of the bytes, as
		// DeleteVerifiedStream does.
		if _, err := bc.GetProperties(ctx, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return fmt.Errorf("%w: export still present after delete: %v", ErrBlobDeleteUncertain, err)
		}
		if err := capacity.Retired(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func openObject(ctx context.Context, u *AzureBlobUploader, container, object string, opts *blob.DownloadStreamOptions) (io.ReadCloser, error) {
	resp, err := u.client.DownloadStream(ctx, container, object, opts)
	if err != nil {
		return nil, fmt.Errorf("open azure blob stream: %w", err)
	}
	if hasRetirementMarker(resp.Metadata) {
		_ = resp.Body.Close()
		return nil, ErrBlobRetired
	}
	return resp.Body, nil
}

// AccountBytesReader reads the bytes an account's rows point at, for the
// export run only. It writes nothing, takes no container, and opens only
// objects in the configured one.
//
// A row's reference is not believed. Every method takes `under`, the places the
// row's bytes may live in, which the CALLER derives from the concept's
// @bytes(under=...) declaration and the row's own values (design section C);
// the resolved key or tree must lie strictly inside one of them. Beneath that
// the reader holds a floor of its own, whatever `under` says: a key is judged
// segment by segment after decoding, nothing under `exports` is read in any
// spelling, and no tree is shallower than two segments. So a wrong `under` can
// widen a read to a wrong place in the container, but never to an export file
// and never to the whole of a top-level area.
type AccountBytesReader struct{ u *AzureBlobUploader }

// AccountBytes reads the bytes an account's rows point at.
func (u *AzureBlobUploader) AccountBytes() *AccountBytesReader { return &AccountBytesReader{u: u} }

// minTreeSegments is the shallowest tree the reader lists: `sites/<id>`, never
// `sites` or `library` alone, which would be every account's bytes at once.
const minTreeSegments = 2

// keySegments judges a container-relative key by its segments and answers them.
// It refuses an empty key or segment (so a leading, doubled or trailing slash),
// a `.` or `..` segment, surrounding space, a backslash or control separator,
// an encoded separator in any case (the raw text is checked, so a value decoded
// once already cannot hide a second encoding), and a first segment of
// `exports`, however it is cased: blob names are case-sensitive, so `EXPORTS`
// is a different object, but nothing an account owns is spelled that way and
// refusing every spelling costs nothing.
func keySegments(key string) ([]string, bool) {
	if key == "" || key != strings.TrimSpace(key) || strings.ContainsAny(key, "\\\x00\r\n") {
		return nil, false
	}
	lower := strings.ToLower(key)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return nil, false
	}
	segments := strings.Split(key, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "://") {
			return nil, false
		}
	}
	if strings.EqualFold(segments[0], "exports") {
		return nil, false
	}
	return segments, true
}

// insidePlace reports whether segments lie STRICTLY inside one of the places,
// compared segment by segment: `library/u1/f1` is inside `library/u1`, while
// `library/u10/f1` and `library/u1` itself are not. A place is judged by the
// same floor as a key, so a malformed or `exports` place matches nothing, and
// no place at all refuses everything.
func insidePlace(segments []string, under []string) bool {
	for _, place := range under {
		prefix, ok := keySegments(place)
		if !ok || len(segments) <= len(prefix) {
			continue
		}
		match := true
		for i, seg := range prefix {
			if segments[i] != seg {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// resolve turns a stored reference into a container-relative key, judged by
// the floor and by `under`. A tree's key ends in "/"; an object's never does.
//
//   - file://...      shipped inside the engine image, not in storage at all;
//   - blob://<prefix>  every object under <prefix>/ (a site bundle), tree;
//   - a URL           as the uploader stores it (splitStoredURL, which decodes
//     the path once), and only in the configured container;
//   - anything else   a bare key (a Library file, an attachment), judged as
//     written, in the configured container, the only one it can reach.
func (r *AccountBytesReader) resolve(ref string, under []string) (container, key string, tree, shipped bool, err error) {
	container, object, tree, shipped, err := r.resolveKey(ref)
	if err != nil || shipped {
		return container, "", false, shipped, err
	}
	segments, _ := keySegments(object)
	if !insidePlace(segments, under) {
		return "", "", false, false, fmt.Errorf("%w: %q names nothing this reader may read in the place it was given", ErrBytesReferenceRefused, ref)
	}
	if tree {
		return container, object + "/", true, false, nil
	}
	return container, object, false, false, nil
}

// Resolve answers the container-relative key a stored reference names, by the
// parse Stat, List and Open use, and whether it is a tree (its prefix, with no
// trailing slash) or a reference to files shipped in the engine image. It is
// judged by the reader's floor and by no place: the export asks the row's
// declaration about THIS key (memorynodes.BytesField.Admits), then reads it
// with the declaration's places, so the key judged is the key read. A
// reference the floor refuses answers ErrBytesReferenceRefused.
func (r *AccountBytesReader) Resolve(ref string) (key string, tree, shipped bool, err error) {
	_, key, tree, shipped, err = r.resolveKey(ref)
	return key, tree, shipped, err
}

// resolveKey is resolve without the place: the reference turned into its
// object in the configured container and held to the floor.
func (r *AccountBytesReader) resolveKey(ref string) (container, object string, tree, shipped bool, err error) {
	container, err = configuredContainer(r.u)
	if err != nil {
		return "", "", false, false, err
	}
	ref = strings.TrimSpace(ref)
	refused := func() (string, string, bool, bool, error) {
		return "", "", false, false, fmt.Errorf("%w: %q names nothing this reader may read", ErrBytesReferenceRefused, ref)
	}
	switch {
	case ref == "":
		return "", "", false, false, fmt.Errorf("%w: the reference is empty", ErrBytesReferenceRefused)
	case strings.HasPrefix(ref, "file://"):
		// Nothing is read for a shipped reference, so no place is needed.
		return container, "", false, true, nil
	case strings.HasPrefix(ref, "blob://"):
		// One trailing slash is how a tree is written; the opener adds it when
		// it is missing, so its absence is not a different tree.
		object, tree = strings.TrimSuffix(strings.TrimPrefix(ref, "blob://"), "/"), true
	case strings.Contains(ref, "://"):
		lower := strings.ToLower(ref)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return refused()
		}
		got, decoded, ok := r.u.splitStoredURL(ref)
		if !ok || got != container {
			return refused()
		}
		object = decoded
	default:
		object = ref
	}
	segments, ok := keySegments(object)
	if !ok || (tree && len(segments) < minTreeSegments) {
		return refused()
	}
	return container, object, tree, false, nil
}

// Stat resolves a reference (a stored URL, a bare key, or blob://<prefix>/ for a tree) to its object.
// ok is false when the object does not exist. A file:// reference answers shipped == true.
//
// under is the places the reference may resolve into (see AccountBytesReader);
// a reference outside them is an error, not a missing object. A tree answers
// its prefix as the key and the sum of its live objects' sizes; it exists when
// a live object is under it. A retired object holds no bytes and answers
// ok == false, as a missing one does.
func (r *AccountBytesReader) Stat(ctx context.Context, ref string, under []string) (info BlobInfo, ok, shipped bool, err error) {
	container, key, tree, shipped, err := r.resolve(ref, under)
	if err != nil || shipped {
		return BlobInfo{}, false, shipped, err
	}
	if tree {
		objects, err := r.listTree(ctx, container, key)
		if err != nil {
			return BlobInfo{}, false, false, err
		}
		info = BlobInfo{Key: key}
		for _, o := range objects {
			info.Size += o.Size
		}
		return info, len(objects) > 0, false, nil
	}
	bc, err := r.u.blockClient(container, key)
	if err != nil {
		return BlobInfo{}, false, false, err
	}
	props, err := bc.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return BlobInfo{}, false, false, nil
	}
	if err != nil {
		return BlobInfo{}, false, false, fmt.Errorf("stat azure blob: %w", err)
	}
	if hasRetirementMarker(props.Metadata) {
		return BlobInfo{}, false, false, nil
	}
	info = BlobInfo{Key: key}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}
	return info, true, false, nil
}

// Open streams one object by the key Stat or List answered: a bare,
// container-relative key, never a URL or a tree, inside one of under.
func (r *AccountBytesReader) Open(ctx context.Context, key string, under []string) (io.ReadCloser, error) {
	container, err := configuredContainer(r.u)
	if err != nil {
		return nil, err
	}
	segments, ok := keySegments(key)
	if !ok || !insidePlace(segments, under) {
		return nil, fmt.Errorf("%w: %q is not an object key this reader may open in the place it was given", ErrBytesReferenceRefused, key)
	}
	return openObject(ctx, r.u, container, key, nil)
}

// List answers every live object under a blob://<prefix>/ tree, with its size.
// The tree must lie inside one of under and be at least two segments deep.
func (r *AccountBytesReader) List(ctx context.Context, treeRef string, under []string) ([]BlobInfo, error) {
	if !strings.HasPrefix(strings.TrimSpace(treeRef), "blob://") {
		return nil, fmt.Errorf("%w: %q is not a blob:// tree", ErrBytesReferenceRefused, treeRef)
	}
	container, key, _, _, err := r.resolve(treeRef, under)
	if err != nil {
		return nil, err
	}
	return r.listTree(ctx, container, key)
}

// listTree lists prefix (ending in "/") and keeps only what lies inside it.
//
// The service's prefix filter is not taken on trust: every name is judged again
// by the same floor and must start with the prefix, so a listing that answered
// more than it was asked can never hand the caller an export file or another
// place's key. Retired objects are skipped, as every other reader skips them:
// a tombstone holds no bytes, and Open would refuse it with ErrBlobRetired.
// Listing asks for metadata so the retirement marker is visible here without a
// read per object. Bounded by maxListedBlobs, as ListPrefix is.
func (r *AccountBytesReader) listTree(ctx context.Context, container, prefix string) ([]BlobInfo, error) {
	var out []BlobInfo
	pager := r.u.client.NewListBlobsFlatPager(container, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix, Include: azblob.ListBlobsInclude{Metadata: true},
	})
	seen := 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list azure blobs under %q: %w", prefix, err)
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			if seen++; seen > maxListedBlobs {
				return nil, fmt.Errorf("more than %d blobs under %q; refusing to list further", maxListedBlobs, prefix)
			}
			name := *item.Name
			if _, ok := keySegments(name); !ok || !strings.HasPrefix(name, prefix) || hasRetirementMarker(item.Metadata) {
				continue
			}
			info := BlobInfo{Key: name}
			if item.Properties != nil && item.Properties.ContentLength != nil {
				info.Size = *item.Properties.ContentLength
			}
			out = append(out, info)
		}
	}
	return out, nil
}

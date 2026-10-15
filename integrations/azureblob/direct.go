package azureblob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/lease"
	"github.com/znasllc-io/memql/component/database/storagequota"
)

// DirectRequest authorizes ONE exact HTTP request. It is never logged or saved
// in a job row. A worker can change neither its length, object, block id, digest,
// lease generation nor operation. The account key never leaves this process.
type DirectRequest struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

func signDirect(account, key string, method, rawURL string, headers map[string]string, now time.Time) (DirectRequest, error) {
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) == 0 {
		return DirectRequest{}, errors.New("direct storage signing is unavailable")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return DirectRequest{}, errors.New("invalid storage destination")
	}
	headers["x-ms-version"] = "2023-11-03"
	headers["x-ms-date"] = now.UTC().Format(http.TimeFormat)
	lower := map[string]string{}
	for k, v := range headers {
		lower[strings.ToLower(k)] = v
	}
	fields := []string{method}
	for _, k := range []string{"content-encoding", "content-language", "content-length", "content-md5", "content-type", "date", "if-modified-since", "if-match", "if-none-match", "if-unmodified-since", "range"} {
		v := lower[k]
		if k == "content-length" && v == "0" {
			v = ""
		}
		fields = append(fields, v)
	}
	var names []string
	for k := range lower {
		if strings.HasPrefix(k, "x-ms-") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	canonical := ""
	for _, k := range names {
		canonical += k + ":" + lower[k] + "\n"
	}
	resource := "/" + account + u.EscapedPath()
	names = nil
	query := u.Query()
	for k := range query {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		values := query[k]
		sort.Strings(values)
		resource += "\n" + strings.ToLower(k) + ":" + strings.Join(values, ",")
	}
	mac := hmac.New(sha256.New, decoded)
	mac.Write([]byte(strings.Join(fields, "\n") + "\n" + canonical + resource))
	headers["Authorization"] = "SharedKey " + account + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return DirectRequest{Method: method, URL: rawURL, Headers: headers, ExpiresAt: now.Add(14 * time.Minute)}, nil
}

func (u *AzureBlobUploader) directSignature(method, rawURL string, headers map[string]string) (DirectRequest, error) {
	// Parse only named fields; neither the connection string nor the signature
	// appears in an error. New() already validated this same configuration.
	values := map[string]string{}
	for _, part := range strings.Split(ConnectionStringFromEnv(), ";") {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			values[k] = v
		}
	}
	if values["AccountName"] == "" || values["AccountKey"] == "" {
		return DirectRequest{}, errors.New("direct backups require account-key request signing")
	}
	return signDirect(values["AccountName"], values["AccountKey"], method, rawURL, headers, time.Now())
}

// PrepareDirect creates an empty, create-only object and acquires a durable
// lease. Retrying with the persisted lease id reconciles an uncertain reply.
func (u *AzureBlobUploader) PrepareDirect(ctx context.Context, container, object, leaseID string) error {
	bc, err := u.blockClient(container, object)
	if err != nil {
		return err
	}
	capacity, err := u.beginCapacity(ctx, container, object)
	if err != nil {
		return err
	}
	if capacity != nil {
		defer capacity.Close()
		if err = capacity.Plan(ctx, 0); err != nil && !errors.Is(err, storagequota.ErrRetired) {
			return err
		}
		if errors.Is(err, storagequota.ErrRetired) {
			p, e := bc.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &leaseID}}})
			if e != nil || p.ContentLength == nil || *p.ContentLength != 0 || p.LeaseStatus == nil || *p.LeaseStatus != lease.StatusTypeLocked {
				return storagequota.ErrRetired
			}
			return nil
		}
	}
	star := azcore.ETag("*")
	_, err = bc.Upload(ctx, streaming.NopCloser(bytes.NewReader(nil)), &blockblob.UploadOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &star}}})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet, bloberror.LeaseIDMissing) {
		return err
	}
	lc, err := lease.NewBlobClient(bc, &lease.BlobClientOptions{LeaseID: &leaseID})
	if err != nil {
		return err
	}
	_, err = lc.AcquireLease(ctx, -1, nil)
	if err != nil {
		p, verifyErr := bc.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &leaseID}}})
		if verifyErr != nil || p.LeaseStatus == nil || *p.LeaseStatus != lease.StatusTypeLocked || p.LeaseDuration == nil || *p.LeaseDuration != lease.DurationTypeInfinite {
			return errors.New("could not establish the backup write lease")
		}
	}
	p, e := bc.GetProperties(ctx, nil)
	if e != nil {
		return e
	}
	return u.checkRetention(ctx, p.VersionID)
}

// FenceDirect invalidates every outstanding grant for previous. Persist next
// before calling; repeating a lost change reply verifies the new lease.
func (u *AzureBlobUploader) FenceDirect(ctx context.Context, container, object, previous, next string) error {
	bc, err := u.blockClient(container, object)
	if err != nil {
		return err
	}
	lc, err := lease.NewBlobClient(bc, &lease.BlobClientOptions{LeaseID: &previous})
	if err != nil {
		return err
	}
	_, err = lc.ChangeLease(ctx, next, nil)
	if err == nil {
		return nil
	}
	p, err := bc.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &next}}})
	if err != nil || p.LeaseStatus == nil || *p.LeaseStatus != lease.StatusTypeLocked || p.LeaseDuration == nil || *p.LeaseDuration != lease.DurationTypeInfinite {
		return errors.New("backup write lease could not be reconciled")
	}
	return nil
}

func (u *AzureBlobUploader) DirectBlockGrant(ctx context.Context, container, object, leaseID, blockID string, size int64, md5 string) (DirectRequest, error) {
	digest, err := base64.StdEncoding.DecodeString(md5)
	if err != nil || len(digest) != 16 || size <= 0 || size > storagequota.BlockBytes {
		return DirectRequest{}, errors.New("invalid backup block length or checksum")
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return DirectRequest{}, err
	}
	capacity, err := u.beginCapacity(ctx, container, object)
	if err != nil {
		return DirectRequest{}, err
	}
	if capacity != nil {
		defer capacity.Close()
		if err = capacity.Stage(ctx, blockID, size); err != nil {
			return DirectRequest{}, err
		}
	}
	target, err := url.Parse(bc.URL())
	if err != nil {
		return DirectRequest{}, err
	}
	q := target.Query()
	q.Set("comp", "block")
	q.Set("blockid", blockID)
	target.RawQuery = q.Encode()
	return u.directSignature(http.MethodPut, target.String(), map[string]string{"Content-Length": strconv.FormatInt(size, 10), "Content-MD5": md5, "Content-Type": "application/octet-stream", "x-ms-lease-id": leaseID})
}

type DirectReceipt struct {
	URL  string `json:"url"`
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

type DirectBlock struct {
	ID   string
	Size int64
}
type DirectInventory struct {
	Committed []DirectBlock
	Staged    map[string]int64
}

func (u *AzureBlobUploader) InspectDirect(ctx context.Context, container, object, leaseID string) (DirectInventory, error) {
	bc, err := u.blockClient(container, object)
	if err != nil {
		return DirectInventory{}, err
	}
	r, err := bc.GetBlockList(ctx, blockblob.BlockListTypeAll, &blockblob.GetBlockListOptions{AccessConditions: &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &leaseID}}})
	if err != nil {
		return DirectInventory{}, err
	}
	p, e := bc.GetProperties(ctx, nil)
	if e != nil {
		return DirectInventory{}, e
	}
	if e = u.checkRetention(ctx, p.VersionID); e != nil {
		return DirectInventory{}, e
	}
	out := DirectInventory{Staged: map[string]int64{}}
	for _, b := range r.CommittedBlocks {
		if b == nil || b.Name == nil || b.Size == nil {
			return out, errors.New("storage returned an incomplete block inventory")
		}
		out.Committed = append(out.Committed, DirectBlock{ID: *b.Name, Size: *b.Size})
	}
	for _, b := range r.UncommittedBlocks {
		if b == nil || b.Name == nil || b.Size == nil {
			return out, errors.New("storage returned an incomplete block inventory")
		}
		out.Staged[*b.Name] = *b.Size
	}
	return out, nil
}

func (u *AzureBlobUploader) CommitDirect(ctx context.Context, container, object, leaseID string, blocks []string, final bool) (DirectReceipt, error) {
	bc, err := u.blockClient(container, object)
	if err != nil {
		return DirectReceipt{}, err
	}
	capacity, err := u.commitCapacity(ctx, container, object, blocks)
	if err != nil {
		return DirectReceipt{}, err
	}
	if capacity != nil {
		defer capacity.Close()
	}
	access := &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &leaseID}}
	_, commitErr := bc.CommitBlockList(ctx, blocks, &blockblob.CommitBlockListOptions{AccessConditions: access})
	// Inventory is the receipt, including after a lost commit reply. An exact
	// ordered list distinguishes our prefix from another or a truncated commit.
	inventory, err := bc.GetBlockList(ctx, blockblob.BlockListTypeAll, &blockblob.GetBlockListOptions{AccessConditions: access})
	if err != nil {
		return DirectReceipt{}, errors.Join(commitErr, err)
	}
	if len(inventory.CommittedBlocks) != len(blocks) || len(inventory.UncommittedBlocks) != 0 {
		return DirectReceipt{}, errors.New("backup checkpoint needs reconciliation")
	}
	var size int64
	for n, b := range inventory.CommittedBlocks {
		if b.Name == nil || b.Size == nil || *b.Name != blocks[n] {
			return DirectReceipt{}, errors.New("backup checkpoint inventory differs")
		}
		size += *b.Size
	}
	props, err := bc.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: access})
	if err != nil {
		return DirectReceipt{}, err
	}
	if props.ETag == nil || props.ContentLength == nil || *props.ContentLength != size {
		return DirectReceipt{}, errors.New("backup checkpoint has no exact receipt")
	}
	if capacity != nil {
		if err = u.checkRetention(ctx, props.VersionID); err != nil {
			return DirectReceipt{}, err
		}
		if err = capacity.BlocksCommitted(ctx, blocks, final); err != nil {
			return DirectReceipt{}, err
		}
	}
	reference, err := storedBlobURL(bc.URL())
	if err != nil {
		return DirectReceipt{}, err
	}
	return DirectReceipt{URL: reference, ETag: string(*props.ETag), Size: size}, nil
}

func (u *AzureBlobUploader) DirectDownloadGrant(ctx context.Context, container, object, etag string, offset, count int64) (DirectRequest, error) {
	if etag == "" || offset < 0 || count <= 0 || count > storagequota.BlockBytes || offset > storagequota.BackupBytes-count {
		return DirectRequest{}, errors.New("invalid backup download range")
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return DirectRequest{}, err
	}
	return u.directSignature(http.MethodGet, bc.URL(), map[string]string{"If-Match": etag, "Range": fmt.Sprintf("bytes=%d-%d", offset, offset+count-1)})
}

func (u *AzureBlobUploader) RetireDirect(ctx context.Context, container, object, leaseID string) error {
	receipt, err := u.CommitDirect(ctx, container, object, leaseID, nil, true)
	if err != nil {
		return err
	}
	if receipt.Size != 0 {
		return errors.New("backup cleanup was not confirmed")
	}
	capacity, err := u.beginCapacity(ctx, container, object)
	if err != nil {
		return err
	}
	if capacity == nil {
		return nil
	}
	defer capacity.Close()
	return capacity.Retired(ctx)
}

// DirectUnreserved proves no managed write could have started: every effect
// reserves first, and the caller serializes the transfer across replicas.
func (u *AzureBlobUploader) DirectUnreserved(ctx context.Context, container, object string) (bool, error) {
	g, err := u.beginCapacity(ctx, container, object)
	if err != nil {
		return false, err
	}
	if g == nil {
		return false, nil
	}
	defer g.Close()
	db, err := g.Store.Database()
	if err != nil {
		return false, err
	}
	var exists bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM storage_capacity_objects WHERE object_key=$1)`, g.Key).Scan(&exists); err != nil || exists {
		return false, err
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return false, err
	}
	_, err = bc.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return true, nil
	}
	return false, err
}

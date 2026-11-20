package library

// FILE SHARING (owner-approved 2026-11-18): one Library file, one person, 1, 7,
// 10 or 30 days. The recipient views, downloads and saves a copy; nothing else.
// Since Nov 20 2026: one GROUP instead of one person, and a picked end date up
// to 90 days away instead of a length (see the concept's comment in
// dsl/library/concepts.memql for how a group share follows membership).
//
// THE ACTS, AND WHO EACH RUNS AS
//
//	libraryShareFile        the sharer, a PERSON (not borrowed, not synthetic),
//	                        on a file they own. Writes the share under the
//	                        sharer and the recipient's notice under the
//	                        recipient, borrowed for that one write.
//	libraryShareStop        the sharer, on a share read back as theirs.
//	librarySaveSharedCopy   the recipient, through the one shared-file resolver
//	                        (component/server EngineLibraryStore.SharedFile).
//	libraryEndSharesForArtifact  the engine's endSharesOnArtifactArchive
//	                        automation only.
//
// Every write goes through ./sharestore, the package that holds the
// internal-origin stamp both concepts' @serverWritten needs. Every decision is
// made here first, under the original caller, with fresh reads.
//
// WHY A BORROWED OR SYNTHETIC ACTOR MAY NOT SHARE. A deployable's access code
// runs a visitor's call under the site owner's borrowed authority, and an
// automation acts as the cluster: neither is the person deciding to hand their
// file to somebody, and sharing is that decision.
//
// A BURST OF THE SAME ACT IS ONE ACT (José, Nov 19 call: twenty quick clicks on
// "Save to my Library" made eighteen copies; "even at 1,000 clicks a second it
// should only count as one click", "especially now with AI agents"). Each act
// here answers a burst with one result, in the act itself, so an agent calling
// the builtin with no button in between is held to it too:
//
//	libraryShareFile        the file's version gate plus renew-or-new: a second
//	                        call finds the first one's share and renews it.
//	libraryShareStop        stopping a stopped share answers as success.
//	librarySaveSharedCopy   ONE SAVE AT A TIME per person and file
//	                        (oneSaveAtATime): while one runs, the same save from
//	                        the same person is refused and makes nothing. A save
//	                        asked for AFTER the first has finished is a new copy
//	                        (José: "just leave the rate limiter", nothing
//	                        remembers which files were saved).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/libraryfile"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/librarykey"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/library/sharestore"
)

// shareLifetimes are the only lifetimes a share may have, in days.
var shareLifetimes = map[int]bool{1: true, 7: true, 10: true, 30: true}

// endSharesAutomation is the one caller libraryEndSharesForArtifact admits.
const endSharesAutomation = "endSharesOnArtifactArchive"

// ShareCopyStore writes a copy's bytes, verified, at a key that is new.
// *azureblob.AzureBlobUploader implements it.
type ShareCopyStore interface {
	CreateVerifiedStream(ctx context.Context, bucket, key string, r io.Reader, size int64, sha256, mimeType string) (azureblob.VerifiedBlob, error)
}

// SetShareCopyStorage wires where librarySaveSharedCopy writes a copy. Absent,
// a copy is refused rather than half made; reading the shared bytes uses the
// blob fetcher (SetBlobFetcher).
func (i *Integration) SetShareCopyStorage(store ShareCopyStore, bucket string) {
	i.copyStore, i.copyBucket = store, bucket
}

// SetSaveClaims wires the cross-replica claim librarySaveSharedCopy takes
// (oneSaveAtATime): the node's automation cluster guard, strict, so a claim
// the database cannot record refuses the save instead of running it unguarded.
func (i *Integration) SetSaveClaims(claims automations.StrictClusterClaimer) {
	i.saveClaims = &claims
}

// saveClaimName namespaces the save claims in automation_execution_claims.
const saveClaimName = "librarySaveSharedCopy"

// saveClaimTTL bounds a save's claim. The claim is given back the moment the
// save ends, so the TTL only matters for a replica that died mid-save: that
// person can save the file again once it lapses.
//
// ponytail: fixed lease, no heartbeat. A copy that outlasts it (a multi-GiB
// file) can be started a second time by a call made after the lease lapsed;
// renew the claim while the copy streams if that ever matters.
const saveClaimTTL = 2 * time.Minute

// errSaveInFlight is the answer to the same save asked for while one runs.
var errSaveInFlight = fmt.Errorf("a copy of this file is already being saved to your Library; this request saved nothing more")

// oneSaveAtATime claims the save of artifactID by caller on every replica, or
// says why it cannot: the same save is running (errSaveInFlight), or no claim
// is wired.
//
// The claim is a row in automation_execution_claims whose primary key admits
// one insert, the same table the automation cluster guard makes events
// exactly-once with, so it holds across replicas and holds no connection while
// the copy streams. release gives it back, also when the caller's request was
// cancelled under it. A claim the database could not record also answers
// errSaveInFlight (the strict claimer reports only won or not, and logs the
// storage failure); with the database unreachable the copy could not have been
// recorded either, and either way nothing was made.
func (i *Integration) oneSaveAtATime(ctx context.Context, caller, artifactID string) (release func(), err error) {
	if i.saveClaims == nil {
		return nil, fmt.Errorf("saving a shared file is unavailable on this node; no copy was made")
	}
	key := caller + "/" + memql.BareShortId(artifactID)
	if !i.saveClaims.ClaimWithTTL(ctx, saveClaimName, key, saveClaimTTL) {
		return nil, errSaveInFlight
	}
	return func() {
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		i.saveClaims.Release(done, saveClaimName, key)
	}, nil
}

// sharingPerson is the caller when they are a signed-in person acting for
// themselves, and "" otherwise (see the file header).
func sharingPerson(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Synthetic || ac.Unranked || ac.IsAnonymousActor() || ac.IsConnector() {
		return ""
	}
	return memql.BareShortId(strings.TrimSpace(ac.UserId))
}

// shareEngine adapts the integration's engine handle to component/server's
// executor seam, so the shared-file resolver is the one the download route runs.
type shareEngine struct{ engine memql.IntegrationEngineAccess }

func (e shareEngine) Execute(ctx context.Context, q string) (any, error) {
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (i *Integration) shareStore() *server.EngineLibraryStore {
	return server.NewEngineLibraryStore(shareEngine{i.engine})
}

// handleShareFile is libraryShareFile: share, or renew an active share.
//
// RENEW OR NEW, decided under the file's version gate. When the caller already
// holds an ACTIVE share of this file with this person (or this group), the same
// row gets the new end and its notices the same: one share, never two. When
// there is none -- never shared, stopped, or expired -- a FRESH share is made
// at a new id, and the ended one stays as the record of who had it and when.
// A fresh row rather than a reactivated one because a write is a read-merge:
// reusing an ended row would carry its revokedAt into the new share, and the
// engine has no spelling that clears a field. The gate is what keeps two
// clicks on two replicas from both finding no active share and making two.
//
// A GROUP SHARE reaches the group's active members other than the sharer, as
// the directory names them NOW (memberUserIds), each told by a notice of their
// own. A renewal re-reads them: newcomers are told, and the notices of members
// who left are ended -- they were already refused, because every access also
// checks the caller is still in the group (component/server SharedFile).
func (i *Integration) handleShareFile(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	sharer := sharingPerson(ctx)
	if sharer == "" {
		return nil, fmt.Errorf("sign in to share a file")
	}
	now := time.Now().UTC()
	expires, days, err := shareEnd(args, now)
	if err != nil {
		return nil, err
	}
	artifactID := memql.BareShortId(strings.TrimSpace(asString(args["artifactId"])))
	recipient := memql.BareShortId(strings.TrimSpace(asString(args["recipientUserId"])))
	group := memql.BareShortId(strings.TrimSpace(asString(args["recipientGroupId"])))
	if artifactID == "" || (recipient == "") == (group == "") {
		return nil, fmt.Errorf("choose a file, and one person or one group to share it with")
	}
	if recipient == sharer {
		return nil, fmt.Errorf("you cannot share a file with yourself")
	}
	ctx = memql.ContextWithFreshRead(ctx)

	// THE FILE IS THE SHARER'S OWN, read under them. libraryArtifactById is
	// owner-scoped, so somebody else's id answers like a made-up one; the
	// owner comparison closes the cluster owner's tier arm, which would
	// otherwise let an operator share a file that is not theirs.
	notMine := fmt.Errorf("you can share only a file of your own that is not in the Bin")
	artifact, err := i.revisionRow(ctx, "libraryArtifactById", map[string]any{"artifactId": artifactID})
	if err != nil {
		return nil, err
	}
	if artifact == nil || memql.BareShortId(stringField(artifact, "ownerUserId")) != sharer ||
		stringField(artifact, "kind") != "file" || boolField(artifact, "archived") {
		return nil, notMine
	}
	file, err := i.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": stringField(artifact, "sourceConceptRef")})
	if err != nil {
		return nil, err
	}
	if file == nil || memql.BareShortId(stringField(file, "ownerUserId")) != sharer || boolField(file, "archived") {
		return nil, notMine
	}

	// THE RECIPIENT IS ONE THE SHARER MAY PICK: the same answer the dialog
	// was given, asked of the same builtin. One sentence for "nobody by that
	// id" and "not yours to pick", so the write is not an oracle for who is on
	// the cluster. A group's members are asked of it too, so a group share
	// reaches exactly the people the dialog said it would.
	recipientName := ""
	raw, err := i.engine.Execute(ctx, "builtin libraryShareDirectory()")
	if err != nil {
		return nil, err
	}
	for _, entry := range extractRows(raw) {
		if (recipient != "" && memql.BareShortId(stringField(entry, "userId")) == recipient) ||
			(group != "" && memql.BareShortId(stringField(entry, "groupId")) == group) {
			recipientName = stringField(entry, "displayName")
		}
	}
	if recipientName == "" {
		return nil, fmt.Errorf("choose someone from the people and groups offered to share with")
	}
	reached := []string{recipient}
	if group != "" {
		call, err := langparser.RenderCall("libraryShareDirectory", map[string]any{"groupId": group})
		if err != nil {
			return nil, err
		}
		raw, err := i.engine.Execute(ctx, "builtin "+call)
		if err != nil {
			return nil, err
		}
		reached = nil
		for _, person := range extractRows(raw) {
			if member := memql.BareShortId(stringField(person, "userId")); member != "" && member != sharer {
				reached = append(reached, member)
			}
		}
		if len(reached) == 0 {
			return nil, fmt.Errorf("nobody but you is in that group yet, so there is nobody to share with")
		}
	}
	sharerName := "Unnamed person"
	if me, err := i.revisionRow(ctx, "userDisplayById", map[string]any{"userId": sharer}); err == nil && me != nil {
		if name := memql.NameWithoutEmail(stringField(me, "displayName")); name != "" {
			sharerName = name
		}
	}

	release, err := i.fileVersionGate(ctx, stringField(file, "id"))
	if err != nil {
		return nil, err
	}
	defer release()
	active, err := i.revisionRows(ctx, "librarySharesForArtifact", map[string]any{"artifactId": artifactID})
	if err != nil {
		return nil, err
	}
	shareID := "share-" + id.NewShortId()
	var renewed map[string]any
	for _, share := range active {
		same := recipient != "" && memql.BareShortId(stringField(share, "recipientUserId")) == recipient
		if group != "" {
			same = memql.BareShortId(stringField(share, "recipientGroupId")) == group
		}
		if same {
			shareID, renewed = memql.BareShortId(stringField(share, "id")), share
		}
	}
	expiresAt := expires.Format(time.RFC3339Nano)
	title := firstNonEmpty(stringField(file, "name"), stringField(artifact, "title"), "Untitled file")
	write := map[string]any{
		"shareId": shareID, "recipientDisplayName": recipientName,
		"artifactId": artifactID, "title": title, "days": days, "expiresAt": expiresAt,
	}
	if group != "" {
		write["recipientGroupId"], write["memberUserIds"] = group, reached
	} else {
		write["recipientUserId"] = recipient
	}
	if err := sharestore.Write(ctx, i.engine, "putLibraryShare", write); err != nil {
		return nil, err
	}
	notice := map[string]any{
		"shareId": shareID, "artifactId": artifactID, "sharerUserId": sharer, "sharerDisplayName": sharerName,
		"title": title, "mimeType": stringField(file, "mimeType"), "expiresAt": expiresAt,
	}
	if format := stringField(file, "format"); format != "" {
		notice["format"] = format
	}
	if size, ok := intArg(file["size"]); ok {
		notice["size"] = size
	}
	// Each recipient's own row, so it is written as them: borrowed for this
	// one write, which is what stamps them its owner.
	// ponytail: one notice write per member, in this request; a group of
	// hundreds is hundreds of writes -- batch them if groups grow that large.
	for _, person := range reached {
		notice["noticeId"] = noticeID(shareID, group, person)
		if err := sharestore.Write(auth.ContextWithUserActor(ctx, person), i.engine, "putLibraryShareNotice", notice); err != nil {
			return nil, err
		}
	}
	// A renewal ends the notices of members who have left the group since.
	if renewed != nil && group != "" {
		still := map[string]bool{}
		for _, person := range reached {
			still[person] = true
		}
		for _, person := range stringSliceField(renewed, "memberUserIds") {
			if person = memql.BareShortId(person); person != "" && !still[person] {
				i.endNotice(ctx, shareID, group, person)
			}
		}
	}
	return reviewResult(map[string]any{"shareId": shareID, "expiresAt": expiresAt, "days": days})
}

// shareMaxDays is the furthest a share may end: 90 days, for a length or a
// picked date alike (Nov 20 2026). A quarter covers the longest real hand-off
// the team described -- a client's assets for a campaign -- and keeps every
// share, and the lists that compare against the clock, short-lived.
const shareMaxDays = 90

// shareEnd decides when a share ends, from exactly one of `days` (one of the
// four lengths) or `until` (an RFC 3339 instant after now, at most
// shareMaxDays away), and the whole days that makes, rounded up.
func shareEnd(args map[string]any, now time.Time) (time.Time, int, error) {
	until := strings.TrimSpace(asString(args["until"]))
	days, hasDays := intArg(args["days"])
	switch {
	case until != "" && hasDays:
		return time.Time{}, 0, fmt.Errorf("choose a length or an end date for the share, not both")
	case until != "":
		end, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("the end date could not be read; pick it again")
		}
		end = end.UTC()
		if !end.After(now) {
			return time.Time{}, 0, fmt.Errorf("that end date has passed; pick a later one")
		}
		if end.After(now.Add(shareMaxDays * 24 * time.Hour)) {
			return time.Time{}, 0, fmt.Errorf("a share can last at most %d days; pick an earlier end date", shareMaxDays)
		}
		whole := int((end.Sub(now) + 24*time.Hour - 1) / (24 * time.Hour))
		return end, whole, nil
	case shareLifetimes[days]:
		return now.Add(time.Duration(days) * 24 * time.Hour), days, nil
	}
	return time.Time{}, 0, fmt.Errorf("a share lasts 1, 7, 10 or 30 days, or until a date at most %d days away", shareMaxDays)
}

// noticeID is where a recipient's notice of a share lives: at the share's own
// short id for a share with one person, and at `<share>-<member>` for each
// member a group share reaches, so every member's notice has its own row.
func noticeID(shareID, group, person string) string {
	if group == "" {
		return shareID
	}
	return shareID + "-" + person
}

// endNotice ends one recipient's notice. The notice is display, so one that
// cannot be ended is logged rather than failing an act that has already closed
// the door (the share decides every access).
func (i *Integration) endNotice(ctx context.Context, shareID, group, person string) {
	if err := sharestore.Write(auth.ContextWithUserActor(ctx, person), i.engine, "endLibraryShareNotice", map[string]any{"noticeId": noticeID(shareID, group, person)}); err != nil {
		i.log().Warn("library: a recipient's share notice could not be ended", "shareId", shareID, "error", err)
	}
}

// handleStopShare is libraryShareStop.
func (i *Integration) handleStopShare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	sharer := sharingPerson(ctx)
	if sharer == "" {
		return nil, fmt.Errorf("sign in to stop sharing a file")
	}
	shareID := strings.TrimSpace(asString(args["shareId"]))
	if shareID == "" {
		return nil, fmt.Errorf("choose a share to stop")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	// READ BACK AS THE CALLER'S OWN before writing: the end runs at internal
	// origin, which skips the write guard's owner check, so this read is the
	// whole of "only the sharer stops their share".
	share, err := i.revisionRow(ctx, "libraryShareById", map[string]any{"shareId": shareID})
	if err != nil {
		return nil, err
	}
	if share == nil || memql.BareShortId(stringField(share, "ownerUserId")) != sharer {
		return nil, fmt.Errorf("that share is not one of yours")
	}
	if err := i.endShare(ctx, share); err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"shareId": memql.BareShortId(shareID), "stopped": true})
}

// endShare revokes one share, read under its sharer and passed in, and ends
// its notices -- the one recipient's, or every member's a group share reached.
// The share is what access reads, so its end must land; a notice is display
// (endNotice).
func (i *Integration) endShare(sharerCtx context.Context, share map[string]any) error {
	shareID := memql.BareShortId(stringField(share, "id"))
	if stringField(share, "revokedAt") == "" {
		if err := sharestore.Write(sharerCtx, i.engine, "endLibraryShare", map[string]any{"shareId": shareID}); err != nil {
			return err
		}
	}
	group := memql.BareShortId(stringField(share, "recipientGroupId"))
	reached := []string{stringField(share, "recipientUserId")}
	if group != "" {
		reached = stringSliceField(share, "memberUserIds")
	}
	for _, person := range reached {
		if person = memql.BareShortId(person); person != "" {
			i.endNotice(sharerCtx, shareID, group, person)
		}
	}
	return nil
}

// handleEndSharesForArtifact is libraryEndSharesForArtifact: the engine's own
// endSharesOnArtifactArchive automation, at internal origin, and nobody else.
// It is handed the archived artifact and its owner off the archived row, and
// believes neither: it reads the artifact under that owner and acts only when
// it is theirs and archived.
func (i *Integration) handleEndSharesForArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || !auth.IsEngineAutomationActor(ac, endSharesAutomation) || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("the shares of an archived file are ended by the cluster's own %s automation; nothing else ends them this way", endSharesAutomation)
	}
	artifactID := memql.BareShortId(strings.TrimSpace(asString(args["artifactId"])))
	owner := memql.BareShortId(strings.TrimSpace(asString(args["ownerUserId"])))
	if artifactID == "" || owner == "" {
		return reviewResult(map[string]any{"artifactId": artifactID, "ended": 0})
	}
	// The owner's own reads, unstamped: the internal origin the automation
	// ran under is not carried into them.
	asOwner := memql.ContextWithFreshRead(auth.ContextWithUserActor(auth.ContextWithClientOrigin(ctx), owner))
	artifact, err := i.revisionRow(asOwner, "libraryArtifactById", map[string]any{"artifactId": artifactID})
	if err != nil {
		return nil, err
	}
	if artifact == nil || !boolField(artifact, "archived") || memql.BareShortId(stringField(artifact, "ownerUserId")) != owner {
		return reviewResult(map[string]any{"artifactId": artifactID, "ended": 0})
	}
	shares, err := i.revisionRows(asOwner, "librarySharesForArtifact", map[string]any{"artifactId": artifactID})
	if err != nil {
		return nil, err
	}
	for _, share := range shares {
		if err := i.endShare(asOwner, share); err != nil {
			return nil, err
		}
	}
	return reviewResult(map[string]any{"artifactId": artifactID, "ended": len(shares)})
}

// handleSaveSharedCopy is librarySaveSharedCopy: the recipient's own copy, as a
// new file in their Library, made while the share is open.
func (i *Integration) handleSaveSharedCopy(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	caller := sharingPerson(ctx)
	if caller == "" || !i.canManageDocument(ctx, auth.VerbCreate) {
		return nil, fmt.Errorf("sign in to save a copy of a shared file")
	}
	artifactID := strings.TrimSpace(asString(args["artifactId"]))
	if artifactID == "" {
		return nil, fmt.Errorf("choose a shared file to copy")
	}
	if i.copyStore == nil || i.copyBucket == "" || i.blobFetcher == nil {
		return nil, fmt.Errorf("Library storage is unavailable on this node; no copy was made")
	}
	// Before anything is read: the same save while one runs makes nothing.
	release, err := i.oneSaveAtATime(ctx, caller, artifactID)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	store := i.shareStore()
	artifact, file, err := store.SharedFile(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if artifact == nil || file == nil {
		return nil, fmt.Errorf("this file is not shared with you, or its share has ended")
	}
	// The sharer's key, held to the sharer, before a byte is read -- the
	// content route's own rule.
	if err := librarykey.Check(file.BlobUrl, file.OwnerUserId, ""); err != nil {
		i.log().Error("library: a shared file names a blob key outside its owner's library; refusing to copy it",
			"artifactId", artifactID, "fileId", file.ID, "error", err)
		return nil, fmt.Errorf("this file is not shared with you, or its share has ended")
	}
	if err := server.CheckStorageAdmission(ctx, i.copyStore, store.StorageFootprint, int64(file.Size), server.LibraryUserQuotaBytes()); err != nil {
		return nil, err
	}

	// Spool once to learn the exact length and digest the verified write
	// commits against: the source row's sha256 may not be measured yet.
	source, err := i.blobFetcher.DownloadStreamURL(ctx, file.BlobUrl)
	if err != nil {
		return nil, fmt.Errorf("read the shared file: %w", err)
	}
	defer source.Close()
	spool, err := os.CreateTemp("", "memql-shared-copy-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(spool, hash), source)
	if err != nil {
		return nil, fmt.Errorf("read the shared file: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	name := server.SanitizeLibraryFileName(firstNonEmpty(file.Name, artifact.Title, "shared-file"))
	mimeType := firstNonEmpty(file.MimeType, "application/octet-stream")
	fileID := "shared-copy-" + id.NewShortId()
	key := "library/" + caller + "/" + fileID + "/" + name
	stored, err := i.copyStore.CreateVerifiedStream(ctx, i.copyBucket, key, spool, size, digest, mimeType)
	if err != nil {
		return nil, fmt.Errorf("store the copy: %w", err)
	}
	if stored.Size != size || stored.SHA256 != digest || strings.TrimSpace(stored.URL) == "" {
		return nil, fmt.Errorf("the stored copy failed its integrity check")
	}
	create := map[string]any{
		"fileId": fileID, "name": name, "mimeType": mimeType, "size": size, "sha256": digest,
		"blobUrl": stored.URL, "source": "derived", "format": firstNonEmpty(file.Format, "other"),
	}
	if file.Summary != "" {
		create["summary"] = file.Summary
	}
	if err := libraryfile.NewStore(shareEngine{i.engine}).Create(ctx, create); err != nil {
		return nil, fmt.Errorf("record the copy in your Library: %w", err)
	}
	// A copy is read like an upload (Jesús, Nov 18: saved copies are searchable
	// by meaning). An opaque one is finished now, as an opaque upload is in the
	// upload route's startAnalysis; a readable one goes through the reading pass
	// once it is promoted, below.
	readable := firstNonEmpty(file.Format, "other") != "other"
	if !readable {
		if err := i.execMutation(ctx, "setLibraryFileStatus", map[string]any{"fileId": fileID, "status": "ready", "embeddingStatus": "none"}); err != nil {
			return nil, fmt.Errorf("finish the copy: %w", err)
		}
	}
	// PROMOTED HERE, not left to indexFileOnCreate, so the answer can name the
	// artifact. The id derives from the file reference, so the automation's
	// own promotion of the same file versions this same row.
	ref := server.LibraryFileConceptRef(fileID)
	promoted, err := i.revisionRow(ctx, "libraryArtifactForFile", map[string]any{"fileId": fileID})
	if err != nil {
		return nil, err
	}
	if promoted == nil {
		call, err := langparser.RenderCall("createArtifact", map[string]any{
			"sourceConceptRef": ref, "ownerUserId": caller, "lens": "artifact", "kind": "file", "source": "derived",
			"title": name, "summary": file.Summary, "format": firstNonEmpty(file.Format, "other"), "mimeType": mimeType,
			"live": false, "archived": false, "folderId": "",
		})
		if err != nil {
			return nil, err
		}
		raw, err := i.engine.Execute(ctx, "mutation "+call)
		if err != nil {
			return nil, fmt.Errorf("add the copy to your Library: %w", err)
		}
		if rows := extractRows(raw); len(rows) > 0 {
			promoted = rows[0]
		}
	}
	copyID := memql.BareShortId(stringField(promoted, "id"))
	if readable {
		i.startCopyAnalysis(ctx, AnalyzeFileParams{
			FileId: fileID, ArtifactId: copyID, OwnerUserId: caller, Name: name, MimeType: mimeType,
			BlobUrl: stored.URL, Sha256: digest,
		})
	}
	return reviewResult(map[string]any{"artifactId": copyID, "fileId": fileID})
}

// startCopyAnalysis runs the reading pass on a saved copy after the answer goes
// out, detached the way the upload route detaches it (auth.DetachUserWork keeps
// the recipient's authority). If it cannot start, the copy is marked ready so it
// never reads as "analysis pending" forever.
func (i *Integration) startCopyAnalysis(ctx context.Context, params AnalyzeFileParams) {
	work, cancel, err := auth.DetachUserWork(ctx, params.OwnerUserId)
	if err != nil {
		i.log().Warn("library: a saved copy could not be read", "fileId", params.FileId, "error", err)
		if err := i.execMutation(ctx, "setLibraryFileStatus", map[string]any{"fileId": params.FileId, "status": "ready", "embeddingStatus": "none"}); err != nil {
			i.log().Warn("library: mark a saved copy ready", "fileId", params.FileId, "error", err)
		}
		return
	}
	go func() {
		defer cancel()
		if err := i.AnalyzeFile(work, params); err != nil {
			i.log().Warn("library: reading a saved copy failed", "fileId", params.FileId, "error", err)
		}
	}()
}

func (i *Integration) execMutation(ctx context.Context, name string, args map[string]any) error {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return err
	}
	_, err = i.engine.Execute(ctx, "mutation "+call)
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

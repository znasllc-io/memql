package library

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/library/reviewstore"
)

// Application is proven by an accepted item AND its immutable document save,
// not by an old selection, an approval alone, or the latest run's status. This
// also covers older reviews and a save whose run lost its final acknowledgment.
// The caller has admitted doc under the original identity; deletion holds the
// same cross-replica document lock as application throughout this read.
func (i *Integration) appliedReviewComments(ctx context.Context, doc reviewDocument, comments []map[string]any) (map[string]bool, error) {
	wanted, applied := map[string]bool{}, map[string]bool{}
	versionSet := map[int]bool{}
	for _, row := range comments {
		if stringField(row, "purpose") != "note" {
			wanted[memql.BareShortId(stringField(row, "id"))] = true
			version, ok := intArg(row["versionNumber"])
			if !ok || version < 0 {
				return nil, fmt.Errorf("could not verify the feedback’s document version")
			}
			versionSet[version] = true
		}
	}
	if len(wanted) == 0 {
		return applied, nil
	}
	ctx = memql.ContextWithCursor(memql.ContextWithFreshRead(ctx), "")
	// Only the versions represented by these comments can have applied them.
	// This keeps a current-page read independent of unrelated document history.
	requestedVersions := make([]int, 0, len(versionSet))
	for version := range versionSet {
		requestedVersions = append(requestedVersions, version)
	}
	sort.Ints(requestedVersions)
	versions := map[int]map[string]any{}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := reviewstore.ApprovedRevisions(ctx, i.engine, doc.artifact, after, requestedVersions)
		if err != nil {
			return nil, err
		}
		page := extractRows(raw)
		for _, receipt := range page {
			if receipt["documentKind"] != doc.kind || memql.BareShortId(asString(receipt["sourceId"])) != memql.BareShortId(doc.source) {
				continue
			}
			items, err := revisionItems(receipt)
			if err != nil {
				return nil, err
			}
			encoded, _ := json.Marshal(receipt["acceptedItemIds"])
			var accepted []string
			if err = json.Unmarshal(encoded, &accepted); err != nil {
				return nil, fmt.Errorf("could not verify the saved review decision")
			}
			matched := []string{}
			for _, item := range items {
				for _, id := range accepted {
					if item.ID == id {
						for _, comment := range item.CommentIDs {
							comment = memql.BareShortId(comment)
							if wanted[comment] && !applied[comment] {
								matched = append(matched, comment)
							}
						}
					}
				}
			}
			if len(matched) == 0 {
				continue
			}
			version, valid := intArg(receipt["version"])
			if !valid || version < 0 {
				return nil, fmt.Errorf("could not verify the applied document version")
			}
			version++
			stored, read := versions[version]
			if !read {
				if doc.kind == "file" && doc.version == version {
					stored = doc.backing
				} else {
					raw, err := reviewstore.VersionReceipt(ctx, i.engine, doc.owner, doc.source, version, doc.kind == "file")
					if err != nil {
						return nil, err
					}
					if rows := extractRows(raw); len(rows) == 1 {
						stored = rows[0]
					}
				}
				versions[version] = stored
			}
			if savedRevisionReceipt(doc.kind, stored, asString(receipt["runId"])) {
				for _, comment := range matched {
					applied[comment] = true
				}
			}
		}
		next := ""
		if meta := raw.GetMeta(); meta != nil {
			next = meta.Cursor
		}
		if next == "" || len(applied) == len(wanted) {
			return applied, nil
		}
		if next == after || len(page) == 0 {
			return nil, fmt.Errorf("could not finish reading the document review history")
		}
		after = next
	}
}

func savedRevisionReceipt(kind string, stored map[string]any, run string) bool {
	run = memql.BareShortId(run)
	if stored == nil || run == "" {
		return false
	}
	if kind == "generated_output" {
		return memql.BareShortId(asString(stored["producedByRunId"])) == run
	}
	// File saves address immutable blobs by operation and content. The head or
	// its retained version must name that exact blob, not merely a newer version.
	hash := sha256.Sum256([]byte("revision-" + run))
	digest := asString(stored["sha256"])
	return len(digest) == 64 && strings.HasSuffix(asString(stored["blobUrl"]), fmt.Sprintf("/revisions/%x/%s/document.md", hash, digest))
}

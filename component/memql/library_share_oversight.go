package memql

// WHOSE FILE SHARES A PERSON MAY SEE AND STOP (item 13 of José's Nov 19 2026
// call; Jesús's pick, Nov 20, draft B: a "Shared by" drop-down on the Files
// app's Sharing page).
//
// Jesús's case: someone shares the wrong document with a client and leaves for
// the weekend; somebody senior has to be able to see "what everybody is
// sharing" and cancel it. José's rule (45:39-46:05): "owners and developers
// should be able to do that, but developers shouldn't be able to manage
// owners' files" -- "It's a trickle-down thing." -- "There could be more than
// one owner." Built as:
//
//   owner       every share, other owners' included
//   developer   their own, and those of anyone ranked below them (admins and
//               everyone else) -- not owners', not other developers'
//   admin       their own, and those of anyone ranked below them -- not
//               owners', developers' or other admins'. A GUESS: José only said
//               "And then admins, same, whatever."
//   anyone else only their own
//
// i.e. an owner oversees everyone; anyone at admin rank or above oversees the
// people ranked STRICTLY below them; everybody keeps their own. Ranks come from
// the cluster's role ladder (rankLadder), so a custom role slots in where its
// rank puts it, exactly as the row-authz rank tier reads it.
//
// SEE ONLY WHAT YOU CAN STOP (Jesús, Nov 20): developers and admins are listed
// exactly the shares this rule lets them stop, nothing more, so the list is the
// rule. libraryShareStop asks this same builtin before it stops somebody else's
// share (integrations/library/share.go), so the list and the act cannot
// disagree.
//
// A BORROWED, SYNTHETIC OR CLIENT-ACCOUNT CALLER oversees nobody: the rank is
// a person's, and authority lent to server-side work or narrowed to one
// client's account is not a person's seniority.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// LibrarySharesOverseenConcept names the virtual rows librarySharesOverseen
// answers with. Nothing is persisted.
const LibrarySharesOverseenConcept = "v1:library:shareOverseen"

const conceptLibraryShare = "v1:library:share"
const conceptLibraryArtifact = "v1:library:artifact"

// shareOversight is one caller's answer to "whose shares are mine to see and
// stop".
type shareOversight struct {
	caller     string // bare
	callerRank int
	ownerRank  int
	adminRank  int
	ladder     roleLadder
	roles      map[string]string // bare user id -> role slug
}

// oversees reports whether the caller may see and stop sharer's shares.
func (o shareOversight) oversees(sharer string) bool {
	sharer = BareShortId(strings.TrimSpace(sharer))
	if sharer == "" {
		return false
	}
	if sharer == o.caller {
		return true
	}
	if o.callerRank >= o.ownerRank {
		return true
	}
	if o.callerRank < o.adminRank {
		return false
	}
	return o.ladder.rankOf(o.roles[sharer]) < o.callerRank
}

// evaluateLibrarySharesOverseenExpression serves `librarySharesOverseen`: one
// row per ACTIVE share the caller may see and stop, their own included,
// newest first.
func (e *MemQLEngine) evaluateLibrarySharesOverseenExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	ac, ok := auth.AccessFromContext(ctx)
	caller := BareShortId(strings.TrimSpace(actingUserFromContext(ctx)))
	if !ok || ac == nil || caller == "" || ac.IsAnonymousActor() || ac.IsConnector() {
		return nil, fmt.Errorf("librarySharesOverseen: sign in to see the files being shared")
	}
	ladder := e.rankLadder(ctx)
	o := shareOversight{
		caller:    caller,
		ownerRank: ladder.rankOf(string(auth.RoleOwner)),
		adminRank: ladder.rankOf(string(auth.RoleAdmin)),
		ladder:    ladder,
		roles:     map[string]string{},
	}
	// Rank 0 -- nobody but yourself -- for authority that is not a person's
	// own seniority (see the file header).
	if !ac.Unranked && !ac.Synthetic && auth.RoleAccountScope(ac.Role) == "" {
		o.callerRank = ladder.rankOf(string(ac.Role))
	}

	users, err := e.readLatestRows(ctx, conceptIdentityUser, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for i := range users {
		payload := accountRowPayload(users[i])
		id := BareShortId(strings.TrimSpace(users[i].ID))
		if payload == nil || id == "" {
			continue
		}
		// The same field principalRoles ranks a principal by.
		o.roles[id] = strings.TrimSpace(stringFromAny(payload["role"]))
		names[id] = sharePersonName(payload)
	}

	shares, err := e.readLatestRows(ctx, conceptLibraryShare, nil)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	type listed struct {
		id      string
		created time.Time
		payload map[string]any
	}
	var out []listed
	untitled := map[string]struct{}{}
	for i := range shares {
		p := accountRowPayload(shares[i])
		if p == nil {
			continue
		}
		sharer := BareShortId(strings.TrimSpace(stringFromAny(p["ownerUserId"])))
		expires, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stringFromAny(p["expiresAt"])))
		if err != nil || !expires.After(now) || strings.TrimSpace(stringFromAny(p["revokedAt"])) != "" || !o.oversees(sharer) {
			continue
		}
		artifact := BareShortId(strings.TrimSpace(stringFromAny(p["artifactId"])))
		row := map[string]any{
			"id":                   BareShortId(shares[i].ID),
			"ownerUserId":          sharer,
			"ownerDisplayName":     firstNonBlank(names[sharer], "Unnamed person"),
			"mine":                 sharer == caller,
			"recipientUserId":      BareShortId(strings.TrimSpace(stringFromAny(p["recipientUserId"]))),
			"recipientGroupId":     BareShortId(strings.TrimSpace(stringFromAny(p["recipientGroupId"]))),
			"recipientDisplayName": strings.TrimSpace(stringFromAny(p["recipientDisplayName"])),
			"artifactId":           artifact,
			"title":                strings.TrimSpace(stringFromAny(p["title"])),
			"days":                 p["days"],
			"expiresAt":            expires.Format(time.RFC3339Nano),
			"createdAt":            shares[i].CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		if row["title"] == "" && artifact != "" {
			untitled[artifact] = struct{}{}
		}
		out = append(out, listed{id: BareShortId(shares[i].ID), created: shares[i].CreatedAt, payload: row})
	}

	// A share made before shares carried the file's name (Nov 20 2026) is
	// named from its artifact; a deleted name reads as "Untitled file".
	if len(untitled) > 0 {
		ids := make([]string, 0, 2*len(untitled))
		for id := range untitled {
			ids = append(ids, id, conceptLibraryArtifact+":"+id)
		}
		artifacts, err := e.readLatestRows(ctx, conceptLibraryArtifact, ids)
		if err != nil {
			return nil, err
		}
		titles := map[string]string{}
		for i := range artifacts {
			if p := accountRowPayload(artifacts[i]); p != nil {
				titles[BareShortId(artifacts[i].ID)] = strings.TrimSpace(stringFromAny(p["title"]))
			}
		}
		for _, l := range out {
			if l.payload["title"] == "" {
				l.payload["title"] = firstNonBlank(titles[l.payload["artifactId"].(string)], "Untitled file")
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.After(out[j].created)
		}
		return out[i].id < out[j].id
	})
	rows := make([]memorynodes.MemoryNode, 0, len(out))
	for _, l := range out {
		raw, err := json.Marshal(l.payload)
		if err != nil {
			return nil, err
		}
		rows = append(rows, memorynodes.MemoryNode{
			ID: l.id, Concept: LibrarySharesOverseenConcept, Type: memorynodes.NodeTypeObject, Payload: raw,
		})
	}
	return rows, nil
}

// staged-data: INDIFFERENT -- the oversight list reads people (for their
// rank and name), every file share, and the artifacts behind untitled
// shares. v1:library:share rows are written only by the engine's own sharing
// acts (integrations/library/sharestore, internal origin), never through a
// staged promotion; were one staged, it would be listed here and stoppable,
// while every access path -- which reads shares through the gated engine
// seam -- already refuses it. Stopping a share that grants nothing harms
// nothing, and the user and artifact reads only name what is listed.
//
// readLatestRows reads the newest version of every row of one concept, or of
// the rows with the given ids: rows are append-only, so a stop or a removal is
// a NEW version, and without the DISTINCT ON collapse an older version would
// still list or rank somebody.
func (e *MemQLEngine) readLatestRows(ctx context.Context, concept string, ids []string) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("librarySharesOverseen: no database")
	}
	var nodes []memorynodes.MemoryNode
	q := db.NewSelect().Model(&nodes).DistinctOn("id").Where("concept = ?", concept)
	if ids != nil {
		q = q.Where("id IN (?)", bun.In(ids))
	}
	if err := q.OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx); err != nil {
		return nil, fmt.Errorf("librarySharesOverseen: read %s: %w", concept, err)
	}
	return nodes, nil
}

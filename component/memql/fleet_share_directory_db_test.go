package memql

// Sharing a machine with people (epic memql#5344): who an owner may pick, what
// the write accepts, and that rows written before the sharing block closed
// still write. Against a real database, because the directory is three
// concepts' rows and the write's refusals are decided by what is stored.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// shareDirActor is a signed-in person with a stated role. ContextWithUserActor
// is not used here: it is BORROWED authority, stamped Unranked, and the
// directory's admin roll-up is exactly the question it must not answer for one.
func shareDirActor(ctx context.Context, userId string, role auth.Role) context.Context {
	claims := map[string]any{"sub": userId, "role": string(role)}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userId, Role: role})
}

type shareDirFixture struct {
	t      *testing.T
	eng    *MemQLEngine
	db     *bun.DB
	prefix string
}

func (f shareDirFixture) id(concept, name string) string {
	return concept + ":" + f.prefix + "-" + name
}

func (f shareDirFixture) insert(concept, id string, payload map[string]any) {
	f.t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(payload)
	require.NoError(f.t, err)
	// THE SCHEMA CARRIES THE CONCEPT'S $id, as every row the engine writes
	// does. A raw row with `{}` has no $id, which compares EQUAL to a concept
	// with no delete variant -- so the write path's prior-row read treats it
	// as deleted, and update() answers "no existing row" for a row every
	// query can see.
	schema, err := json.Marshal(map[string]any{"$id": concept})
	require.NoError(f.t, err)
	row := memorynodes.MemoryNode{
		ID: id, CreatedAt: time.Now().UTC(), CreatedBy: "system:share-directory-test", Concept: concept,
		Type: memorynodes.NodeTypeObject, Schema: schema, Payload: raw,
		Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
	}
	_, err = f.db.NewInsert().Model(&row).Exec(ctx)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", concept).Where("id = ?", id).Exec(context.Background())
	})
}

func (f shareDirFixture) person(name string, active bool) string {
	id := f.id(conceptIdentityUser, name)
	f.insert(conceptIdentityUser, id, map[string]any{
		"active": active, "displayName": strings.ToUpper(name[:1]) + name[1:] + " " + f.prefix,
		"primaryEmail": name + "-" + f.prefix + "@example.test", "role": "writer",
	})
	return id
}

func (f shareDirFixture) group(name, status string) string {
	id := f.id(conceptIdentityGroup, name)
	f.insert(conceptIdentityGroup, id, map[string]any{
		"ownerUserId": "", "name": strings.ToUpper(name[:1]) + name[1:] + " " + f.prefix, "kind": "custom", "status": status,
	})
	return id
}

func (f shareDirFixture) member(groupId, userId, status string) {
	id := conceptIdentityGroupMembership + ":" + BareShortId(groupId) + "-" + BareShortId(userId)
	f.insert(conceptIdentityGroupMembership, id, map[string]any{
		"ownerUserId": "", "groupId": groupId, "userId": userId, "origin": "added", "status": status,
	})
}

func (f shareDirFixture) machine(name, owner string) string {
	id := f.id("v1:worker:registration", name)
	f.insert("v1:worker:registration", id, map[string]any{
		"ownerUserId": owner, "name": name, "identityId": f.id("v1:identity:identity", name),
		"capabilities": []string{"MODEL"}, "concurrency": map[string]any{"MODEL": 1},
		"capabilityDescriptor": map[string]any{"inferenceServe": "cluster"},
		"registeredAt":         time.Now().UTC().Format(time.RFC3339Nano),
	})
	return id
}

// sharingOf reads the newest stored `sharing` block of a registration.
func (f shareDirFixture) sharingOf(registrationId string) map[string]any {
	f.t.Helper()
	var node memorynodes.MemoryNode
	err := f.db.NewSelect().Model(&node).Where("concept = ?", "v1:worker:registration").
		Where("id = ?", registrationId).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background())
	require.NoError(f.t, err)
	var payload map[string]any
	require.NoError(f.t, json.Unmarshal(node.Payload, &payload))
	sharing, _ := payload["sharing"].(map[string]any)
	return sharing
}

type shareDirectoryAnswer struct {
	MachineId string `json:"machineId"`
	Everyone  bool   `json:"everyone"`
	People    []struct {
		Id     string `json:"id"`
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"people"`
	Groups []struct {
		Id      string `json:"id"`
		Name    string `json:"name"`
		Members int    `json:"members"`
	} `json:"groups"`
	Current struct {
		People []struct {
			Id          string `json:"id"`
			Name        string `json:"name"`
			InDirectory bool   `json:"inDirectory"`
		} `json:"people"`
		Groups []struct {
			Id          string `json:"id"`
			Name        string `json:"name"`
			InDirectory bool   `json:"inDirectory"`
		} `json:"groups"`
	} `json:"current"`
}

func (f shareDirFixture) directory(ctx context.Context, registrationId string) shareDirectoryAnswer {
	f.t.Helper()
	nodes, err := f.eng.evaluateFleetShareDirectoryExpression(ctx, map[string]any{"registrationId": registrationId})
	require.NoError(f.t, err)
	require.Len(f.t, nodes, 1)
	var out shareDirectoryAnswer
	require.NoError(f.t, json.Unmarshal(nodes[0].Payload, &out))
	return out
}

func (f shareDirFixture) share(ctx context.Context, registrationId, mode string, userIds, groupIds []string) error {
	args := map[string]any{"registrationId": registrationId, "mode": mode}
	if userIds != nil {
		args["userIds"] = toAnyList(userIds)
	}
	if groupIds != nil {
		args["groupIds"] = toAnyList(groupIds)
	}
	_, err := f.eng.evaluateFleetSetSharingExpression(ctx, args)
	return err
}

func toAnyList(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func shareRefusalCode(err error) string {
	var r modelPullRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func dirPeopleIds(a shareDirectoryAnswer) []string {
	out := make([]string, 0, len(a.People))
	for _, p := range a.People {
		out = append(out, p.Id)
	}
	return out
}

func dirGroupIds(a shareDirectoryAnswer) []string {
	out := make([]string, 0, len(a.Groups))
	for _, g := range a.Groups {
		out = append(out, g.Id)
	}
	return out
}

func TestShareDirectoryOffersCoMembersAndAnAdminEveryone(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("sharedir")}
	ana, bo, cy, dee := f.person("ana", true), f.person("bo", true), f.person("cy", true), f.person("dee", false)
	ada := f.person("ada", true)
	design, ops, old := f.group("design", "active"), f.group("ops", "active"), f.group("old", "archived")
	f.member(design, ana, "active")
	f.member(design, bo, "active")
	f.member(design, dee, "active") // an inactive person is never offered
	f.member(ops, cy, "active")
	f.member(old, ana, "active") // an archived group is never offered, and places nobody
	f.member(old, cy, "active")
	anasMachine := f.machine("anas-mac", ana)
	adasMachine := f.machine("adas-box", ada)

	// A PLAIN USER sees the people they share an active group with, and those
	// groups -- no email, and never themselves (design G2).
	asAna := shareDirActor(context.Background(), ana, auth.RoleWriter)
	got := f.directory(asAna, anasMachine)
	require.False(t, got.Everyone, "a plain user is not offered the whole cluster")
	require.Equal(t, []string{BareShortId(bo)}, dirPeopleIds(got), "ana's co-members: bo only (dee is inactive, cy is in no group ana is in, old is archived)")
	require.Equal(t, []string{BareShortId(design)}, dirGroupIds(got))
	require.Empty(t, got.People[0].Detail, "a plain user must not be handed anybody's email")
	require.Equal(t, 2, got.Groups[0].Members, "members counts ACTIVE people with an active membership -- ana and bo, not dee")

	// AN ADMIN sees every active person, with the email they already see in
	// Users, and every active group.
	asAda := shareDirActor(context.Background(), ada, auth.RoleAdmin)
	all := f.directory(asAda, adasMachine)
	require.True(t, all.Everyone)
	offered := map[string]string{}
	for _, p := range all.People {
		offered[p.Id] = p.Detail
	}
	for _, want := range []string{ana, bo, cy} {
		require.Contains(t, offered, BareShortId(want), "an admin is offered every active person")
	}
	require.NotContains(t, offered, BareShortId(dee), "an inactive person is never offered")
	require.NotContains(t, offered, BareShortId(ada), "nobody is offered their own machine")
	require.Equal(t, "bo-"+f.prefix+"@example.test", offered[BareShortId(bo)])
	require.Contains(t, dirGroupIds(all), BareShortId(design))
	require.Contains(t, dirGroupIds(all), BareShortId(ops))
	require.NotContains(t, dirGroupIds(all), BareShortId(old), "an archived group is never offered")

	// THE DIRECTORY ANSWERS ONLY AN OWNER, about one of their own machines: a
	// person cannot enumerate anybody through somebody else's machine.
	_, err := eng.evaluateFleetShareDirectoryExpression(asAna, map[string]any{"registrationId": adasMachine})
	require.Equal(t, "not_your_machine", shareRefusalCode(err))
}

func TestFleetSetSharingLendsOnlyToPeopleTheOwnerCanPick(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("setsharing")}
	ana, bo, cy := f.person("ana", true), f.person("bo", true), f.person("cy", true)
	design := f.group("design", "active")
	f.member(design, ana, "active")
	f.member(design, bo, "active")
	machine := f.machine("anas-mac", ana)
	asAna := shareDirActor(context.Background(), ana, auth.RoleWriter)

	// Somebody outside ana's directory is refused, with the one sentence that
	// does not say whether the id exists (design G9).
	err := f.share(asAna, machine, "people", []string{cy}, nil)
	require.Equal(t, "share_subject_unknown", shareRefusalCode(err), "cy shares no group with ana: %v", err)
	require.Nil(t, f.sharingOf(machine), "a refused write must leave the row untouched")
	err = f.share(asAna, machine, "people", []string{"v1:identity:user:nobody-" + f.prefix}, nil)
	require.Equal(t, "share_subject_unknown", shareRefusalCode(err), "an id that exists nowhere gets the SAME refusal")

	// A co-member and the group are accepted; the owner's own id is dropped
	// rather than refused.
	require.NoError(t, f.share(asAna, machine, "people", []string{ana, bo}, []string{BareShortId(design)}))
	stored := f.sharingOf(machine)
	require.Equal(t, "people", stored["mode"])
	require.Equal(t, []any{bo}, stored["userIds"], "the owner is never stored in their own list")
	require.Equal(t, []any{BareShortId(design)}, stored["groupIds"])
	require.Equal(t, ana, stored["sharedBy"])

	// bo leaves design: he is no longer somebody ana can pick, but he is still
	// on the list -- so the directory names him, marked, and re-saving the list
	// with him still on it is accepted. Only NEW names are checked.
	f.member(design, bo, "removed")
	dir := f.directory(asAna, machine)
	require.Len(t, dir.Current.People, 1)
	require.Equal(t, BareShortId(bo), dir.Current.People[0].Id)
	require.False(t, dir.Current.People[0].InDirectory, "a stored subject who left the directory is marked")
	require.NotEmpty(t, dir.Current.People[0].Name, "and still named, so the owner can see who to remove")
	require.Len(t, dir.Current.Groups, 1)
	require.True(t, dir.Current.Groups[0].InDirectory)
	require.NoError(t, f.share(asAna, machine, "people", []string{bo}, []string{design}), "an existing subject is not re-checked")
	err = f.share(asAna, machine, "people", []string{bo, cy}, nil)
	require.Equal(t, "share_subject_unknown", shareRefusalCode(err), "a NEW subject still is")

	// The shape refusals.
	require.Equal(t, "share_needs_someone", shareRefusalCode(f.share(asAna, machine, "people", nil, nil)))
	require.Equal(t, "share_needs_someone", shareRefusalCode(f.share(asAna, machine, "people", []string{ana}, nil)),
		"a list naming only the owner names nobody")
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("v1:identity:user:%s-many-%d", f.prefix, i)
	}
	require.Equal(t, "share_too_many", shareRefusalCode(f.share(asAna, machine, "people", many, nil)))
	require.Equal(t, "invalid_sharing_mode", shareRefusalCode(f.share(asAna, machine, "shared", nil, nil)))

	// Changing mode clears the lists (design G10): a share taken back leaves no
	// names behind for a later reader to honour.
	require.NoError(t, f.share(asAna, machine, "owner", []string{bo}, []string{design}))
	stored = f.sharingOf(machine)
	require.Equal(t, "owner", stored["mode"])
	require.Equal(t, []any{}, stored["userIds"])
	require.Equal(t, []any{}, stored["groupIds"])
	require.NoError(t, f.share(asAna, machine, "cluster", nil, nil))
	require.Equal(t, "cluster", f.sharingOf(machine)["mode"])
}

func TestARowSharedBeforeTheBlockClosedStillWrites(t *testing.T) {
	// G11. Every heartbeat and every owner edit is a read-merge that validates
	// the MERGED payload, so a stored `sharing` the closed block did not accept
	// would make every shared machine's row unwritable -- the memql#5199 brick.
	// The positive case is a row exactly as the pre-epic setWorkerSharing wrote
	// it; the negative control proves the block really is closed, so the
	// positive case is a real claim rather than a pass-through.
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("sharingblock")}
	ana := f.person("ana", true)
	asAna := shareDirActor(context.Background(), ana, auth.RoleWriter)

	legacy := f.id("v1:worker:registration", "legacy")
	f.insert("v1:worker:registration", legacy, map[string]any{
		"ownerUserId": ana, "name": "legacy", "identityId": f.id("v1:identity:identity", "legacy"),
		"capabilities": []string{"MODEL"}, "concurrency": map[string]any{"MODEL": 1},
		"registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"sharing":      map[string]any{"mode": "cluster", "sharedAt": time.Now().UTC().Format(time.RFC3339Nano), "sharedBy": ana},
	})
	_, err := eng.Execute(asAna, renameCall(t, legacy))
	require.NoError(t, err, "a row shared before the block closed must still accept an ordinary write")

	stray := f.id("v1:worker:registration", "stray")
	f.insert("v1:worker:registration", stray, map[string]any{
		"ownerUserId": ana, "name": "stray", "identityId": f.id("v1:identity:identity", "stray"),
		"capabilities": []string{"MODEL"}, "concurrency": map[string]any{"MODEL": 1},
		"registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"sharing":      map[string]any{"mode": "cluster", "sharedWith": "everyone"},
	})
	_, err = eng.Execute(asAna, renameCall(t, stray))
	require.Error(t, err, "NEGATIVE CONTROL: a key the block does not declare must be refused, or the positive case proves nothing")
	require.Contains(t, err.Error(), "sharedWith", "and refused FOR that key, not for some other reason the fixture could trip")
}

func renameCall(t *testing.T, registrationId string) string {
	t.Helper()
	call, err := langparser.RenderCall("renameWorker", map[string]any{"registrationId": registrationId, "displayName": "Renamed"})
	require.NoError(t, err)
	return call
}

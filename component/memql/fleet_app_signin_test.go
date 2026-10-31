package memql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// recordedRelay stands in for the replica holding the machine.
type recordedRelay struct {
	mu      sync.Mutex
	actions []AppSignInAction
	holders []string
	err     error
}

func (r *recordedRelay) RelayAppSignIn(_ context.Context, holder string, action AppSignInAction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, action)
	r.holders = append(r.holders, holder)
	return r.err
}


func signInMachineRow() map[string]any {
	return map[string]any{
		"id": "machine", "name": "Laptop", "connectedNodeId": "agent-1",
		"apps":           []any{map[string]any{"id": "codex", "allowed": true}},
		"appDescriptors": []any{map[string]any{"id": "codex", "signIn": true}},
	}
}

func TestPlanAppSignInRefusesWhatCannotHappen(t *testing.T) {
	for code, change := range map[string]func(map[string]any){
		"machine_revoked": func(r map[string]any) { r["revokedAt"] = "2026-10-30T00:00:00Z" },
		"apps_disabled":   func(r map[string]any) { r["ownerFeatures"] = map[string]any{"apps": false} },
		"app_not_installed": func(r map[string]any) {
			r["apps"] = []any{}
		},
		"app_not_allowed": func(r map[string]any) {
			r["apps"] = []any{map[string]any{"id": "codex", "allowed": false}}
		},
		// A Cockpit that predates the field reports no such claim, and would ignore the
		// message and leave a person waiting.
		"sign_in_unsupported": func(r map[string]any) {
			r["appDescriptors"] = []any{map[string]any{"id": "codex"}}
		},
		"machine_offline": func(r map[string]any) { r["connectedNodeId"] = "" },
	} {
		row := signInMachineRow()
		change(row)
		_, err := planAppSignIn(row, "codex")
		refusal, ok := err.(modelPullRefusal)
		if !ok || refusal.Code != code {
			t.Errorf("%s: got %v", code, err)
		}
	}
	if _, err := planAppSignIn(signInMachineRow(), "a-third-app"); err == nil {
		t.Error("an app outside the closed set was accepted")
	}
	holder, err := planAppSignIn(signInMachineRow(), "codex")
	if err != nil || holder != "agent-1" {
		t.Fatalf("got %q, %v", holder, err)
	}
}

// The whole act against the real concept, query and mutations: the row a person
// watches, what reaches the machine, and who may do any of it.
func TestFleetAppSignInDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("app-signin")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	raw, _ := json.Marshal(map[string]any{
		"ownerUserId": owner, "identityId": "test", "name": "Laptop", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"connectedNodeId": "agent-1",
		"apps":            []map[string]any{{"id": "claude-code", "allowed": true, "signedIn": false, "subscription": "unknown"}},
		"appDescriptors":  []map[string]any{{"id": "claude-code", "harness": "claude-headless", "signIn": true}},
	})
	row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err := db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", registration).Exec(context.Background())
	})
	relay := &recordedRelay{}
	eng.SetAppSignInRelay(relay)
	t.Cleanup(func() { eng.SetAppSignInRelay(nil) })
	ctx := auth.ContextWithUserActor(context.Background(), owner)

	// Nobody else: not a stranger, and not a cluster owner either.
	for _, other := range []context.Context{
		auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+key+"-stranger"),
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:" + key + "-operator", Role: auth.RoleOwner}),
	} {
		_, err = eng.evaluateFleetAppSignIn(other, map[string]any{"registrationId": registration, "app": "claude-code"})
		require.Error(t, err)
	}
	require.Empty(t, relay.actions, "the machine was told about a sign-in its owner did not start")

	nodes, err := eng.evaluateFleetAppSignIn(ctx, map[string]any{"registrationId": registration, "app": "claude-code"})
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
	signInId := result["signInId"].(string)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id LIKE ?", "%"+signInId).Exec(context.Background())
	})
	require.Len(t, relay.actions, 1)
	require.Equal(t, AppSignInStart, relay.actions[0].Action)
	require.Equal(t, "agent-1", relay.holders[0])
	require.Equal(t, owner, relay.actions[0].OwnerUserId)

	signIn, err := eng.appSignInForCaller(ctx, signInId)
	require.NoError(t, err)
	require.Equal(t, "requested", mapString(signIn, "status"))

	// The machine answers, under the owner, as component/worker writes it.
	call, err := parser.RenderCall("updateAppSignIn", map[string]any{"signInId": signInId, "status": "waiting", "url": "https://example.invalid/page", "needsCode": true, "updatedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = eng.Execute(auth.ContextWithInternalOrigin(ctx), call)
	require.NoError(t, err)

	// A row nobody else may read: the tier is plain owner, with no cluster-owner bypass.
	operator := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:" + key + "-operator", Role: auth.RoleOwner})
	_, err = eng.appSignInForCaller(operator, signInId)
	require.Error(t, err, "someone other than the machine's owner read a sign-in")
	_, err = eng.evaluateFleetAppSignInCode(operator, map[string]any{"signInId": signInId, "code": "theirs"})
	require.Error(t, err)

	// What the person pasted reaches the machine and no row.
	const pasted = "pasted-code-1234#state-5678"
	for _, bad := range []string{"", "two\nlines", strings.Repeat("x", maxAppSignInCode+1)} {
		_, err = eng.evaluateFleetAppSignInCode(ctx, map[string]any{"signInId": signInId, "code": bad})
		require.Error(t, err)
	}
	_, err = eng.evaluateFleetAppSignInCode(ctx, map[string]any{"signInId": signInId, "code": "  " + pasted + "\n"})
	require.NoError(t, err)
	require.Equal(t, pasted, relay.actions[len(relay.actions)-1].Code)
	var versions []memorynodes.MemoryNode
	require.NoError(t, db.NewSelect().Model(&versions).Where("id LIKE ?", "%"+signInId).Scan(context.Background()))
	require.NotEmpty(t, versions)
	for _, version := range versions {
		require.NotContains(t, string(version.Payload), "pasted-code", "a pasted code reached a row")
	}

	// A finished sign-in takes no more codes, and its row holds no address.
	call, err = parser.RenderCall("updateAppSignIn", map[string]any{"signInId": signInId, "status": "succeeded", "updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "endedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = eng.Execute(auth.ContextWithInternalOrigin(ctx), call)
	require.NoError(t, err)
	signIn, err = eng.appSignInForCaller(ctx, signInId)
	require.NoError(t, err)
	require.Equal(t, "succeeded", mapString(signIn, "status"))
	require.Empty(t, mapString(signIn, "url"))
	_, err = eng.evaluateFleetAppSignInCode(ctx, map[string]any{"signInId": signInId, "code": pasted})
	require.Error(t, err)
}

// The row must not sit at requested when the machine could not be told, and the
// person must read why.
func TestFleetAppSignInDBClosesTheRowWhenTheMachineCannotBeTold(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("app-signin-refused")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	raw, _ := json.Marshal(map[string]any{
		"ownerUserId": owner, "identityId": "test", "name": "Laptop", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"connectedNodeId": "agent-gone",
		"apps":            []map[string]any{{"id": "codex", "allowed": true, "signedIn": false, "subscription": "unknown"}},
		"appDescriptors":  []map[string]any{{"id": "codex", "harness": "codex-app-server", "signIn": true}},
	})
	row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err := db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", registration).Exec(context.Background())
	})
	eng.SetAppSignInRelay(&recordedRelay{err: errors.New("the replica holding that machine (agent-gone) is not reachable from this node")})
	t.Cleanup(func() { eng.SetAppSignInRelay(nil) })
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	_, err = eng.evaluateFleetAppSignIn(ctx, map[string]any{"registrationId": registration, "app": "codex"})
	require.ErrorContains(t, err, "not reachable")
	res, err := eng.Execute(ContextWithFreshRead(ctx), "query appSignInsForWorker(workerId: "+parser.QuoteString(registration)+")")
	require.NoError(t, err)
	rows := MaterializeRows(res)
	require.Len(t, rows, 1)
	require.Equal(t, "failed", mapString(rows[0], "status"))
	require.Contains(t, mapString(rows[0], "errorMessage"), "not reachable")
}

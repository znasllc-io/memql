package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
)

// A MACHINE'S SHARING IS ITS OWNER'S CONSENT, AND A RAW WRITE CANNOT GIVE IT
// FOR THEM (memql#5658).
//
// v1:worker:registration declares @rowAuthz(owner="ownerUserId", clusterOwner),
// so the write guard's cluster-owner escape admits a cluster owner onto every
// machine in the cluster. #5335 made setWorkerSharing @serverOnly and routed it
// through fleetSetSharing, which resolves the machine through the caller's own;
// the raw insert()/update() literal names no construct, never consults
// @serverOnly, and kept landing. Since #5344 the block names people and groups,
// so that write could lend somebody's machine to anyone at all.
//
// Every refusal below also reads the stored row: a refusal raised after the
// write landed would otherwise pass.

func TestAClusterOwnerCannotRewriteSharingOnAMachineTheyDoNotOwn(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("sharing-guard")}
	ana := f.person("ana", true)
	operator := f.person("operator", true)
	stranger := f.person("stranger", true)
	machine := f.machine("anas-mac", ana)
	asOperator := shareDirActor(context.Background(), operator, auth.RoleOwner)

	// The raw insert() literal is the one inline write the query surface
	// takes, and aimed at an existing id it is a read-merge onto that row --
	// an update by another spelling. The two cases are the two shapes #5344
	// gave the block: everyone, and named people.
	writes := map[string]string{
		"lending it to the cluster": rawRegistrationWrite(machine, map[string]any{
			"sharing": map[string]any{"mode": "cluster", "userIds": []string{}, "groupIds": []string{}},
		}),
		"lending it to one named person": rawRegistrationWrite(machine, map[string]any{
			"sharing": map[string]any{"mode": "people", "userIds": []string{BareShortId(stranger)}, "groupIds": []string{}},
		}),
	}
	for name, statement := range writes {
		t.Run(name, func(t *testing.T) {
			_, err := eng.Execute(asOperator, statement)
			require.Error(t, err, "a raw insert() %s, by a cluster owner who does not own the machine, was ADMITTED. "+
				"The tier's cluster-owner escape lets the write through and nothing judges the block", name)
			require.Contains(t, err.Error(), "sharing", "the refusal should name the block it protects, got %v", err)
			require.Nil(t, f.sharingOf(machine), "a refused write must leave the consent untouched")
		})
	}

	// The update{} form: what the body of an inline or authored mutation bound
	// to this concept reaches, with no template's @serverOnly in front of it.
	// Execute takes no update(...) literal, so it is driven through
	// executeMutation, which is where every update{} body lands.
	t.Run("an update{} body lending it to the cluster", func(t *testing.T) {
		ctx := provenance.ContextWithProvenance(asOperator, provenance.Direct("rawUpdate:v1:worker:registration"))
		_, err := eng.executeMutation(ctx, MutationNode{
			Kind:       ast.MutationKindUpdate,
			Concept:    WorkerRegistrationConcept,
			ID:         machine,
			PayloadRaw: `{"sharing": {"mode": "cluster", "userIds": [], "groupIds": []}}`,
		})
		require.Error(t, err, "an update{} of a machine the operator does not own lent it to the cluster")
		require.Nil(t, f.sharingOf(machine), "a refused write must leave the consent untouched")
	})

	// The two-step version: take the machine, then lend it as its "owner"
	// through fleetSetSharing. The owner field is @serverSet and changes only
	// through server code, so moving it is refused for the same reason -- by
	// the raw literal, and by createWorkerRegistration aimed at an existing id,
	// whose template stamps the caller as the owner over the stored one.
	takeovers := map[string]string{
		"a raw insert() naming the operator as owner": rawRegistrationWrite(machine, map[string]any{"ownerUserId": operator}),
		"createWorkerRegistration over the existing id": fmt.Sprintf(
			`mutation createWorkerRegistration(registrationId: %s, identityId: "takeover", name: "taken", `+
				`capabilities: ["MODEL"], concurrency: {MODEL: 1}, registeredAt: "2026-09-27T12:00:00Z")`,
			langparser.QuoteString(machine)),
	}
	for name, statement := range takeovers {
		t.Run(name, func(t *testing.T) {
			_, err := eng.Execute(asOperator, statement)
			require.Error(t, err, "%s moved a machine the operator does not own onto them, after which "+
				"fleetSetSharing would treat them as its owner", name)
			require.Equal(t, ana, storedRegistrationOwner(t, f, machine), "a refused takeover must leave the owner untouched")
		})
	}
	require.Equal(t, "not_your_machine", shareRefusalCode(f.share(asOperator, machine, "cluster", nil, nil)),
		"fleetSetSharing still resolves the machine through the caller's own, and it is not the operator's")
}

// The owner's own raw write is refused too: fleetSetSharing is the block's one
// writer, and it is where the people and groups named are checked against the
// ones the owner may pick. A raw write would skip that check.
func TestAnOwnersRawWriteCannotSkipFleetSetSharing(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("sharing-guard-owner")}
	ana := f.person("ana", true)
	outsider := f.person("outsider", true)
	machine := f.machine("anas-mac", ana)
	asAna := shareDirActor(context.Background(), ana, auth.RoleWriter)

	_, err := eng.Execute(asAna, rawRegistrationWrite(machine, map[string]any{
		"sharing": map[string]any{"mode": "people", "userIds": []string{BareShortId(outsider)}, "groupIds": []string{}},
	}))
	require.Error(t, err, "the owner's raw write named somebody fleetSetSharing would have refused, and landed")
	require.Nil(t, f.sharingOf(machine))
}

// What must keep working: (a) the owner's own fleetSetSharing, (b) a write
// stamped with internal origin, (c) fleetRevokeMachine's deliberate cluster-
// owner arm, and (d) the operator Fleet view's cluster-wide read.
func TestTheLegitimateSharingAndOperatorPathsStillWork(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("sharing-guard-legit")}
	ana := f.person("ana", true)
	operator := f.person("operator", true)
	lent := f.machine("anas-mac", ana)
	offboarded := f.machine("anas-old-laptop", ana)
	asAna := shareDirActor(context.Background(), ana, auth.RoleWriter)
	asOperator := shareDirActor(context.Background(), operator, auth.RoleOwner)

	// (a)
	require.NoError(t, f.share(asAna, lent, "cluster", nil, nil), "the owner's own fleetSetSharing was refused")
	stored := f.sharingOf(lent)
	require.Equal(t, "cluster", stored["mode"])
	require.Equal(t, ana, stored["sharedBy"])

	// A write that does not touch the block passes the guard: every heartbeat,
	// rename and label edit writes this row, and the read-merge carries the
	// consent through unchanged.
	_, err := eng.Execute(asAna, fmt.Sprintf(`mutation renameWorker(registrationId: %s, displayName: "Ana's Mac")`,
		langparser.QuoteString(BareShortId(lent))))
	require.NoError(t, err, "a rename carries `sharing` through the read-merge unchanged and must not be judged as a change to it")
	require.Equal(t, "cluster", f.sharingOf(lent)["mode"], "the rename must not have disturbed the consent")

	// (b)
	internal := auth.ContextWithInternalOrigin(asOperator)
	_, err = eng.Execute(internal, rawRegistrationWrite(lent, map[string]any{
		"sharing": map[string]any{"mode": "owner", "userIds": []string{}, "groupIds": []string{}},
	}))
	require.NoError(t, err, "an internal-origin write of the block was refused; server code is the block's writer")
	require.Equal(t, "owner", f.sharingOf(lent)["mode"])

	// (c)
	nodes, err := eng.evaluateFleetRevokeMachineExpression(asOperator, map[string]any{
		"registrationId": offboarded, "reason": "offboarding",
	})
	require.NoError(t, err, "fleetRevokeMachine's cluster-owner arm was refused; offboarding a laptop is an operator act (#5335)")
	require.Len(t, nodes, 1)
	require.NotEmpty(t, storedRegistrationField(t, f, offboarded, "revokedAt"), "the operator's removal did not land")

	// (d)
	res, err := eng.Execute(asOperator, `query allWorkersWithStatus()`)
	require.NoError(t, err)
	blob := resultBlob(t, res)
	for _, machine := range []string{lent, offboarded} {
		require.Contains(t, blob, BareShortId(machine), "the operator's cluster-wide Fleet read lost a machine its owner did not share")
	}
}

// rawRegistrationWrite renders the raw insert() literal onto one existing
// registration: a read-merge, so only the fields named change.
func rawRegistrationWrite(registrationId string, payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:worker:registration"), langparser.QuoteString(registrationId), string(raw))
}

func storedRegistrationOwner(t *testing.T, f shareDirFixture, registrationId string) string {
	t.Helper()
	owner := storedRegistrationField(t, f, registrationId, "ownerUserId")
	if !strings.HasPrefix(owner, conceptIdentityUser+":") {
		owner = conceptIdentityUser + ":" + owner
	}
	return owner
}

// storedRegistrationField reads one field off the newest stored version of a
// registration, straight off the table, so a tier cannot hide the answer.
func storedRegistrationField(t *testing.T, f shareDirFixture, registrationId, field string) string {
	t.Helper()
	var node memorynodes.MemoryNode
	err := f.db.NewSelect().Model(&node).Where("concept = ?", "v1:worker:registration").
		Where("id = ?", registrationId).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background())
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(node.Payload, &payload))
	s, _ := payload[field].(string)
	return s
}

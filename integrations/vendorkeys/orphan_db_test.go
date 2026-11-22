package vendorkeys

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// orphan_db_test.go -- a key and the engine's record of it are two rows and
// two writes, and the second one can fail. What a key with no record does, and
// what an add that half-landed leaves behind.

// refusingEngine fails every statement that names one of its constructs, and
// passes the rest to a real engine.
type refusingEngine struct {
	inner   Engine
	refuses []string
}

func (e refusingEngine) Execute(ctx context.Context, query string) (any, error) {
	for _, construct := range e.refuses {
		if strings.Contains(query, construct+"(") {
			return nil, errors.New("the database did not take the write")
		}
	}
	return e.inner.Execute(ctx, query)
}

func (w *world) storeRefusing(constructs ...string) *Store {
	store := NewStore(refusingEngine{inner: liveEngine{w.eng}, refuses: constructs}, func() *sql.DB { return w.gate }, discardLogger())
	store.now = func() time.Time { return w.clock }
	return store
}

func (w *world) newKey(label string) NewKey {
	w.t.Helper()
	sealed, fingerprint, err := secret.Encrypt("plain-value-of-" + label)
	require.NoError(w.t, err)
	id := "vk-key-" + label + "-" + w.suffix
	w.keys = append(w.keys, id)
	return NewKey{Id: id, Vendor: memql.VendorAnthropic, Label: label + " " + w.suffix, Fingerprint: fingerprint,
		CheckedBy: CheckedByList, IsDefault: true, Sealed: sealedValue(sealed)}
}

// An add whose second write fails is an add that failed, and the person is
// told so. The key it wrote first must not stay behind, active and paying.
func TestAnAddWhoseRecordCannotBeWrittenLeavesNoKeyBehind(t *testing.T) {
	d := newDesk(t, "orphanadd")
	dana := d.member("dana", auth.RoleDeveloper)
	key := d.newKey("half")

	err := d.storeRefusing("createVendorKeyState").Add(context.Background(), dana, key)
	require.Error(t, err, "an add whose record was not written reported success")

	mine, err := d.store.ActiveKeysOfOwner(context.Background(), dana)
	require.NoError(t, err)
	require.Empty(t, mine, "the add failed and left an active key behind")
	require.Empty(t, d.keysFor(d.resolver, dana, true).usable, "a key from a failed add pays for calls")
	require.Equal(t, statusRemoved, d.latest(keyConcept, key.Id)["status"])
}

// A key that has no record is still a key that can stop working. With nothing
// to move, a refusal used to be dropped: the key was tried by every call for
// ever, Check now changed nothing, the daily check passed it over and its
// owner was never told.
func TestAKeyWithNoRecordIsStillJudged(t *testing.T) {
	d := newDesk(t, "orphan")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	key := d.newKey("bare")
	// Both the record and the cleanup failed: the key is there, with no record.
	require.Error(t, d.storeRefusing("createVendorKeyState", "removeVendorKey").Add(context.Background(), dana, key))
	require.Equal(t, statusActive, d.latest(keyConcept, key.Id)["status"])
	require.Len(t, d.keysFor(d.resolver, dana, true).usable, 1, "this test needs a key that pays with no record")

	d.falls(key.Id, memql.VendorKeyRefused, "invalid x-api-key")
	state := d.state(key.Id)
	require.Equal(t, StateRefused, state["state"])
	require.Equal(t, "invalid x-api-key", state["detail"])
	require.Contains(t, state["ownerUserId"], dana, "the record does not belong to the key's owner")

	asked := d.keysFor(d.resolver, dana, true)
	require.Empty(t, asked.usable, "a refused key with no record went on paying")
	require.Equal(t, []string{auditWentBad}, d.auditActions(key.Id))
	require.Equal(t, 1, count(t, d.tell(), "sent"), "the owner of a refused key with no record was not told")
}

// The daily check lists the models of a key that has no record: it is the
// longest-unchecked key there is.
func TestSweepChecksAKeyWithNoRecord(t *testing.T) {
	d := newDesk(t, "orphansweep")
	dana := d.member("dana", auth.RoleDeveloper)
	key := d.newKey("unrecorded")
	require.Error(t, d.storeRefusing("createVendorKeyState", "removeVendorKey").Add(context.Background(), dana, key))

	d.answers(memql.VendorKeyProbeRefused, "invalid x-api-key")
	reply := d.runSweep()
	require.Equal(t, 1, count(t, reply, "refused"), "the daily check said %v", reply)
	require.Equal(t, StateRefused, d.state(key.Id)["state"])
}

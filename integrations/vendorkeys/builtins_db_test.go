package vendorkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/common"
)

// builtins_db_test.go -- what a person can do to an API key, and who may
// (design record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K4, K5, K7, K11, K12 and K16, safety rules S5 to S7).
//
// The vendor is stood in for by a scripted check (desk.vendor): these tests are
// about what the cluster does with the vendor's answer, and the request that
// gets the answer is tested against a stand-in server in component/memql
// (vendor_key_probe_test.go).

// A value shaped like each vendor's key. Neither is a key.
const (
	deskAnthropicKey = "sk-ant-desk-test-value-0001"
	deskOpenAIKey    = "sk-desk-test-value-0001"
)

// desk is a world plus the builtins, with a vendor that answers what the test
// says and a log that keeps everything written to it.
type desk struct {
	*world
	in *Integration

	mu     sync.Mutex
	vendor func(vendor, key string) memql.VendorKeyProbe
	asked  []string
	logged *bytes.Buffer
}

func newDesk(t *testing.T, name string) *desk {
	t.Helper()
	d := &desk{world: newWorld(t, name), logged: &bytes.Buffer{}}
	d.vendor = func(string, string) memql.VendorKeyProbe {
		return memql.VendorKeyProbe{Verdict: memql.VendorKeyProbeGood, Models: 3, At: d.clock}
	}
	logger := slog.New(slog.NewJSONHandler(d.logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d.store.logger = logger
	d.in = NewIntegration(d.store, d.resolver, nil, d.eng, logger)
	d.in.probe = func(_ context.Context, vendor, key string) memql.VendorKeyProbe {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.asked = append(d.asked, vendor+":"+key)
		return d.vendor(vendor, key)
	}
	return d
}

// answers scripts what the vendor says to the next checks.
func (d *desk) answers(verdict, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vendor = func(string, string) memql.VendorKeyProbe {
		return memql.VendorKeyProbe{Verdict: verdict, Detail: detail, At: d.clock}
	}
}

// member writes a user with a role and returns their bare id.
func (d *desk) member(name string, role auth.Role) string {
	d.t.Helper()
	user := "vk-" + name + "-" + d.suffix
	payload, err := json.Marshal(map[string]any{
		"displayName": strings.ToUpper(name[:1]) + name[1:], "primaryEmail": user + "@example.test", "role": string(role), "active": true,
	})
	require.NoError(d.t, err)
	_, err = d.eng.Execute(seedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:identity:user"), langparser.QuoteString(user), payload))
	require.NoError(d.t, err)
	d.users = append(d.users, user)
	return user
}

// session is a person's own signed-in session.
func session(user string, role auth.Role) context.Context {
	return sessionWith(user, role, nil)
}

func sessionWith(user string, role auth.Role, extra map[string]any) context.Context {
	claims := map[string]any{"sub": user, "role": string(role)}
	for k, v := range extra {
		claims[k] = v
	}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: role})
}

func cardOf(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	require.Len(t, nodes, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &payload))
	return payload
}

func (d *desk) add(ctx context.Context, vendor, label, key string) (map[string]any, error) {
	d.t.Helper()
	nodes, err := d.in.handleAdd(ctx, map[string]any{"vendor": vendor, "label": label, "key": key}, 0)
	if err != nil {
		return nil, err
	}
	card := cardOf(d.t, nodes)
	d.keys = append(d.keys, card["id"].(string))
	return card, nil
}

func (d *desk) mustAdd(ctx context.Context, label string) map[string]any {
	d.t.Helper()
	card, err := d.add(ctx, memql.VendorAnthropic, label, deskAnthropicKey)
	require.NoError(d.t, err)
	return card
}

func (d *desk) list(ctx context.Context, all bool) (mine, lent, every []map[string]any) {
	d.t.Helper()
	nodes, err := d.in.handleForActor(ctx, map[string]any{"all": all}, 0)
	require.NoError(d.t, err)
	card := cardOf(d.t, nodes)
	rows := func(key string) []map[string]any {
		var out []map[string]any
		list, _ := card[key].([]any)
		for _, item := range list {
			out = append(out, item.(map[string]any))
		}
		return out
	}
	return rows("mine"), rows("lent"), rows("all")
}

func ids(cards []map[string]any) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c["id"].(string))
	}
	return out
}

// S5: the key is sealed where it arrives, and nothing a caller, a log reader or
// an auditor can see ever carries it.
func TestAddSealsAndNeverReturnsTheValue(t *testing.T) {
	d := newDesk(t, "add")
	dev := d.member("dana", auth.RoleDeveloper)

	nodes, err := d.in.handleAdd(session(dev, auth.RoleDeveloper), map[string]any{
		"vendor": "anthropic", "label": "  Personal  ", "key": "  " + deskAnthropicKey + "\n",
	}, 0)
	require.NoError(t, err)
	card := cardOf(t, nodes)
	keyId := card["id"].(string)
	d.keys = append(d.keys, keyId)

	require.Equal(t, []string{"anthropic:" + deskAnthropicKey}, d.asked, "the key is checked once, trimmed, before anything is saved")
	require.Equal(t, "Personal", card["label"])
	require.Equal(t, "anthropic", card["vendor"])
	require.Equal(t, "...0001", card["fingerprint"])
	require.Equal(t, true, card["isDefault"], "a person's first key at a vendor is their default")
	require.Equal(t, false, card["autoUse"])
	require.Equal(t, "good", card["state"])
	require.Equal(t, "mine", card["relation"])
	require.Equal(t, dev, card["ownerUserId"])
	require.Equal(t, "Dana", card["ownerName"])

	row := d.latest(keyConcept, keyId)
	sealed, _ := row["encryptedValue"].(string)
	plain, err := secret.Decrypt(sealed)
	require.NoError(t, err)
	require.Equal(t, deskAnthropicKey, plain, "what is stored unseals to the key")
	require.NotContains(t, sealed, deskAnthropicKey)

	raw, err := json.Marshal(nodes)
	require.NoError(t, err)
	for name, text := range map[string]string{
		"the reply": string(raw), "the log": d.logged.String(), "the audit event": d.lastAudit(keyId),
	} {
		require.NotContains(t, text, deskAnthropicKey, "%s carries the key", name)
		require.NotContains(t, text, sealed, "%s carries the sealed value", name)
	}
	require.Equal(t, []string{auditAdded}, d.auditActions(keyId))
}

// K11: a key the vendor does not accept is not saved, and neither is one
// nothing could be learned about.
func TestAddRefusesARefusedKeyAndSavesNothing(t *testing.T) {
	for verdict, says := range map[string]string{
		memql.VendorKeyProbeRefused:     "invalid x-api-key",
		memql.VendorKeyProbeCredit:      "credit balance is too low",
		memql.VendorKeyProbeUnreachable: "Anthropic could not be reached to check the key. Try again in a moment.",
	} {
		t.Run(verdict, func(t *testing.T) {
			d := newDesk(t, "add-"+verdict)
			dev := d.member("dana", auth.RoleDeveloper)
			d.answers(verdict, says)

			_, err := d.add(session(dev, auth.RoleDeveloper), "anthropic", "Personal", deskAnthropicKey)
			require.Error(t, err)
			require.Contains(t, err.Error(), says, "the refusal does not say what the vendor said")
			require.NotContains(t, err.Error(), deskAnthropicKey)
			require.Contains(t, err.Error(), "not saved")

			keys, err := d.store.ActiveKeysOfOwner(context.Background(), dev)
			require.NoError(t, err)
			require.Empty(t, keys, "a key the vendor did not accept was saved")
		})
	}

	// A value that cannot be a key is refused before the vendor is asked.
	d := newDesk(t, "add-shape")
	dev := d.member("dana", auth.RoleDeveloper)
	for _, tc := range []struct{ vendor, label, key, says string }{
		{"anthropic", "Personal", "sk-ant-admin01-desk-test", "Admin"},
		{"anthropic", "Personal", deskOpenAIKey, "sk-ant-"},
		{"openai", "Personal", "", "Paste"},
		{"mistral", "Personal", deskOpenAIKey, "Anthropic or OpenAI"},
		{"anthropic", "", deskAnthropicKey, "name"},
		{"anthropic", strings.Repeat("n", 61), deskAnthropicKey, "name"},
	} {
		_, err := d.add(session(dev, auth.RoleDeveloper), tc.vendor, tc.label, tc.key)
		require.Error(t, err)
		require.Contains(t, err.Error(), tc.says)
	}
	require.Empty(t, d.asked, "the vendor was asked about a value that could not be a key")
}

// K12: a key that is valid and may not list models is saved, and says so.
func TestAddSavesAUseOnlyKey(t *testing.T) {
	d := newDesk(t, "use-only")
	dev := d.member("dana", auth.RoleDeveloper)
	d.answers(memql.VendorKeyProbeUseOnly, "Missing scopes: api.model.read")

	card, err := d.add(session(dev, auth.RoleDeveloper), "openai", "Restricted", deskOpenAIKey)
	require.NoError(t, err)
	require.Equal(t, "use", card["checkedBy"])
	require.Equal(t, "unchecked", card["state"], "a key nothing was checked about must not read as good")
	state := d.state(card["id"].(string))
	require.Equal(t, "unchecked", state["state"])
	require.Empty(t, state["checkedAt"])

	// Check now says why it cannot check, and asks the vendor nothing.
	d.asked = nil
	nodes, err := d.in.handleCheck(session(dev, auth.RoleDeveloper), map[string]any{"keyId": card["id"]}, 0)
	require.NoError(t, err)
	require.Contains(t, cardOf(t, nodes)["note"], "checked when it is used")
	require.Empty(t, d.asked)

	// Its first served call is its check.
	d.clock = d.clock.Add(time.Minute)
	d.resolver.Observe(context.Background(), memql.VendorAccount{Kind: memql.VendorAccountKey, Vendor: memql.VendorOpenAI, KeyId: card["id"].(string), OwnerUserId: dev}, memql.VendorKeyGood, d.clock, "")
	require.Equal(t, "good", d.state(card["id"].(string))["state"])
}

// K4 as the owner settled it on 2026-11-22: API keys are bring-your-own for
// everybody. Every predefined role holds the part, so any signed-in person
// adds a key of their own. It is still a part: a role that holds nothing is
// refused by its name, and an operator's deny stops one person and nobody
// else.
func TestEveryRoleAddsAKeyAndADenyStopsOnePerson(t *testing.T) {
	d := newDesk(t, "everyone")
	registerForDispatch(t, d)
	add := func(ctx context.Context) error {
		call, err := langparser.RenderCall("vendorKeyAdd", map[string]any{"vendor": "anthropic", "label": "Personal", "key": deskAnthropicKey})
		require.NoError(t, err)
		res, err := d.eng.Execute(ctx, "builtin "+call)
		if err != nil {
			return err
		}
		rows := memql.MaterializeRows(res)
		require.Len(t, rows, 1)
		d.keys = append(d.keys, memql.BareShortId(fmt.Sprint(rows[0]["id"])))
		return nil
	}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin, auth.RoleWriter, auth.RoleReader} {
		person := d.member("any"+string(role), role)
		require.NoError(t, add(session(person, role)), "a signed-in %s could not add a key of their own", role)
	}

	// A role that holds nothing holds no part, this one included.
	holdsNothing := auth.Role("vk-holds-nothing")
	nina := d.member("nina", holdsNothing)
	err := add(session(nina, holdsNothing))
	require.Error(t, err, "a role that holds nothing added a key")
	require.Contains(t, err.Error(), "app:settings/apikeys")

	// THE OPERATOR'S DENY. Two writers; one is denied the part. A grant is read
	// through the engine's grant source, which a node installs when it starts.
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	d.eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	wes, will := d.member("wes", auth.RoleWriter), d.member("will", auth.RoleWriter)
	grant := fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "execute", resourceType: "app:settings/apikeys", effect: "deny", grantedBy: "vendor-keys-seeder")`,
		langparser.QuoteString(auth.GrantRowID(auth.SubjectKindUser, wes, auth.VerbExecute, "app:settings/apikeys")), langparser.QuoteString(wes))
	_, err = d.eng.Execute(seedContext(), grant)
	require.NoError(t, err)
	err = add(session(wes, auth.RoleWriter))
	require.Error(t, err, "a person denied the part added a key")
	require.Contains(t, err.Error(), "app:settings/apikeys")
	require.NoError(t, add(session(will, auth.RoleWriter)), "one person's deny stopped somebody else")
}

// THE OWNER'S WORDS, 2026-11-22: a key's owner uses and manages it whatever
// their role. The part gates ADDING. Seeing, renaming, checking and removing a
// key are its owner's, so a person whose role stops holding the part keeps
// every one of them over the keys they already have.
func TestAPersonManagesTheirOwnKeysWhateverTheirRole(t *testing.T) {
	d := newDesk(t, "anyrole")
	dana := d.member("dana", auth.RoleDeveloper)
	card := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")
	id := card["id"].(string)

	// Her role becomes one that holds nothing at all.
	asDana := session(dana, auth.Role("vk-holds-nothing"))
	mine, _, _ := d.list(asDana, false)
	require.Equal(t, []string{id}, ids(mine), "a person whose role holds no part no longer sees the key they own")

	_, err := d.in.handleUpdate(asDana, map[string]any{"keyId": id, "label": "Dana's, renamed"}, 0)
	require.NoError(t, err, "she could not rename her own key")
	_, err = d.in.handleCheck(asDana, map[string]any{"keyId": id}, 0)
	require.NoError(t, err, "she could not check her own key")
	_, err = d.in.handleRemove(asDana, map[string]any{"keyId": id}, 0)
	require.NoError(t, err, "she could not remove her own key")
	mine, _, _ = d.list(asDana, false)
	require.Empty(t, mine)
}

// S5: adding a key is a person's act, in their own session.
func TestAddNeedsThePersonsOwnSession(t *testing.T) {
	d := newDesk(t, "gates")
	owner := d.member("olga", auth.RoleOwner)
	registerForDispatch(t, d)

	call, err := langparser.RenderCall("vendorKeyAdd", map[string]any{"vendor": "anthropic", "label": "Personal", "key": deskAnthropicKey})
	require.NoError(t, err)
	res, err := d.eng.Execute(session(owner, auth.RoleOwner), "builtin "+call)
	require.NoError(t, err)
	rows := memql.MaterializeRows(res)
	require.Len(t, rows, 1)
	d.keys = append(d.keys, memql.BareShortId(fmt.Sprint(rows[0]["id"])))
	require.NotContains(t, fmt.Sprint(rows[0]), deskAnthropicKey)

	// THE SESSION. Every one of these holds the part through the owner's role.
	notAPerson := map[string]context.Context{
		"a service account":       sessionWith(owner, auth.RoleOwner, map[string]any{"class": "service_account"}),
		"an app session":          sessionWith(owner, auth.RoleOwner, map[string]any{"class": "app_session"}),
		"a personal access token": sessionWith(owner, auth.RoleOwner, map[string]any{"pat_id": "pat-1"}),
		"a badge":                 sessionWith(owner, auth.RoleOwner, map[string]any{"class": "badge"}),
		"a step of a run":         common.ContextWithRun(session(owner, auth.RoleOwner), common.RunContext{RunId: "v1:work:run:r1", StepKey: "s1"}),
		"borrowed authority":      auth.ContextWithUserActor(context.Background(), owner),
		"the cluster acting":      auth.ContextWithAccess(context.Background(), auth.SystemActor("sweep")),
		"nobody":                  context.Background(),
	}
	before := len(d.asked)
	for name, ctx := range notAPerson {
		_, err := d.add(ctx, "anthropic", "From "+name, deskAnthropicKey)
		require.Error(t, err, "%s added a key", name)
		require.NotContains(t, err.Error(), deskAnthropicKey)
	}
	require.Len(t, d.asked, before, "a key was sent to the vendor for a caller who may not add one")
	keys, err := d.store.ActiveKeysOfOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, keys, 1)
}

// registerForDispatch puts the desk's integration behind the real dispatch, so
// a test can go through `builtin vendorKey...` and meet the capability gate.
func registerForDispatch(t *testing.T, d *desk) {
	t.Helper()
	dispatchMu.Lock()
	t.Cleanup(dispatchMu.Unlock)
	dispatched = d.in
	t.Cleanup(func() { dispatched = nil })
	dispatchOnce.Do(func() {
		require.NoError(t, d.eng.RegisterIntegration(&dispatchedIntegration{}))
	})
}

// K7: the first key at a vendor is the default, and a person holds at most ten
// active keys per vendor.
func TestFirstKeyIsTheDefaultAndTheEleventhIsRefused(t *testing.T) {
	d := newDesk(t, "limit")
	dev := d.member("dana", auth.RoleDeveloper)
	ctx := session(dev, auth.RoleDeveloper)

	first := d.mustAdd(ctx, "Key 1")
	require.Equal(t, true, first["isDefault"])
	for n := 2; n <= maxKeysPerVendor; n++ {
		d.clock = d.clock.Add(time.Second)
		card := d.mustAdd(ctx, fmt.Sprintf("Key %d", n))
		require.Equal(t, false, card["isDefault"], "key %d took the default from the first", n)
	}
	_, err := d.add(ctx, "anthropic", "Key 11", deskAnthropicKey)
	require.Error(t, err)
	require.Contains(t, err.Error(), "10")

	// The limit is per vendor, and the other vendor's first key is its default.
	other, err := d.add(ctx, "openai", "OpenAI 1", deskOpenAIKey)
	require.NoError(t, err)
	require.Equal(t, true, other["isDefault"])

	// Removing one makes room.
	_, err = d.in.handleRemove(ctx, map[string]any{"keyId": first["id"]}, 0)
	require.NoError(t, err)
	_, err = d.add(ctx, "anthropic", "Key 11", deskAnthropicKey)
	require.NoError(t, err)
}

// K7: a name is unique among a person's active keys, whatever its case, and a
// removed key's name is free again. Two people may use the same name.
func TestALabelIsUniqueAmongActiveKeys(t *testing.T) {
	d := newDesk(t, "labels")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	ctx := session(dana, auth.RoleDeveloper)

	first := d.mustAdd(ctx, "Work")
	for _, label := range []string{"Work", "work", "  WORK "} {
		_, err := d.add(ctx, "openai", label, deskOpenAIKey)
		require.Error(t, err, "%q was accepted beside Work", label)
		require.Contains(t, err.Error(), "already")
	}
	d.mustAdd(session(omar, auth.RoleOwner), "Work")

	second := d.mustAdd(ctx, "Home")
	_, err := d.in.handleUpdate(ctx, map[string]any{"keyId": second["id"], "label": "work"}, 0)
	require.Error(t, err, "a rename took another key's name")
	_, err = d.in.handleUpdate(ctx, map[string]any{"keyId": second["id"], "label": "home"}, 0)
	require.NoError(t, err, "a key could not be renamed to its own name in another case")

	_, err = d.in.handleRemove(ctx, map[string]any{"keyId": first["id"]}, 0)
	require.NoError(t, err)
	d.mustAdd(ctx, "Work")
}

// K7: making one key the default takes it from the other, and the default
// cannot be switched off while another key could hold it.
func TestMovingTheDefault(t *testing.T) {
	d := newDesk(t, "default")
	dev := d.member("dana", auth.RoleDeveloper)
	ctx := session(dev, auth.RoleDeveloper)
	first := d.mustAdd(ctx, "First")
	d.clock = d.clock.Add(time.Second)
	second := d.mustAdd(ctx, "Second")
	other, err := d.add(ctx, "openai", "Other vendor", deskOpenAIKey)
	require.NoError(t, err)

	nodes, err := d.in.handleUpdate(ctx, map[string]any{"keyId": second["id"], "isDefault": true}, 0)
	require.NoError(t, err)
	require.Equal(t, true, cardOf(t, nodes)["isDefault"])
	require.Equal(t, false, d.latest(keyConcept, first["id"].(string))["isDefault"], "two keys are the default at one vendor")
	require.Equal(t, true, d.latest(keyConcept, other["id"].(string))["isDefault"], "moving one vendor's default moved the other's")
	require.Equal(t, []string{second["id"].(string)}, d.keysFor(d.resolver, dev, true).usable, "the resolver does not pay with the new default")

	_, err = d.in.handleUpdate(ctx, map[string]any{"keyId": second["id"], "isDefault": false}, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "default")
	require.Equal(t, true, d.latest(keyConcept, second["id"].(string))["isDefault"])

	// Auto is a switch of its own.
	nodes, err = d.in.handleUpdate(ctx, map[string]any{"keyId": first["id"], "autoUse": true}, 0)
	require.NoError(t, err)
	require.Equal(t, true, cardOf(t, nodes)["autoUse"])
	require.Equal(t, false, cardOf(t, nodes)["isDefault"], "switching Auto on changed which key is the default")
}

// K16: removal is one-way and the row stays; when the default goes, the
// earliest of the person's other keys at that vendor takes it.
func TestRemoveIsOneWayAndPromotesAnotherDefault(t *testing.T) {
	d := newDesk(t, "remove")
	dev := d.member("dana", auth.RoleDeveloper)
	ctx := session(dev, auth.RoleDeveloper)
	first := d.mustAdd(ctx, "First")
	d.clock = d.clock.Add(time.Second)
	second := d.mustAdd(ctx, "Second")
	d.clock = d.clock.Add(time.Second)
	third := d.mustAdd(ctx, "Third")

	nodes, err := d.in.handleRemove(ctx, map[string]any{"keyId": first["id"]}, 0)
	require.NoError(t, err)
	removed := cardOf(t, nodes)
	require.Equal(t, "removed", removed["status"])
	row := d.latest(keyConcept, first["id"].(string))
	require.Equal(t, "removed", row["status"])
	require.Equal(t, dev, memql.BareShortId(fmt.Sprint(row["removedBy"])))
	require.NotEmpty(t, row["encryptedValue"], "the record of a removed key is gone")
	require.Equal(t, true, d.latest(keyConcept, second["id"].(string))["isDefault"], "the earliest remaining key did not take the default")
	require.Equal(t, false, d.latest(keyConcept, third["id"].(string))["isDefault"])

	// Nothing brings it back.
	for name, act := range map[string]func() error{
		"removing it again": func() error {
			_, err := d.in.handleRemove(ctx, map[string]any{"keyId": first["id"]}, 0)
			return err
		},
		"renaming it": func() error {
			_, err := d.in.handleUpdate(ctx, map[string]any{"keyId": first["id"], "label": "Back"}, 0)
			return err
		},
		"making it the default": func() error {
			_, err := d.in.handleUpdate(ctx, map[string]any{"keyId": first["id"], "isDefault": true}, 0)
			return err
		},
		"checking it": func() error {
			_, err := d.in.handleCheck(ctx, map[string]any{"keyId": first["id"]}, 0)
			return err
		},
	} {
		require.Error(t, act(), "%s was accepted for a removed key", name)
	}
	require.Equal(t, "removed", d.latest(keyConcept, first["id"].(string))["status"])
	mine, _, _ := d.list(ctx, false)
	require.ElementsMatch(t, []string{second["id"].(string), third["id"].(string)}, ids(mine))
}

// K5: a cluster owner can see, check and remove any key on their cluster, and
// can never rename it, make it the default or switch Auto on.
func TestAClusterOwnerMayCheckAndRemoveButNotChange(t *testing.T) {
	d := newDesk(t, "operator")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	key := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")
	operator := session(omar, auth.RoleOwner)

	for name, args := range map[string]map[string]any{
		"rename":  {"keyId": key["id"], "label": "Mine now"},
		"default": {"keyId": key["id"], "isDefault": false},
		"auto":    {"keyId": key["id"], "autoUse": true},
	} {
		_, err := d.in.handleUpdate(operator, args, 0)
		require.Error(t, err, "a cluster owner could %s somebody else's key", name)
	}
	row := d.latest(keyConcept, key["id"].(string))
	require.Equal(t, "Dana's", row["label"])
	require.Equal(t, false, row["autoUse"])

	d.clock = d.clock.Add(time.Minute)
	d.answers(memql.VendorKeyProbeRefused, "revoked at the vendor")
	nodes, err := d.in.handleCheck(operator, map[string]any{"keyId": key["id"]}, 0)
	require.NoError(t, err)
	checked := cardOf(t, nodes)
	require.Equal(t, "refused", checked["state"])
	require.Equal(t, "other", checked["relation"])
	require.NotContains(t, fmt.Sprint(checked), deskAnthropicKey)

	_, err = d.in.handleRemove(operator, map[string]any{"keyId": key["id"]}, 0)
	require.NoError(t, err)
	row = d.latest(keyConcept, key["id"].(string))
	require.Equal(t, "removed", row["status"])
	require.Equal(t, omar, memql.BareShortId(fmt.Sprint(row["removedBy"])), "the record does not say who removed it")
	require.Equal(t, dana, memql.BareShortId(fmt.Sprint(row["ownerUserId"])), "removing a key made the cluster owner its owner")
}

// K5: what each person sees. The owner sees their keys in full; a person a key
// is lent to sees a short card; a cluster owner may ask for every key.
func TestListShowsMineLentAndForAnOwnerAll(t *testing.T) {
	d := newDesk(t, "list")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	wes := d.member("wes", auth.RoleWriter)
	danas := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")
	omars := d.mustAdd(session(omar, auth.RoleOwner), "Omar's")
	d.lend(dana, danas["id"].(string), people(wes, omar))

	mine, lent, every := d.list(session(dana, auth.RoleDeveloper), true)
	require.Equal(t, []string{danas["id"].(string)}, ids(mine))
	require.Empty(t, lent)
	require.Empty(t, every, "a developer asked for every key and got it")
	require.EqualValues(t, 2, mine[0]["lentTo"])
	require.Equal(t, "...0001", mine[0]["fingerprint"])
	// Her own card says who she lent it to: it is her setting to edit.
	lending, _ := mine[0]["sharing"].(map[string]any)
	require.Equal(t, "people", lending["mode"])
	require.ElementsMatch(t, []string{wes, omar}, bareList(lending["userIds"]))
	require.Equal(t, false, lending["paused"])

	// Wes holds no part and no key. He sees the card of the key lent to him.
	mine, lent, every = d.list(session(wes, auth.RoleWriter), true)
	require.Empty(t, mine)
	require.Empty(t, every)
	require.Equal(t, []string{danas["id"].(string)}, ids(lent))
	card := lent[0]
	require.Equal(t, "lent", card["relation"])
	require.Equal(t, "Dana's", card["label"])
	require.Equal(t, "Dana", card["ownerName"])
	require.Equal(t, "good", card["state"])
	for _, private := range []string{"fingerprint", "isDefault", "autoUse", "lentTo", "sharing", "checkedBy", "detail", "addedAt"} {
		require.NotContains(t, card, private, "a borrower's card carries %s", private)
	}

	// A cluster owner sees their own, what is lent to them, and on request all.
	mine, lent, every = d.list(session(omar, auth.RoleOwner), false)
	require.Equal(t, []string{omars["id"].(string)}, ids(mine))
	require.Equal(t, []string{danas["id"].(string)}, ids(lent))
	require.Empty(t, every, "every key was listed without being asked for")
	_, _, every = d.list(session(omar, auth.RoleOwner), true)
	require.Subset(t, ids(every), []string{danas["id"].(string), omars["id"].(string)})
	for _, c := range every {
		if c["id"] == danas["id"] {
			require.Equal(t, "lent", c["relation"])
			require.Equal(t, "...0001", c["fingerprint"], "a cluster owner reads a key's facts")
			// How many it is lent to is a fact. Who they are is its owner's.
			require.EqualValues(t, 2, c["lentTo"])
			require.NotContains(t, c, "sharing", "a cluster owner's card of somebody's key names who it is lent to")
		}
	}

	raw, err := json.Marshal([]any{mine, lent, every})
	require.NoError(t, err)
	require.NotContains(t, string(raw), deskAnthropicKey)
	require.NotContains(t, string(raw), "encryptedValue")

	// Nobody signed in sees nothing, and is told why.
	_, err = d.in.handleForActor(context.Background(), map[string]any{}, 0)
	require.Error(t, err)
}

// S9: somebody with no tie to a key learns nothing about it, not even that it
// exists: every act answers as it does for an id that was never minted.
func TestAStrangerLearnsNothingAboutAKey(t *testing.T) {
	d := newDesk(t, "stranger")
	dana := d.member("dana", auth.RoleDeveloper)
	sam := d.member("sam", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	key := d.mustAdd(session(dana, auth.RoleDeveloper), "Dana's")
	d.lend(dana, key["id"].(string), people(wes))

	acts := func(ctx context.Context, keyId any) []string {
		var said []string
		for _, act := range []func() error{
			func() error {
				_, err := d.in.handleUpdate(ctx, map[string]any{"keyId": keyId, "label": "Taken"}, 0)
				return err
			},
			func() error { _, err := d.in.handleCheck(ctx, map[string]any{"keyId": keyId}, 0); return err },
			func() error { _, err := d.in.handleRemove(ctx, map[string]any{"keyId": keyId}, 0); return err },
		} {
			err := act()
			require.Error(t, err)
			said = append(said, err.Error())
		}
		return said
	}
	stranger := session(sam, auth.RoleDeveloper)
	require.Equal(t, acts(stranger, "vk-key-never-minted"), acts(stranger, key["id"]),
		"a stranger can tell a real key from an id that was never minted")
	// A borrower may spend a key and may not change, check or remove it.
	require.Equal(t, acts(stranger, "vk-key-never-minted"), acts(session(wes, auth.RoleWriter), key["id"]))

	row := d.latest(keyConcept, key["id"].(string))
	require.Equal(t, "active", row["status"])
	require.Equal(t, "Dana's", row["label"])
	mine, lent, _ := d.list(stranger, false)
	require.Empty(t, mine)
	require.Empty(t, lent)
}

// K11: Check now records what the vendor said, as a real call would.
func TestCheckRecordsWhatTheVendorSaid(t *testing.T) {
	d := newDesk(t, "check")
	dev := d.member("dana", auth.RoleDeveloper)
	ctx := session(dev, auth.RoleDeveloper)
	key := d.mustAdd(ctx, "Personal")
	keyId := key["id"].(string)
	d.asked = nil

	d.clock = d.clock.Add(2 * time.Hour)
	nodes, err := d.in.handleCheck(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"anthropic:" + deskAnthropicKey}, d.asked, "the check did not unseal and send the stored key")
	require.Equal(t, "good", cardOf(t, nodes)["state"])
	require.Equal(t, stamp(d.clock), d.state(keyId)["checkedAt"])

	d.clock = d.clock.Add(time.Minute)
	d.answers(memql.VendorKeyProbeCredit, "credit balance is too low")
	nodes, err = d.in.handleCheck(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	require.Equal(t, "credit", cardOf(t, nodes)["state"])
	require.Equal(t, "credit balance is too low", cardOf(t, nodes)["detail"])
	require.Empty(t, d.keysFor(d.resolver, dev, true).usable, "a key the check found out of credit still pays")

	// Nothing learned changes nothing.
	d.clock = d.clock.Add(time.Minute)
	d.answers(memql.VendorKeyProbeUnreachable, "Anthropic could not be reached to check the key. Try again in a moment.")
	nodes, err = d.in.handleCheck(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	card := cardOf(t, nodes)
	require.Equal(t, "credit", card["state"], "an unreachable vendor changed what is known about the key")
	require.Contains(t, card["note"], "could not be reached")

	// A listing is answered whether or not the account has credit. So a check
	// that succeeds on a key held for credit lifts the hold, for the next real
	// call to find out, and does not claim the key works.
	d.clock = d.clock.Add(time.Minute)
	d.answers(memql.VendorKeyProbeGood, "")
	nodes, err = d.in.handleCheck(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	card = cardOf(t, nodes)
	require.Equal(t, "credit", card["state"], "a listing was taken as proof that the account has credit again")
	require.Contains(t, card["note"], "credit")
	require.Len(t, d.keysFor(d.resolver, dev, true).usable, 1, "the hold was not lifted, so no call can find out")
	require.Equal(t, []string{auditAdded, auditWentBad}, d.auditActions(keyId))

	// The call that finds out is served, and only then has the key recovered.
	d.clock = d.clock.Add(time.Minute)
	require.NoError(t, d.store.observe(context.Background(), keyId, memql.VendorKeyGood, d.clock, "", false))
	require.Equal(t, "good", d.state(keyId)["state"])
	require.Equal(t, []string{auditAdded, auditWentBad, auditRecovered}, d.auditActions(keyId))
}

// S7: every act on a key writes an audit event naming who did it, and none
// carries the key's value.
func TestEveryActIsAudited(t *testing.T) {
	d := newDesk(t, "audited")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	ctx := session(dana, auth.RoleDeveloper)
	first := d.mustAdd(ctx, "First")
	keyId := first["id"].(string)
	d.clock = d.clock.Add(time.Second)
	second := d.mustAdd(ctx, "Second")

	for _, args := range []map[string]any{
		{"keyId": keyId, "label": "Renamed"},
		{"keyId": keyId, "autoUse": true},
		{"keyId": second["id"], "isDefault": true},
	} {
		_, err := d.in.handleUpdate(ctx, args, 0)
		require.NoError(t, err)
	}
	_, err := d.in.handleRemove(session(omar, auth.RoleOwner), map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)

	require.Equal(t, []string{auditAdded, auditRenamed, auditAutoChanged, auditDefaultChanged, auditRemoved}, d.auditActions(keyId),
		"losing the default is an event of the key that lost it")
	require.Equal(t, []string{auditAdded, auditDefaultChanged}, d.auditActions(second["id"].(string)))

	last := d.lastAudit(keyId)
	require.Contains(t, last, omar, "the removal's event does not name who removed the key")
	require.Contains(t, last, dana, "the removal's event does not name whose key it was")
	for _, key := range []string{keyId, second["id"].(string)} {
		var nodes []memorynodes.MemoryNode
		require.NoError(t, d.db.NewSelect().Model(&nodes).
			Where("concept = ?", "v1:identity:auditEvent").Where("payload->>'targetId' LIKE ?", "%"+key).Scan(context.Background()))
		for _, node := range nodes {
			require.NotContains(t, string(node.Payload), deskAnthropicKey)
			require.NotContains(t, string(node.Payload), "Renamed", "an audit event carries a key's name")
		}
	}
}

// ---------------------------------------------------------------------------
// the real dispatch
// ---------------------------------------------------------------------------

// The engine keeps one integration per name for the life of the process, and
// each test builds its own. So what is registered is a forwarder to whichever
// desk currently holds the dispatch.
var (
	dispatchMu   sync.Mutex
	dispatchOnce sync.Once
	dispatched   *Integration
)

type dispatchedIntegration struct{}

func (*dispatchedIntegration) IntegrationName() string { return "vendorkeys" }

func (*dispatchedIntegration) Capabilities() []memql.IntegrationCapability {
	declared := (&Integration{}).Capabilities()
	out := make([]memql.IntegrationCapability, 0, len(declared))
	for _, capability := range declared {
		name := capability.Name
		capability.Handler = func(ctx context.Context, args map[string]any, limit int) ([]memorynodes.MemoryNode, error) {
			for _, live := range dispatched.Capabilities() {
				if live.Name == name {
					return live.Handler(ctx, args, limit)
				}
			}
			return nil, fmt.Errorf("no capability %q", name)
		}
		out = append(out, capability)
	}
	return out
}

package emailrules

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// fakeEngine records every rendered call AND the origin/actor it ran under.
// Recording the origin is the point: an unstamped @serverOnly write is refused
// with only a WARN, so a test that checked the call text alone would pass
// against code whose writes never land.
type fakeEngine struct {
	calls    []string
	internal []bool
	rule     map[string]any
	bundle   map[string]any
	template map[string]any
	audience map[string]any
	identity map[string]any
	// noTemplate makes templateById answer empty, which is what an
	// unreadable template looks like from the author's envelope.
	noTemplate bool
	activate   func(owner, bundleID string) error
	retire     func(owner, bundleID string) error
}

func (e *fakeEngine) Execute(ctx context.Context, q string) (any, error) {
	e.calls = append(e.calls, q)
	e.internal = append(e.internal, auth.OriginFromContext(ctx).IsInternal())
	switch {
	case strings.HasPrefix(q, "query emailRuleById"):
		if e.rule == nil {
			return rowsEnvelope(nil), nil
		}
		return rowsEnvelope([]map[string]any{e.rule}), nil
	case strings.HasPrefix(q, "query authoringBundleById"):
		if e.bundle == nil {
			return rowsEnvelope(nil), nil
		}
		return rowsEnvelope([]map[string]any{e.bundle}), nil
	case strings.HasPrefix(q, "query templateById"):
		if e.noTemplate {
			return rowsEnvelope(nil), nil
		}
		t := e.template
		if t == nil {
			t = map[string]any{"id": "v1:campaigns:template:t1", "subject": "s", "textBody": "b", "status": "ready"}
		}
		return rowsEnvelope([]map[string]any{t}), nil
	case strings.HasPrefix(q, "query audienceById"):
		a := e.audience
		if a == nil {
			a = map[string]any{"id": "v1:campaigns:audience:a1", "status": "active"}
		}
		return rowsEnvelope([]map[string]any{a}), nil
	case strings.HasPrefix(q, "query senderIdentityById"):
		if e.identity == nil {
			return rowsEnvelope(nil), nil
		}
		return rowsEnvelope([]map[string]any{e.identity}), nil
	}
	return rowsEnvelope(nil), nil
}

func (e *fakeEngine) ActivateApprovedBundle(_ context.Context, owner, bundleID string, _ memql.AuthoredRuntimeDeps) (memql.ActivationResult, error) {
	if e.activate != nil {
		if err := e.activate(owner, bundleID); err != nil {
			return memql.ActivationResult{}, err
		}
	}
	return memql.ActivationResult{}, nil
}

func (e *fakeEngine) RetireActiveBundle(_ context.Context, owner, bundleID string, _ memql.AuthoredRuntimeDeps) error {
	if e.retire != nil {
		return e.retire(owner, bundleID)
	}
	return nil
}

func (e *fakeEngine) find(prefix string) (string, bool, bool) {
	for i, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			return c, e.internal[i], true
		}
	}
	return "", false, false
}

func rowsEnvelope(rows []map[string]any) any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}
}

func ruleRow() map[string]any {
	return map[string]any{
		"id":             "v1:campaigns:emailRule:ab12cd34",
		"ownerUserId":    "v1:identity:user:owner1",
		"name":           "Tell the owner about new admins",
		"triggerConcept": "v1:identity:user",
		"eventKind":      "created",
		"templateId":     "v1:campaigns:template:t1",
		"recipientMode":  ModeClusterRoles,
		"status":         "draft",
	}
}

func ownerCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:owner1", Role: role,
	})
}

func deps() func() memql.AuthoredRuntimeDeps {
	return func() memql.AuthoredRuntimeDeps { return memql.AuthoredRuntimeDeps{} }
}

// The whole point of this package: the rule is live when Activate returns, not
// at next boot. ActivateApprovedBundle had NO production caller before it.
func TestActivateReachesTheRealActivationEntryPoint(t *testing.T) {
	called := ""
	e := &fakeEngine{rule: ruleRow(), activate: func(_, bundleID string) error { called = bundleID; return nil }}

	res, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if called == "" {
		t.Fatal("ActivateApprovedBundle was never called; the rule would take effect only at next boot, which is the gap this closes")
	}
	if called != res.BundleID {
		t.Errorf("activated %q but reported %q", called, res.BundleID)
	}
	if res.Status != "active" {
		t.Errorf("status = %q, want active", res.Status)
	}
}

// Every rendered call must PARSE. The suites in this tree that drive a fake
// engine recording call strings have hidden render bugs before; parsing each
// one with the real parser is the reachable positive.
func TestEveryRenderedCallParses(t *testing.T) {
	e := &fakeEngine{rule: ruleRow()}
	if _, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(e.calls) < 5 {
		t.Fatalf("only %d calls recorded; the activation path did not run", len(e.calls))
	}
	for _, q := range e.calls {
		tokens, lerr := langparser.NewLexer(q).Tokenize()
		if lerr != nil {
			t.Errorf("rendered call does not lex: %s\n  -> %v", q, lerr)
			continue
		}
		if _, err := langparser.NewParser(tokens).Parse(); err != nil {
			t.Errorf("rendered call does not parse: %s\n  -> %v", q, err)
		}
	}
}

// The three @serverOnly writes are refused with only a WARN when unstamped, so
// the stamping is load-bearing rather than hygiene.
func TestServerOnlyWritesStampInternalOrigin(t *testing.T) {
	e := &fakeEngine{rule: ruleRow()}
	if _, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, prefix := range []string{
		"mutation recordEmailRuleGeneration",
		"mutation recordBundleValidation",
		"mutation recordBundleDryRun",
	} {
		call, internal, found := e.find(prefix)
		if !found {
			t.Errorf("%s was never called", prefix)
			continue
		}
		if !internal {
			t.Errorf("%s ran WITHOUT internal origin; the engine refuses it with only a WARN, so the write would silently not land:\n  %s", prefix, call)
		}
	}
	// And the ordinary reads must NOT be stamped -- an over-broad stamp would
	// let a later frame inherit the mark.
	if call, internal, found := e.find("query emailRuleById"); found && internal {
		t.Errorf("the rule read was stamped internal origin; the read is the AUTHORIZATION and must run as the caller:\n  %s", call)
	}
}

// Arming a rule authors an automation that mails people. A writer must not be
// able to do it, whatever they own.
func TestArmingIsOwnerOrDeveloperOnly(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader, auth.RoleAdmin} {
		e := &fakeEngine{rule: ruleRow()}
		_, err := NewActivator(e, deps()).Activate(ownerCtx(role), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
		if err == nil {
			t.Errorf("role %q armed a rule; arming authors an automation that sends mail on this cluster's behalf", role)
		}
	}
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		e := &fakeEngine{rule: ruleRow()}
		if _, err := NewActivator(e, deps()).Activate(ownerCtx(role), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34"); err != nil {
			t.Errorf("role %q was refused: %v", role, err)
		}
	}
}

// A failure has to land where the operator is looking, not only in the error
// the caller sees -- a rule stuck at draft with the reason in one replica's log
// is the silence this feature exists to remove.
func TestAFailedActivationStampsTheRuleRow(t *testing.T) {
	bad := ruleRow()
	bad["triggerConcept"] = "not-a-concept-id"
	e := &fakeEngine{rule: bad}

	_, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
	if err == nil {
		t.Fatal("a rule naming a bare concept name was armed")
	}
	call, internal, found := e.find("mutation recordEmailRuleGeneration")
	if !found {
		t.Fatal("the refusal was never recorded on the rule row")
	}
	if !internal {
		t.Error("the refusal was recorded without internal origin, so it would not land")
	}
	if !strings.Contains(call, `status: "failed"`) {
		t.Errorf("the rule was not moved to failed:\n  %s", call)
	}
	if !strings.Contains(call, "lastError") {
		t.Errorf("the engine's own sentence was not recorded:\n  %s", call)
	}
}

// A rule the caller does not own must not be armed: the generated automation
// runs under its AUTHOR's envelope, so arming somebody else's is running their
// reads under their identity on your say-so.
func TestArmingSomebodyElsesRuleIsRefused(t *testing.T) {
	e := &fakeEngine{rule: ruleRow()}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:someone-else", Role: auth.RoleOwner,
	})
	if _, err := NewActivator(e, deps()).Activate(ctx, "v1:identity:user:someone-else", "v1:campaigns:emailRule:ab12cd34"); err == nil {
		t.Fatal("a rule owned by somebody else was armed")
	}
}

// Retiring a rule that was never armed is a reasonable ask and a no-op is the
// right answer to it -- not an error.
func TestRetiringADraftIsANoOp(t *testing.T) {
	e := &fakeEngine{rule: ruleRow()}
	res, err := NewActivator(e, deps()).Retire(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if res.Status != "draft" {
		t.Errorf("status = %q, want draft", res.Status)
	}
}

// Without the App's handoff the two activation capabilities must refuse with a
// sentence, not nil-panic and not silently do nothing.
func TestUnboundNodeRefusesRatherThanPanics(t *testing.T) {
	a := NewActivator(&fakeEngine{rule: ruleRow()}, nil)
	if _, err := a.Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "r"); err == nil {
		t.Error("an unwired node armed a rule")
	}
	if _, err := a.Retire(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "r"); err == nil {
		t.Error("an unwired node retired a rule")
	}
}

// A rule whose world the AUTHOR cannot read fires forever and mails nobody --
// correctly, silently, on every matching row. The check belongs at arm time,
// where the operator is still looking at the form.
func TestArmingVerifiesTheRulesWorldIsReadable(t *testing.T) {
	t.Run("an unreadable template refuses", func(t *testing.T) {
		e := &fakeEngine{rule: ruleRow(), noTemplate: true}
		_, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
		if err == nil {
			t.Fatal("a rule naming a template its author cannot read was armed")
		}
		if !strings.Contains(err.Error(), "mail nobody") {
			t.Errorf("the refusal does not say what would happen: %v", err)
		}
		if call, _, found := e.find("mutation recordEmailRuleGeneration"); !found || !strings.Contains(call, `status: "failed"`) {
			t.Error("the refusal was not stamped on the rule row, so the operator would never see it")
		}
	})

	t.Run("a disabled sending identity refuses rather than falling back", func(t *testing.T) {
		r := ruleRow()
		r["senderIdentityId"] = "v1:campaigns:senderIdentity:s1"
		e := &fakeEngine{
			rule:     r,
			identity: map[string]any{"id": "v1:campaigns:senderIdentity:s1", "status": "disabled"},
		}
		_, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
		if err == nil {
			t.Fatal("a rule naming a disabled identity was armed; it would silently fall back to the default mailbox")
		}
	})

	t.Run("the operational lane does not require an audience", func(t *testing.T) {
		// cluster_roles never reads an audience, so a rule that names none
		// must still arm -- the check has to follow the lane, not the schema.
		e := &fakeEngine{rule: ruleRow()}
		if _, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34"); err != nil {
			t.Fatalf("an operational rule with no audience was refused: %v", err)
		}
	})
}

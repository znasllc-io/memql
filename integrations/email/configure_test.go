package email

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

type recordingWriter struct{ calls []string }

func (w *recordingWriter) Execute(_ context.Context, q string) (any, error) {
	w.calls = append(w.calls, q)
	return nil, nil
}

func (w *recordingWriter) find(prefix string) (string, bool) {
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			return c, true
		}
	}
	return "", false
}

func configureCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:op", Role: role,
	})
}

func newConfigurableIntegration(w *recordingWriter) *Integration {
	lazy := NewLazySender(NewLogSender(slog.Default()), nil, nil, slog.Default())
	return NewIntegration(lazy, slog.Default()).WithConfigWriter(w)
}

// Writing is stricter than reading, and deliberately so: the read carries no
// secret and takes nothing from anybody, while the write changes where this
// cluster's mail leaves from.
func TestConfigureIsOwnerOrDeveloperOnly(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		w := &recordingWriter{}
		i := newConfigurableIntegration(w)
		if _, err := i.handleConfigure(configureCtx(role), map[string]any{
			"slot": "senderAddress", "value": "news@example.test",
		}, 0); err != nil {
			t.Errorf("role %q was refused: %v", role, err)
		}
	}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleWriter, auth.RoleReader} {
		w := &recordingWriter{}
		i := newConfigurableIntegration(w)
		if _, err := i.handleConfigure(configureCtx(role), map[string]any{
			"slot": "senderAddress", "value": "news@example.test",
		}, 0); err == nil {
			t.Errorf("role %q changed integration configuration", role)
		}
		if len(w.calls) != 0 {
			t.Errorf("role %q was refused but a write still went out: %v", role, w.calls)
		}
	}
	// No caller at all fails closed.
	w := &recordingWriter{}
	if _, err := newConfigurableIntegration(w).handleConfigure(context.Background(), map[string]any{
		"slot": "senderAddress", "value": "x@y.test",
	}, 0); err == nil {
		t.Error("an unauthenticated call changed integration configuration")
	}
}

// THE ROW NAME IS NOT A PARAMETER. A caller that could supply it could write
// MEMQL_EMAIL_SENDR, get a green save, and never be mailed anything again.
func TestConfigureRefusesAnUnknownSlotAndSaysWhatExists(t *testing.T) {
	w := &recordingWriter{}
	_, err := newConfigurableIntegration(w).handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
		"slot": "MEMQL_EMAIL_SENDR", "value": "x@y.test",
	}, 0)
	if err == nil {
		t.Fatal("an unknown slot was accepted")
	}
	if !strings.Contains(err.Error(), "senderAddress") {
		t.Errorf("the refusal does not name the settings that DO exist, so a caller cannot fix it: %v", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("an unknown slot still produced a write: %v", w.calls)
	}
}

// The plaintext crosses the wire once and is never stored. A row carrying it
// would be a credential in the graph, readable by anything that can read a
// globalSecret row's payload.
func TestConfigureSealsACredential(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	const plaintext = "s3cret-value-nobody-should-see"
	w := &recordingWriter{}
	if _, err := newConfigurableIntegration(w).handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
		"slot": "clientSecret", "value": plaintext,
	}, 0); err != nil {
		t.Fatalf("configure: %v", err)
	}
	call, ok := w.find("mutation setGlobalSecret")
	if !ok {
		t.Fatalf("a credential was not written through setGlobalSecret: %v", w.calls)
	}
	if strings.Contains(call, plaintext) {
		t.Errorf("THE PLAINTEXT IS IN THE ROW. The write must carry the sealed value only:\n  %s", call)
	}
	if !strings.Contains(call, "encryptedValue") || !strings.Contains(call, "fingerprint") {
		t.Errorf("the sealed write is missing its ciphertext or fingerprint:\n  %s", call)
	}
	// And a non-secret slot goes to the plaintext tier, not the sealed one.
	w2 := &recordingWriter{}
	if _, err := newConfigurableIntegration(w2).handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
		"slot": "senderAddress", "value": "news@example.test",
	}, 0); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, ok := w2.find("mutation setGlobalVariable"); !ok {
		t.Errorf("a non-secret slot did not go to globalVariable: %v", w2.calls)
	}
}

// The row id is already derived three ways in this tree, and the resolver looks
// rows up by NAME -- so a mismatched id does not fail, it writes a SECOND row
// carrying the same name and makes "which value is live" a question about query
// order. This pins the seeder's derivation, which is the row an installed
// cluster already has.
func TestConfigureWritesAtTheSeedersRowId(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	for _, tc := range []struct{ slot, prefix, wantId string }{
		{"senderAddress", "mutation setGlobalVariable", "var-global-memql-email-sender"},
		{"clientSecret", "mutation setGlobalSecret", "secret-global-memql-email-azure-client-secret"},
	} {
		w := &recordingWriter{}
		if _, err := newConfigurableIntegration(w).handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
			"slot": tc.slot, "value": "value-for-" + tc.slot,
		}, 0); err != nil {
			t.Fatalf("configure %s: %v", tc.slot, err)
		}
		call, ok := w.find(tc.prefix)
		if !ok {
			t.Fatalf("%s produced no %s call: %v", tc.slot, tc.prefix, w.calls)
		}
		if !strings.Contains(call, `id: "`+tc.wantId+`"`) {
			t.Errorf("%s wrote at the wrong row id -- the seeded row would survive beside it and "+
				"which one answers becomes a question about query order.\n  want id %q\n  got   %s",
				tc.slot, tc.wantId, call)
		}
	}
}

// A rendered call that does not parse drops the write it was making, and the
// only symptom is a credential that never took.
func TestConfigureRenderedCallsParse(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	w := &recordingWriter{}
	i := newConfigurableIntegration(w)
	for _, tc := range []struct{ slot, value string }{
		{"senderAddress", `news@example.test`},
		{"fromName", `Acme, "Inc." \ News`},
		{"clientSecret", "s3cret"},
	} {
		if _, err := i.handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
			"slot": tc.slot, "value": tc.value,
		}, 0); err != nil {
			t.Fatalf("configure %s: %v", tc.slot, err)
		}
	}
	if len(w.calls) != 3 {
		t.Fatalf("expected three writes, got %d", len(w.calls))
	}
	for _, q := range w.calls {
		tokens, lerr := langparser.NewLexer(q).Tokenize()
		if lerr != nil {
			t.Errorf("rendered call does not lex: %v\n  %s", lerr, q)
			continue
		}
		if _, err := langparser.NewParser(tokens).Parse(); err != nil {
			t.Errorf("rendered call does not parse: %v\n  %s", err, q)
		}
	}
}

// An empty value would write a row that reads as configured and resolves to
// nothing -- the exact half-working state this surface exists to remove.
func TestConfigureRefusesAnEmptyValue(t *testing.T) {
	w := &recordingWriter{}
	if _, err := newConfigurableIntegration(w).handleConfigure(configureCtx(auth.RoleOwner), map[string]any{
		"slot": "senderAddress", "value": "   ",
	}, 0); err == nil {
		t.Error("an empty value was written")
	}
	if len(w.calls) != 0 {
		t.Errorf("an empty value still produced a write: %v", w.calls)
	}
}

// Without invalidation a correct save on a node that has already sent leaves
// every subsequent message going wherever the first one went.
//
// Asserted by COUNTING RESOLUTIONS rather than by comparing senders: with
// nothing configured, resolution correctly returns the same LogSender both
// times, so an identity comparison would pass against a wrapper that never
// re-read anything. What has to be true is that the TIERS were consulted
// again, and the resolver is the only thing that can say so.
func TestInvalidateMakesTheNextSendReResolve(t *testing.T) {
	reads := 0
	resolveVar := func(context.Context, string) (string, error) {
		reads++
		return "", nil
	}
	lazy := NewLazySender(NewLogSender(slog.Default()), resolveVar, nil, slog.Default())

	lazy.Resolve(context.Background())
	afterFirst := reads
	if afterFirst == 0 {
		t.Fatal("the first resolution consulted no tier at all")
	}
	lazy.Resolve(context.Background())
	if reads != afterFirst {
		t.Fatalf("Resolve is not caching: %d tier reads became %d without an invalidation", afterFirst, reads)
	}

	if !lazy.Invalidate() {
		t.Error("Invalidate reported nothing to discard after a resolution had happened")
	}
	lazy.Resolve(context.Background())
	if reads == afterFirst {
		t.Error("the tiers were NOT consulted again after Invalidate; a credential saved from Settings would " +
			"never take effect on this node, while the console said it was configured")
	}

	// A wrapper that never resolved has nothing to discard, and says so --
	// which is what lets a surface tell the truth about whether a save will
	// change anything here.
	fresh := NewLazySender(NewLogSender(slog.Default()), nil, nil, slog.Default())
	if fresh.Invalidate() {
		t.Error("Invalidate claimed it discarded a resolution that had never happened")
	}
}

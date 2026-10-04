package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/secret"
)

// A VARIABLE OR SECRET NAME IS A LITERAL, NEVER QUERY SYNTAX (memql#5625).
//
// readNamedRowFields built its lookup as `payload.name=="<name>"` by pasting
// the name between two literal quotes. A name carrying a quote closed the
// literal early and the rest of it was read as query: a name ending in
// `" || payload.name=="X` resolved to the row named X -- a DIFFERENT row --
// and a lone quote or backslash made the lookup unparseable, so a row that
// exists could not be resolved at all. Every ResolveVariable / ResolveSecret
// caller reaches this, including callers resolving a name they read off a row
// somebody else can write (a store's token reference, a provider's auth
// field).
//
// The contract this pins: a name resolves to the row of exactly that name, or
// to nothing. The rows are real and the reads go through the real engine,
// because the defect was in the statement the engine was handed -- a fake
// executor would have accepted the broken string and proved nothing.

func TestResolvedNamesMatchTheirExactRowOrNothing(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	suffix := uniqueSuffix("name-quoting")
	ctx := clusterOwnerCtx("v1:identity:user:name-quoting-" + suffix)

	// The row an injected name would reach if the name were read as syntax.
	target := "target-" + suffix
	exact := map[string]string{
		`has"quote-` + suffix:      "value-of-the-quoted-name",
		`back\slash-` + suffix:     "value-of-the-backslashed-name",
		`trailing-` + suffix + `\`: "value-of-the-name-ending-in-a-backslash",
		target:                     "value-of-the-target",
	}
	i := 0
	for name, value := range exact {
		i++
		runMutation(t, ctx, eng, "setGlobalVariable", map[string]any{
			"id":    "name-quoting-var-" + strings.Repeat("x", i) + "-" + suffix,
			"name":  name,
			"value": value,
		})
	}

	t.Run("a name carrying a quote or a backslash resolves to its own row", func(t *testing.T) {
		for name, want := range exact {
			for resolver, resolve := range map[string]func(string) (string, error){
				"ResolveSystemVariable": func(n string) (string, error) { return eng.ResolveSystemVariable(ctx, n) },
				// The partition-first resolver: a lookup that fails to PARSE on
				// the partition concept is an error, not a miss, so it never
				// reaches the global row it should fall back to.
				"ResolveVariable": func(n string) (string, error) { return eng.ResolveVariable(ctx, n) },
			} {
				got, err := resolve(name)
				if err != nil {
					t.Fatalf("%s(%q) failed: %v -- the row exists, so the lookup statement itself is broken", resolver, name, err)
				}
				if got != want {
					t.Fatalf("%s(%q) = %q, want %q", resolver, name, got, want)
				}
			}
		}
	})

	t.Run("a name that spells query syntax resolves to nothing, never to another row", func(t *testing.T) {
		injected := []string{
			`nobody-` + suffix + `" || payload.name=="` + target,
			`nobody-` + suffix + `"||payload.name=="` + target + `"||payload.name=="`,
		}
		for _, name := range injected {
			got, err := eng.ResolveSystemVariable(ctx, name)
			if got == exact[target] {
				t.Fatalf("ResolveSystemVariable(%q) returned the row named %q. The name was read as query "+
					"syntax rather than as a literal, so it resolved a DIFFERENT row", name, target)
			}
			if !isNotFoundVariable(err) {
				t.Fatalf("ResolveSystemVariable(%q) = (%q, %v), want a not-found miss: no row carries that name", name, got, err)
			}
		}
	})
}

// The secret half of the same lookup: ResolveSecret and ResolveSystemSecret
// reach readNamedRowFields the same way, then decrypt. A miss here would read
// the ciphertext of a different secret, which is the worse of the two.
func TestResolvedSecretNamesMatchTheirExactRowOrNothing(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	suffix := uniqueSuffix("secret-name-quoting")
	ctx := clusterOwnerCtx("v1:identity:user:secret-name-quoting-" + suffix)

	target := "SECRET_TARGET_" + suffix
	quoted := `SECRET_"QUOTED"_` + suffix
	for i, row := range []struct{ name, plaintext string }{
		{quoted, "plaintext-of-the-quoted-secret"},
		{target, "plaintext-of-the-target-secret"},
	} {
		ct, fp, err := secret.Encrypt(row.plaintext)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		runMutation(t, ctx, eng, "setGlobalSecret", map[string]any{
			"id":             "name-quoting-secret-" + strings.Repeat("x", i+1) + "-" + suffix,
			"name":           row.name,
			"encryptedValue": ct,
			"fingerprint":    fp,
		})
	}

	got, err := eng.ResolveSystemSecret(ctx, quoted)
	if err != nil || got != "plaintext-of-the-quoted-secret" {
		t.Fatalf("ResolveSystemSecret(%q) = (%q, %v), want the quoted secret's own plaintext", quoted, got, err)
	}

	injected := `NOBODY_` + suffix + `" || payload.name=="` + target
	got, err = eng.ResolveSystemSecret(ctx, injected)
	if got == "plaintext-of-the-target-secret" {
		t.Fatalf("ResolveSystemSecret(%q) decrypted the secret named %q: the name was read as query syntax", injected, target)
	}
	if !isNotFoundVariable(err) {
		t.Fatalf("ResolveSystemSecret(%q) = (%q, %v), want a not-found miss", injected, got, err)
	}
}

// The default injector's only-if-absent check is the same lookup. Its names
// come from the compiled env-var registry, so no caller reaches it with a
// quote today; it is held to the same contract so the two lookups cannot
// drift into rendering their literal two ways.
func TestDefaultInjectorPresenceCheckMatchesTheExactName(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	suffix := uniqueSuffix("injector-quoting")
	ctx := clusterOwnerCtx("v1:identity:user:injector-quoting-" + suffix)
	name := `INJECTOR_"QUOTED"_` + suffix
	runMutation(t, ctx, eng, "setGlobalVariable", map[string]any{
		"id":    "injector-quoting-" + suffix,
		"name":  name,
		"value": "set by an operator",
	})

	store := &engineInjectStore{engine: eng}
	present, err := store.valuePresent(ctx, "v1:platform:globalVariable", name)
	if err != nil || !present {
		t.Fatalf("valuePresent(%q) = (%v, %v), want (true, nil): the row exists under exactly that name", name, present, err)
	}
	present, err = store.valuePresent(ctx, "v1:platform:globalVariable", `NOBODY_`+suffix+`" || payload.name=="`+name)
	if err != nil || present {
		t.Fatalf("valuePresent of an injected name = (%v, %v), want (false, nil): no row carries that name", present, err)
	}
}

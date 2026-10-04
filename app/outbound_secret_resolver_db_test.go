package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/outbound"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
)

// TestOutboundSecretTargetsResolveUnderTheWorkersActor -- memql#5480, on a
// real engine over a real Postgres.
//
// app wires the outbound worker's Secrets to MemQLEngine.ResolveSystemSecret,
// and the worker calls it under outbound.SystemActorContext: a system token,
// no internal origin, no person. That is the half no unit test reaches -- the
// worker's own tests answer from a fake resolver -- so this reads a
// globalSecret sealed under MEMQL_MASTER_KEY through the real resolver as that
// actor. A row tier added to v1:platform:globalSecret that shut the actor out
// would otherwise show up as every secret target failing at run time.
//
// It also pins the two answers the worker fails a row on instead of retrying
// it -- nobody stored the secret, and it does not decrypt -- against the
// errors the real resolver returns rather than the ones a fake was written to
// return.
func TestOutboundSecretTargetsResolveUnderTheWorkersActor(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, strings.Repeat("5a", 32))
	e := workTemplateDBEngine(t)
	suffix := strings.ToUpper(strings.ReplaceAll(id.NewShortId(), "-", ""))
	worker := outbound.SystemActorContext(context.Background())

	url := "https://discord.com/api/webhooks/5480/" + strings.Repeat("t0k", 20)
	ciphertext, fingerprint, err := secret.Encrypt(url)
	if err != nil {
		t.Fatalf("seal the fixture: %v", err)
	}
	name := "DISCORD_5480_" + suffix
	sealOutboundTestSecret(t, e, name, ciphertext, fingerprint)

	got, err := e.ResolveSystemSecret(worker, name)
	if err != nil || got != url {
		t.Fatalf("ResolveSystemSecret under the outbound worker's actor answered %d byte(s) and %v; want the "+
			"sealed URL back. The worker resolves every secret target exactly this way, so a refusal here "+
			"is every Discord notification failing", len(got), err)
	}

	_, err = e.ResolveSystemSecret(worker, "DISCORD_5480_NOBODY_"+suffix)
	if !memql.IsVariableNotFound(err) {
		t.Fatalf("a secret nobody stored answered %v; the worker fails such a row only when "+
			"memql.IsVariableNotFound recognises the miss, and retries anything else", err)
	}

	junk := "DISCORD_5480_JUNK_" + suffix
	sealOutboundTestSecret(t, e, junk, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("not a ciphertext ", 3))), "...junk")
	_, err = e.ResolveSystemSecret(worker, junk)
	if !memql.IsSecretUndecryptable(err) {
		t.Fatalf("a secret that does not decrypt answered %v; the worker fails such a row only when "+
			"memql.IsSecretUndecryptable recognises it, and retries anything else", err)
	}
}

// sealOutboundTestSecret writes a globalSecret row through the mutation the
// secret CLI uses, as a boot seed writes: internal origin, a synthetic owner.
func sealOutboundTestSecret(t *testing.T, e *memql.MemQLEngine, name, ciphertext, fingerprint string) {
	t.Helper()
	seeder := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "dbtest-outbound-seeder", Role: auth.RoleOwner, Synthetic: true, Unranked: true,
	}))
	seeder = auth.ContextWithToken(seeder, &auth.TokenInfo{Subject: "dbtest-outbound-seeder"})
	if _, err := e.Execute(seeder, fmt.Sprintf(
		`mutation setGlobalSecret(id: %s, name: %s, encryptedValue: %s, fingerprint: %s, kind: "webhook_url")`,
		langparser.QuoteString("gs-"+strings.ToLower(name)), langparser.QuoteString(name),
		langparser.QuoteString(ciphertext), langparser.QuoteString(fingerprint))); err != nil {
		t.Fatalf("setGlobalSecret %s: %v", name, err)
	}
}

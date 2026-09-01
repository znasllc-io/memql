package email

// configure.go -- writing an integration's configuration back, from a surface
// (memql#4825 / #4826, program decision P6).
//
// # Why this is a capability and not the browser calling setGlobalVariable
//
// It would compile. It would also be wrong three separate ways, and each way
// fails silently -- a save that reports success and changes nothing is the
// worst outcome available here, because the operator then goes looking for the
// problem somewhere else entirely.
//
//  1. THE ROW NAME IS NOT A PARAMETER. A caller that supplied the name could
//     write MEMQL_EMAIL_SENDR, get a row, get a green save, and never be
//     mailed anything again -- the resolver looks up the names it knows and
//     that is not one of them. Here the caller picks a SLOT from the declared
//     manifest and this file knows the variable, exactly as
//     component/memql/provider_config_write.go does for vendor keys and for
//     the same stated reason.
//
//  2. THE ROW ID IS ALREADY DERIVED THREE WAYS IN THIS TREE, and a fourth
//     would be the one that disagrees. scripts/secrets seeds at
//     `secret-global-<slug>` / `var-global-<slug>`; the default injector uses
//     the same pair; provider_config_write uses `sec-<slug>`. The resolver
//     looks rows up by NAME, so a mismatched id does not fail -- it writes a
//     SECOND row carrying the same name, and which of the two answers is then
//     a question about query order. This file uses the seeder's derivation,
//     because the seeder's row is the one an installed cluster already has and
//     UPDATING it is the only outcome that means what the operator intended.
//
//  3. A SECRET CANNOT BE SEALED IN A BROWSER. setGlobalSecret takes
//     `encryptedValue` and `fingerprint`, and the sealing is secret.Encrypt
//     under MEMQL_MASTER_KEY -- a key that exists on nodes and must never
//     exist on a laptop. What crosses the wire is the plaintext, once, over
//     the same TLS-terminated stream every other call uses, and it is never
//     sent back.
//
// And one more that is not about the write at all: LazySender resolves its
// sender ONCE per process. Without an explicit invalidation a correct save on
// a node that has already sent mail flips the card to "configured" while every
// message keeps going to the log. Configure invalidates, so the next send
// re-resolves and the change takes effect with no restart -- which is what
// "resolved at use time" was always supposed to mean.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// ConfigWriter is the sliver of the engine a configuration write needs.
//
// Its Execute returns `any` rather than the engine's concrete result type so a
// test can stand in for it with three lines. The plug-in surface hands back
// *memql.ExecuteResult, which is why plugin.go wraps it -- one adapter, at the
// one seam, rather than this package importing the engine's result type to
// declare a method whose value it discards.
type ConfigWriter interface {
	Execute(ctx context.Context, query string) (any, error)
}

// configWriterAdapter fits the plug-in engine surface to ConfigWriter.
type configWriterAdapter struct {
	inner interface {
		Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
	}
}

func (a configWriterAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.inner.Execute(ctx, query)
}

// NewConfigWriter adapts the plug-in engine surface for WithConfigWriter.
func NewConfigWriter(engine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}) ConfigWriter {
	return configWriterAdapter{inner: engine}
}

// configureAuthorized gates the write at owner-or-developer, which is program
// decision P6 and the role set the OS Settings section declares.
//
// STRICTER THAN THE READ, deliberately. statusAuthorized also admits admin,
// because reading which mailbox a deployment sends as carries no secret and
// takes nothing away from anybody. Writing does: it changes where this
// cluster's mail leaves from, and administering PEOPLE -- which is what an
// admin is for -- is a different job from wiring up what the cluster talks to.
func configureAuthorized(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("email.configure: no authenticated caller")
	}
	if ac.Role == auth.RoleOwner || ac.Role == auth.RoleDeveloper {
		return nil
	}
	return fmt.Errorf("email.configure: role %q may not change integration configuration (owner or developer required)", string(ac.Role))
}

// rowIdFor derives the row id the SEEDER uses. See reason 2 in the file
// header: this is a deliberate re-use rather than a new derivation.
func rowIdFor(prefix, envVar string) string {
	return prefix + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(envVar), "_", "-"))
}

// handleConfigure writes one slot's value.
func (i *Integration) handleConfigure(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := configureAuthorized(ctx); err != nil {
		return nil, err
	}
	writer, ok := i.configWriter()
	if !ok {
		return nil, fmt.Errorf("email.configure: this node has no engine wired, so configuration cannot be written here")
	}

	slotName := strings.TrimSpace(argString(args, "slot"))
	if slotName == "" {
		return nil, fmt.Errorf("email.configure: slot is required")
	}
	slot, found := EmailConfigManifest().SlotByName(slotName)
	if !found {
		return nil, fmt.Errorf("email.configure: %q is not a configurable setting of the email integration; the settings it has are %s",
			slotName, strings.Join(EmailConfigManifest().SlotNames(), ", "))
	}

	// The VALUE is trimmed but never inspected further, and never logged. A
	// credential that failed because of a trailing newline is a real support
	// case; a credential in a log line is a worse one.
	value := strings.TrimSpace(argString(args, "value"))
	if value == "" {
		return nil, fmt.Errorf("email.configure: %s cannot be set to an empty value. To stop using it, clear the variable in the environment or remove the row; an empty row would read as configured and resolve to nothing", slot.Name)
	}

	var call string
	if slot.Secret {
		ciphertext, fingerprint, err := secret.Encrypt(value)
		if err != nil {
			// Never wrap the plaintext into an error: this string reaches a log.
			return nil, fmt.Errorf("email.configure: seal %s: %w", slot.Name, err)
		}
		call = renderConfigCall("setGlobalSecret", map[string]string{
			"id":             rowIdFor("secret-global-", slot.EnvVar),
			"name":           slot.EnvVar,
			"encryptedValue": ciphertext,
			"fingerprint":    fingerprint,
			"kind":           "integration",
			"description":    "Set from the OS Settings Integrations section.",
			"addedBy":        actorOrSystem(ctx),
		})
	} else {
		call = renderConfigCall("setGlobalVariable", map[string]string{
			"id":          rowIdFor("var-global-", slot.EnvVar),
			"name":        slot.EnvVar,
			"value":       value,
			"description": "Set from the OS Settings Integrations section.",
		})
	}

	if _, err := writer.Execute(ctx, call); err != nil {
		return nil, fmt.Errorf("email.configure: write %s: %w", slot.Name, err)
	}

	// Take effect NOW. Without this the row is correct, the report says
	// configured, and the sender this process resolved on its first send keeps
	// delivering to whatever it resolved to.
	reresolves := i.invalidateSender()

	return configureResult(map[string]any{
		"slot":        slot.Name,
		"envVar":      slot.EnvVar,
		"secret":      slot.Secret,
		"source":      SourceGlobalSecretOrVariable(slot.Secret),
		"reresolves":  reresolves,
		"takesEffect": takesEffectSentence(reresolves),
	})
}

// SourceGlobalSecretOrVariable names the tier the value now lives in, so a
// surface can say where it came from without re-deriving the rule.
func SourceGlobalSecretOrVariable(isSecret bool) string {
	if isSecret {
		return SourceGlobalSecret
	}
	return SourceGlobalVariable
}

func takesEffectSentence(reresolves bool) string {
	if reresolves {
		return "Saved. The next message this node sends re-resolves its sender, so it takes effect without a restart. Other replicas pick it up on their own next send."
	}
	return "Saved. This node resolves its sender from the environment, which outranks stored rows, so the row is recorded but this node will keep using the environment value."
}

// configWriter returns the engine, when one is wired.
func (i *Integration) configWriter() (ConfigWriter, bool) {
	if i == nil || i.engine == nil {
		return nil, false
	}
	return i.engine, true
}

// invalidateSender clears the lazy resolution so the next send re-reads. It
// reports whether it did: a node whose sender came from the ENVIRONMENT is not
// going to change its mind on the next send, and telling an operator that it
// will is worse than telling them it will not.
func (i *Integration) invalidateSender() bool {
	lazy, ok := i.sender.(*LazySender)
	if !ok {
		return false
	}
	return lazy.Invalidate()
}

// renderConfigCall renders `mutation <name>(k: "v", ...)`. Every value rides
// through the parser's own quoter rather than %q: a sealed secret is base64
// and a description is prose, and the two quoters disagree on control bytes in
// a way that makes the statement unparseable rather than wrong -- which would
// drop the very write that was recording the credential.
func renderConfigCall(name string, args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// Deterministic order so a rendered call is diffable in a test.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(args[k]) == "" {
			continue
		}
		parts = append(parts, k+": "+langparser.QuoteString(args[k]))
	}
	return fmt.Sprintf("mutation %s(%s)", name, strings.Join(parts, ", "))
}

// argString reads a string argument, tolerating the number and boolean forms a
// JSON caller can produce for a field the schema calls a string.
func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

// configureResult wraps the reply as the synthetic node a builtin returns.
// The concept id sits outside the v{major}:{domain}:{entity} grammar
// deliberately: it is a reply, not a row.
func configureResult(payload map[string]any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("email.configure: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "integrationConfigured",
		Concept:   "integration:email:configured",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

func actorOrSystem(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && strings.TrimSpace(ac.UserId) != "" {
		return ac.UserId
	}
	return "system:integration-config"
}

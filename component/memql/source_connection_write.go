package memql

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// A source connection records provider-verified access, not a client claim.
// Named mutations and raw writes meet this gate, including cluster owners.
func (e *MemQLEngine) validateSourceConnectionWrite(ctx context.Context, concept string, prior, delta map[string]any) error {
	if concept == "v1:platform:externalConnection" && auth.OriginFromContext(ctx) != auth.OriginInternal {
		// Only provider callbacks can create or restore verified selections.
		// A person may remove their own selection, never change its identity.
		actor, _ := auth.AccessFromContext(ctx)
		if prior == nil || actor == nil || BareShortId(stringFromAny(prior["ownerUserId"])) != BareShortId(actor.UserId) ||
			stringFromAny(delta["status"]) != "removed" || !e.OrganizationCapable(ctx, "", auth.VerbExecute, "app:settings/connections") {
			return fmt.Errorf("external connections must be added through provider authorization")
		}
		for _, field := range []string{"ownerUserId", "provider", "resourceId", "label"} {
			if value, set := delta[field]; set && !reflect.DeepEqual(value, prior[field]) {
				return fmt.Errorf("external connection identity cannot be changed")
			}
		}
		return nil
	}
	if concept == "v1:platform:sourceConnection" {
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			return fmt.Errorf("source connections must be changed through their verified source actions")
		}
		return nil
	}
	if concept != "v1:platform:package" {
		return nil
	}
	if _, changesVisibility := delta["sourceRemoved"]; changesVisibility && prior != nil {
		ac, _ := auth.AccessFromContext(ctx)
		if ac == nil || strings.TrimSpace(stringFromAny(prior["ownerUserId"])) == "" || BareShortId(ac.UserId) != BareShortId(stringFromAny(prior["ownerUserId"])) {
			return fmt.Errorf("only the repository owner can remove or restore its source entry")
		}
	}
	merged := make(map[string]any, len(prior)+len(delta))
	for k, v := range prior {
		merged[k] = v
	}
	for k, v := range delta {
		merged[k] = v
	}
	connectionID := strings.TrimSpace(stringFromAny(merged["sourceConnectionId"]))
	if connectionID == "" {
		if strings.TrimSpace(stringFromAny(prior["sourceConnectionId"])) != "" {
			return fmt.Errorf("a repository's verified source connection cannot be cleared")
		}
		return nil // Existing DSL authors may still name a credential directly.
	}
	changed := prior == nil
	for _, field := range []string{"sourceConnectionId", "credentialId", "repoUrl", "sourceKind"} {
		if next, set := delta[field]; set && !reflect.DeepEqual(prior[field], next) {
			changed = true
		}
	}
	if !changed {
		return nil
	} // Removing a chooser entry does not stop existing deployables.
	// Fetches use the persisted repository owner's personal authorization.
	// An organization editor cannot replace it with their own binding while
	// leaving that owner in place: the write would succeed but every fetch fail.
	if owner := strings.TrimSpace(stringFromAny(prior["ownerUserId"])); owner != "" {
		actor, _ := auth.AccessFromContext(ctx)
		if actor == nil || BareShortId(actor.UserId) != BareShortId(owner) {
			return fmt.Errorf("only the repository owner can change its GitHub source connection")
		}
	}
	if stringFromAny(merged["sourceKind"]) != "repo" {
		return fmt.Errorf("a GitHub source connection can only supply a repository")
	}
	handler := e.builtinExecutorHandlers["integration.packages.validateSourceConnectionRepository"]
	if handler == nil {
		return fmt.Errorf("this node cannot validate the repository's source connection")
	}
	_, err := handler(ctx, map[string]any{
		"connectionId": connectionID,
		"credentialId": stringFromAny(merged["credentialId"]),
		"repoUrl":      stringFromAny(merged["repoUrl"]),
	}, 0)
	return err
}

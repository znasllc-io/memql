package memql

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// These fields attest what the source feed, analysis or completed deployment
// observed. Their named recorders are @serverOnly and packages.store writes
// them with internal origin. A raw write must not forge the same facts: the
// upstream comparison reads deployedVersion, and changing it together with
// the policy epoch can start a new automatic deployment without its grant.
var packageObservedFields = []string{
	"deployedVersion", "latestKnownVersion", "updateAvailable", "declares",
	packageDslDomainsField, packageDslDeployedByField,
}

// The initial registration may leave observation fields blank. In particular,
// createPackage explicitly initializes updateAvailable=false. It may name the
// source on create; only recordPackageName changes that observation afterward.
func emptyPackageObservation(field string, value any) bool {
	if value == nil {
		return true
	}
	switch field {
	case "updateAvailable":
		v, ok := value.(bool)
		return ok && !v
	case "declares", packageDslDomainsField:
		switch v := value.(type) {
		case []any:
			return len(v) == 0
		case []string:
			return len(v) == 0
		}
	default:
		v, ok := value.(string)
		return ok && v == ""
	}
	return false
}

// Called on the final merged row after before-write hooks. Existing values may
// ride through ordinary business edits, but neither raw writes nor hooks may
// replace or remove server observations. This supplements the earlier strict
// DSL-field delta guard; it does not loosen that guard or the owner restoration.
func (e *MemQLEngine) validatePackageLifecycleWrite(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	serverField := func(field string) error {
		return fmt.Errorf("%s: field %q requires internal origin from its source or deployment recorder", conceptPlatformPackage, field)
	}
	for _, field := range packageObservedFields {
		if reflect.DeepEqual(prior[field], final[field]) {
			continue
		}
		if prior == nil && emptyPackageObservation(field, final[field]) {
			continue
		}
		return serverField(field)
	}
	if prior != nil && !reflect.DeepEqual(prior["name"], final["name"]) {
		return serverField("name")
	}
	if reflect.DeepEqual(prior["autoDeployChangedAt"], final["autoDeployChangedAt"]) ||
		prior == nil && strings.TrimSpace(stringFromAny(final["autoDeployChangedAt"])) == "" {
		return nil
	}
	// The epoch is the person's policy decision, not a server observation.
	// setPackageAutoDeploy writes it even when Automatic stays selected. Ask
	// the same source-policy capability for a raw epoch change, in both sides
	// of an organization move, while retaining the existing data/row guards.
	accounts := map[string]bool{}
	for _, row := range []map[string]any{prior, final} {
		if account := strings.TrimSpace(stringFromAny(row["accountId"])); account != "" {
			accounts[BareShortId(account)] = true
		}
	}
	if len(accounts) == 0 {
		accounts[""] = true
	}
	for account := range accounts {
		if e == nil || !e.OrganizationCapable(ctx, account, auth.VerbExecute, "app:pipelines/repositories") {
			return fmt.Errorf("%s: changing a package deployment policy epoch requires execute app:pipelines/repositories", CodeCapabilityNotHeld)
		}
	}
	return nil
}

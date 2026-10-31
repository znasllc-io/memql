package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// platform_package_dsl_domains_guard.go -- package.dslDomains has one writer.
//
// The field is the list of DSL domains a package ships, and the bff opens an
// access code only for a gate whose domain is in it (design record
// 2026-10-29, D5). Whoever writes it chooses which gates answer for the
// package's sites: a domain it names that the package does not ship reaches
// another product's operations through this package's code, and an emptied
// list closes every code on every site of the package.
//
// Its writer is a package deploy, and a rollback, under internal origin,
// through the @serverOnly recordPackageDslDomains (component/packages). But
// @serverOnly bars only that NAMED call. A raw insert() at an existing package
// id read-merges its delta onto the stored row (executeWrite), so
//
//	insert("v1:platform:package", id=<pkg>, payload={"dslDomains": [...]})
//
// rewrote the list for a cluster owner, a developer, and a plain writer in the
// package's organization, on their own package and on a colleague's -- every
// caller the package's row tier admits to a write. @serverWritten would shut
// it, but a package has client-reachable writes (registering one, its settings,
// archiving it), so the field is reserved and the concept is not.

// packageDslDomainsField is the one field of v1:platform:package this guard
// reserves: dsl/platform/concepts.memql:package.dslDomains.
const packageDslDomainsField = "dslDomains"

// validatePackageDslDomainsServerOnly refuses a write to a package without
// internal origin whose DELTA names dslDomains.
//
// Judged on the delta, and on whether it NAMES the field, never on its value:
// a client that wants the list left as it is leaves it out, and the read-merge
// keeps what a delta omits -- so a write that merely re-sends the rest of the
// row cannot change it. A rule that compared values would need the stored row,
// and would admit a client re-asserting the list it just read, which is
// exactly the shape of a forgery that happens to match.
//
// Called by executeWrite on the delta as supplied, BEFORE the read-merge: after
// it the stored list and a forged one are one map entry, which is also why
// executeWrite captures deltaNamedOwner before the merge. A refused write reads
// nothing.
func validatePackageDslDomainsServerOnly(ctx context.Context, delta map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if _, named := delta[packageDslDomainsField]; !named {
		return nil
	}
	return fmt.Errorf(
		"%s: write to `%s` refused -- this field lists the DSL domains a package ships, and it is "+
			"written only by a package deploy or rollback, under internal origin, through "+
			"recordPackageDslDomains: the bff opens an access code only for a gate whose domain is in "+
			"it, so a caller who could name it would choose which gates answer for the package's sites. "+
			"@serverOnly bars only that named call; a raw insert() at the package's id read-merges its "+
			"delta onto the stored row, so the delta is judged here. A write that does not name the "+
			"field leaves it as it is",
		conceptPlatformPackage, packageDslDomainsField)
}

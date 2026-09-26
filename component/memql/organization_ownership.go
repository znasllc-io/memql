package memql

// Organization ownership is a product policy, distinct from the DSL's additive
// account-sharing annotation. Multi-account labels on goals/files/knowledge do
// not become ownership merely because they mention an account.
import (
	"context"
	"fmt"
	"github.com/uptrace/bun"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func organizationOwnedConcept(concept string) bool {
	switch concept {
	case "v1:campaigns:audience", "v1:campaigns:recipient", "v1:campaigns:template", "v1:campaigns:senderIdentity", "v1:campaigns:campaign", "v1:campaigns:delivery", "v1:campaigns:engagementEvent", "v1:campaigns:consentEvent", "v1:campaigns:emailRule", "v1:platform:site", "v1:platform:package", "v1:platform:packageDeployment", "v1:identity:group", "v1:identity:groupMembership":
		return true
	}
	return false
}

func organizationOperator(ctx context.Context) bool {
	ac, _ := auth.AccessFromContext(ctx)
	return ac != nil && (ac.Unranked && ac.IsClusterOwner() || auth.IsClusterOperator(ac.Role))
}

// Defaults are resolved from verified identity and current shared memberships,
// never from the requested account. Multiple memberships require a choice.
func organizationDefaultAccount(operator bool, scope *accountScope) (string, error) {
	if operator {
		return "self", nil
	}
	choices := map[string]bool{}
	if scope != nil {
		for id := range scope.accounts {
			choices[BareShortId(id)] = true
		}
	}
	if len(choices) == 1 {
		for id := range choices {
			return id, nil
		}
	}
	return "", fmt.Errorf("organization_required: select one of your authorized organizations")
}

// staged-data: MUST-NOT-GATE -- account existence is an authorization fact, including staged rows; hiding it can misattribute work.
func (e *MemQLEngine) validateOrganizationOwnership(ctx context.Context, concept string, payload map[string]any, existed bool) error {
	if concept == conceptAccountsAccount || concept == conceptIdentityGroup || concept == conceptIdentityGroupMembership {
		ac, _ := auth.AccessFromContext(ctx)
		if organizationOperator(ctx) || (ac != nil && ac.Synthetic && auth.OriginFromContext(ctx).IsInternal()) {
			return nil
		}
		return fmt.Errorf("organization_forbidden: organization and membership changes require their governed management actions")
	}
	if !organizationOwnedConcept(concept) {
		return nil
	}
	// Server writers copy attribution from their authoritative parent. Legacy
	// maintenance writes must remain possible before old rows are attributed.
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return fmt.Errorf("organization_forbidden: this write has no authenticated actor")
	}
	if ac.Synthetic {
		return nil
	}
	account, _ := payload["accountId"].(string)
	account = strings.TrimSpace(account)
	scope := e.accountScopeFor(ctx)
	if account == "" {
		if existed {
			return fmt.Errorf("organization_required: attribute this existing record before changing it")
		}
		var err error
		account, err = organizationDefaultAccount(organizationOperator(ctx), scope)
		if err != nil {
			return err
		}
	}
	if !organizationOperator(ctx) && (scope == nil || !scope.admits(account)) {
		return fmt.Errorf("organization_forbidden: you are not a member of the selected organization")
	}
	if !organizationOperator(ctx) && !e.organizationAppAllows(ctx, account, organizationApp(concept)) {
		return fmt.Errorf("organization_forbidden: this app is not available to you in this organization")
	}
	verb := auth.VerbCreate
	if existed {
		verb = auth.VerbUpdate
	}
	if !e.OrganizationCapable(ctx, account, auth.VerbRead, auth.ResourceData) || !e.OrganizationCapable(ctx, account, verb, auth.ResourceData) {
		return fmt.Errorf("organization_forbidden: this organization does not permit the requested data change")
	}
	// An authorized spelling must still name an active account. A dangling
	// membership or a forged id must not create unattached organization work.
	db := e.database()
	if db == nil {
		return fmt.Errorf("organization_unavailable: account registry cannot be read")
	}
	var rows []memorynodes.MemoryNode
	err := db.NewSelect().Model(&rows).Where("concept = ?", conceptAccountsAccount).
		Where("id IN (?)", bun.In([]string{account, BareShortId(account), conceptAccountsAccount + ":" + BareShortId(account)})).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx)
	if err != nil {
		return fmt.Errorf("organization_unavailable: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("organization_not_found: select an existing organization")
	}
	row := accountRowPayload(rows[0])
	if row == nil || row["status"] == "archived" || row["status"] == "deleted" {
		return fmt.Errorf("organization_inactive: select an active organization")
	}
	payload["accountId"] = conceptAccountsAccount + ":" + BareShortId(account)
	return e.validateOrganizationReferences(ctx, concept, payload, account)
}

// Related organization-owned records must agree even for an operator who may
// read both. Otherwise a campaign for Acme could mail another client's list.
func organizationReferences(concept string) map[string]string {
	switch concept {
	case "v1:campaigns:campaign", "v1:campaigns:emailRule":
		return map[string]string{"audienceId": "v1:campaigns:audience", "templateId": "v1:campaigns:template", "senderIdentityId": "v1:campaigns:senderIdentity"}
	case "v1:campaigns:recipient":
		return map[string]string{"audienceId": "v1:campaigns:audience"}
	case "v1:campaigns:delivery":
		return map[string]string{"campaignId": "v1:campaigns:campaign", "emailRuleId": "v1:campaigns:emailRule", "recipientId": "v1:campaigns:recipient"}
	case "v1:campaigns:engagementEvent":
		return map[string]string{"campaignId": "v1:campaigns:campaign"}
	case "v1:campaigns:consentEvent":
		return map[string]string{"recipientId": "v1:campaigns:recipient"}
	case "v1:platform:packageDeployment":
		return map[string]string{"packageId": "v1:platform:package"}
	case "v1:identity:groupMembership":
		return map[string]string{"groupId": "v1:identity:group"}
	}
	return nil
}

// staged-data: MUST-NOT-GATE -- compare all stored references so staging cannot bypass organization equality.
func (e *MemQLEngine) validateOrganizationReferences(ctx context.Context, concept string, payload map[string]any, account string) error {
	for field, parentConcept := range organizationReferences(concept) {
		id, _ := payload[field].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		var rows []memorynodes.MemoryNode
		err := e.database().NewSelect().Model(&rows).Where("concept = ?", parentConcept).
			Where("id IN (?)", bun.In([]string{id, BareShortId(id), parentConcept + ":" + BareShortId(id)})).
			OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx)
		if err != nil {
			return fmt.Errorf("organization_unavailable: %w", err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("organization_reference: %s is unavailable", field)
		}
		parent := accountRowPayload(rows[0])
		parentAccount, _ := parent["accountId"].(string)
		if !organizationAccountsAgree(account, parentAccount) {
			return fmt.Errorf("organization_mismatch: %s must belong to the same organization", field)
		}
	}
	return nil
}

func organizationAccountsAgree(account, parent string) bool {
	return strings.TrimSpace(account) != "" && strings.TrimSpace(parent) != "" && BareShortId(account) == BareShortId(parent)
}

func organizationApp(concept string) string {
	if strings.HasPrefix(concept, "v1:campaigns:") {
		return "app:campaigns"
	}
	if strings.HasPrefix(concept, "v1:platform:") {
		return "app:deployables"
	}
	return ""
}

// Group grants attached to another organization do not change this one's
// app decision. Untied groups and direct user grants remain global by design.
func (e *MemQLEngine) organizationAppAllows(ctx context.Context, account, resource string) bool {
	return e.organizationCapabilityAllows(ctx, account, auth.VerbRead, resource)
}

// OrganizationCapable resolves an action's current authority in its target
// organization, including borrowed worker identities and organization group
// grants. Callers must still resolve and authorize the resource itself. An
// empty account supports existing private rows and conveys no membership.
func (e *MemQLEngine) OrganizationCapable(ctx context.Context, account string, verb, resource string) bool {
	if strings.TrimSpace(account) != "" && !organizationOperator(ctx) {
		scope := e.accountScopeFor(ctx)
		if scope == nil || !scope.admits(account) {
			return false
		}
	}
	return e.organizationCapabilityAllows(ctx, account, verb, resource)
}

func (e *MemQLEngine) organizationCapabilityAllows(ctx context.Context, account string, verb, resource string) bool {
	if resource == "" {
		return true
	}
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok {
		return false
	}
	if subject.Unranked {
		ac, _ := auth.AccessFromContext(ctx)
		if ac == nil || ac.Synthetic {
			return auth.CapableFor(ctx, subject, verb, resource)
		}
		role, found := e.organizationUserRole(ctx, subject.UserId)
		if !found {
			return false
		}
		subject.Role = role
		subject.Unranked = false
	}
	if resource == auth.ResourceData && strings.TrimSpace(account) == "" && auth.RoleAccountScope(subject.Role) != "" {
		return false
	}
	if e != nil {
		memberships := e.membershipsFor(ctx, subject.UserId)
		subject.GroupIds = nil
		for _, group := range memberships.groupIds {
			groupAccount := memberships.groupAccounts[BareShortId(group)]
			if groupAccount == "" || groupAccount == BareShortId(account) {
				subject.GroupIds = append(subject.GroupIds, group)
			}
		}
	}
	return auth.CapableFor(ctx, subject, verb, resource)
}

func organizationAppAllowsRow(ctx context.Context, account, resource string) bool {
	memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo)
	var engine *MemQLEngine
	if memo != nil {
		engine = memo.engine
	}
	return engine.organizationReadAllows(ctx, account, resource)
}

// GlobalDataCapable is the targetless transport decision. Organization-bound
// groups and roles cannot lend their authority to unrelated personal data.
// Unknown roles retain the transport's existing reader fallback.
func (e *MemQLEngine) GlobalDataCapable(ctx context.Context, verb string) bool {
	ac, _ := auth.AccessFromContext(ctx)
	normalized := auth.AccessContext{Role: auth.RoleReader}
	if ac != nil {
		normalized = *ac
	}
	normalized.Role = auth.Role(strings.ToLower(strings.TrimSpace(string(normalized.Role))))
	if !auth.IsValidRole(normalized.Role) {
		normalized.Role = auth.RoleReader
	}
	return e.organizationCapabilityAllows(auth.ContextWithAccess(ctx, &normalized), "", verb, auth.ResourceData)
}

func (e *MemQLEngine) organizationReadAllows(ctx context.Context, account, app string) bool {
	if organizationOperator(ctx) || strings.TrimSpace(account) == "" {
		return e.GlobalDataCapable(ctx, auth.VerbRead)
	}
	return e.organizationCapabilityAllows(ctx, account, auth.VerbRead, auth.ResourceData) && e.organizationAppAllows(ctx, account, app)
}

// Borrowed authority is a real person whose role was intentionally not copied
// onto a worker context. Re-read it at the receiver before resolving app grants;
// the synthetic writer rung is attribution, not their current permission.
// staged-data: MUST-NOT-GATE -- a current principal role is authorization state, not content visibility.
func (e *MemQLEngine) organizationUserRole(ctx context.Context, userID string) (auth.Role, bool) {
	if e == nil || e.database() == nil {
		return "", false
	}
	var rows []memorynodes.MemoryNode
	err := e.database().NewSelect().Model(&rows).Where("concept = ?", conceptIdentityUser).
		Where("id IN (?)", bun.In([]string{userID, BareShortId(userID), conceptIdentityUser + ":" + BareShortId(userID)})).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx)
	if err != nil || len(rows) != 1 {
		return "", false
	}
	row := accountRowPayload(rows[0])
	if row == nil || row["active"] == false || row["status"] == "deleted" {
		return "", false
	}
	role, _ := row["role"].(string)
	if !auth.IsValidRole(auth.Role(role)) {
		return "", false
	}
	return auth.Role(role), true
}

// A record with dependent organization work cannot be retagged behind that
// work's back. Unchanged attribution skips the dependent-row scan. A caller
// authorized in both organizations can move an unused draft; moving history
// requires an explicit coordinated migration.
// staged-data: MUST-NOT-GATE -- a staged dependent still prevents an inconsistent ownership transfer.
func (e *MemQLEngine) validateOrganizationTransfer(ctx context.Context, concept, id string, prior, delta map[string]any) error {
	if !organizationOwnedConcept(concept) {
		return nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac != nil && ac.Synthetic {
		return nil
	}
	old, _ := prior["accountId"].(string)
	// A move requires write authority in the source as well as the target.
	// Otherwise membership in both accounts plus a target-only grant could
	// take a row out of an organization the caller may only read.
	if !e.OrganizationCapable(ctx, old, auth.VerbUpdate, auth.ResourceData) {
		return fmt.Errorf("organization_forbidden: you cannot change data in the existing organization")
	}
	next, present := delta["accountId"].(string)
	if !present || BareShortId(next) == BareShortId(old) {
		return nil
	}
	if e.database() == nil {
		return fmt.Errorf("organization_unavailable: dependent records cannot be checked")
	}
	for _, childConcept := range []string{"v1:campaigns:campaign", "v1:campaigns:emailRule", "v1:campaigns:recipient", "v1:campaigns:delivery", "v1:campaigns:engagementEvent", "v1:campaigns:consentEvent", "v1:platform:packageDeployment", "v1:identity:groupMembership"} {
		for field, parent := range organizationReferences(childConcept) {
			if parent != concept {
				continue
			}
			var rows []memorynodes.MemoryNode
			if err := e.database().NewSelect().Model(&rows).DistinctOn("id").Where("concept = ?", childConcept).OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx); err != nil {
				return fmt.Errorf("organization_unavailable: %w", err)
			}
			for _, row := range rows {
				payload := accountRowPayload(row)
				if BareShortId(stringFromAny(payload[field])) != BareShortId(id) {
					continue
				}
				if BareShortId(stringFromAny(payload["accountId"])) != BareShortId(next) {
					return fmt.Errorf("organization_transfer_refused: related records still belong to the existing organization")
				}
			}
		}
	}
	return nil
}

// Result caching must observe app grants as well as membership. Otherwise a
// denial on another replica can leave a previously cached campaign readable.
func (e *MemQLEngine) organizationAuthorizationFingerprint(ctx context.Context) string {
	if organizationOperator(ctx) {
		return fmt.Sprintf("operator/read/%t", e.GlobalDataCapable(ctx, auth.VerbRead))
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return "anonymous"
	}
	scope := e.accountScopeFor(ctx)
	decisions := map[string]struct{}{fmt.Sprintf("global/read/%t", e.GlobalDataCapable(ctx, auth.VerbRead)): {}}
	if scope != nil {
		if scope.everyAccount {
			for _, resource := range []string{auth.ResourceData, "app:campaigns", "app:deployables"} {
				decisions[fmt.Sprintf("every/%s/%t", resource, e.organizationAppAllows(ctx, "", resource))] = struct{}{}
			}
		}
		for account := range scope.accounts {
			bare := BareShortId(account)
			for _, resource := range []string{auth.ResourceData, "app:campaigns", "app:deployables"} {
				decisions[fmt.Sprintf("%s/%s/%t", bare, resource, e.organizationAppAllows(ctx, bare, resource))] = struct{}{}
			}
		}
	}
	return fingerprintAccountSet(ac.UserId, decisions)
}

// HasOrganizationBoundary reports product resources whose reads and writes
// enforce account membership in addition to their declared row authorization.
func HasOrganizationBoundary(concept string) bool {
	return concept == conceptAccountsAccount || organizationOwnedConcept(concept)
}

package memql

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A delayed publication inherits the original Execute's authorization memos.
// A fresh evaluation must read another replica's committed changes without
// replacing the requester, cancellation budget, or native write restrictions.
func TestFreshAuthorizationRechecksCrossReplicaMembershipAndPreservesFences(t *testing.T) {
	writer, db, _ := sharedReadMergeEngine(t)
	receiver, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Logger = writer.Logger
	if err := receiver.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	receiver.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(oldGrants); auth.SetMembershipSource(oldMembers) })
	suffix := uniqueSuffix("fresh-authority")
	account, group, member, peer := "account-"+suffix, "group-"+suffix, "member-"+suffix, "peer-"+suffix
	seedPrincipal(t, writer, member, auth.RoleWriter)
	seedPrincipal(t, writer, peer, auth.RoleReader)
	if err := organizationInsert(t, writer, groupSeedCtx(), conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, writer, group, group, "account", account)
	seedMembership(t, writer, group, member, "active")
	writeGrant(t, writer, auth.SubjectKindGroup, group, auth.VerbExecute, "app:deployables/deploy", auth.GrantAllow)

	parent, cancel := context.WithTimeout(rankActorCtx(member, auth.RoleWriter), time.Minute)
	defer cancel()
	warm := contextWithPersonRoleMemo(contextWithRankScopeMemo(contextWithAccountScopeMemo(parent, receiver), receiver))
	warm = auth.ContextWithGrantMemo(warm)
	warm = ContextWithRowVersionFence(warm, conceptPlatformSite, "site-"+suffix, time.Unix(100, 0))
	warm = ContextWithPackagePublicationFence(warm, "run-"+suffix, strings.Repeat("1", 64), "lease-"+suffix, "site/serving")
	if !receiver.accountScopeFor(warm).admits(account) || !receiver.OrganizationCapable(warm, account, auth.VerbExecute, "app:deployables/deploy") {
		t.Fatal("initial account and group action were not authorized")
	}
	if !receiver.rankScopeFor(warm).admitsRead(peer) || receiver.personRole(warm, member) != auth.RoleWriter {
		t.Fatal("initial rank/person-role fixture did not resolve")
	}

	// Real writes through the other engine; no cache invalidation event or
	// heartbeat is needed by the final fresh evaluation.
	seedMembership(t, writer, group, member, "removed")
	seedPrincipal(t, writer, peer, auth.RoleOwner)
	seedPrincipal(t, writer, member, auth.RoleReader)
	if !receiver.accountScopeFor(warm).admits(account) || !receiver.OrganizationCapable(warm, account, auth.VerbExecute, "app:deployables/deploy") || !receiver.rankScopeFor(warm).admitsRead(peer) || receiver.personRole(warm, member) != auth.RoleWriter {
		t.Fatal("negative control did not retain the original request snapshots")
	}

	fresh := ContextWithFreshAuthorization(warm)
	if receiver.accountScopeFor(fresh).admits(account) || receiver.OrganizationCapable(fresh, account, auth.VerbExecute, "app:deployables/deploy") {
		t.Fatal("fresh evaluation reused revoked membership/account authority")
	}
	if receiver.rankScopeFor(fresh).admitsRead(peer) || receiver.personRole(fresh, member) != auth.RoleReader {
		t.Fatal("fresh evaluation reused rank or person-role snapshots")
	}
	if !FreshReadFromContext(fresh) {
		t.Fatal("fresh evaluation did not bypass result cache")
	}
	beforeActor, _ := auth.AccessFromContext(warm)
	afterActor, _ := auth.AccessFromContext(fresh)
	beforeToken := auth.TokenInfoFromContext(warm)
	if beforeToken == nil || beforeToken.Subject != member {
		t.Fatal("fixture did not carry the original requester token")
	}
	if beforeActor != afterActor || beforeToken != auth.TokenInfoFromContext(fresh) || auth.OriginFromContext(warm) != auth.OriginFromContext(fresh) {
		t.Fatal("fresh evaluation changed identity, token, origin or claimed role")
	}
	beforeDeadline, beforeOK := warm.Deadline()
	afterDeadline, afterOK := fresh.Deadline()
	if beforeOK != afterOK || !beforeDeadline.Equal(afterDeadline) || warm.Done() != fresh.Done() {
		t.Fatal("fresh evaluation changed cancellation/deadline")
	}
	beforeFence, beforeFenced := packagePublicationFenceFrom(warm)
	afterFence, afterFenced := packagePublicationFenceFrom(fresh)
	if !beforeFenced || !afterFenced || beforeFence != afterFence || !reflect.DeepEqual(rowVersionFences(warm), rowVersionFences(fresh)) {
		t.Fatal("fresh evaluation changed native publication or row-version fences")
	}
	cancel()
	if fresh.Err() != context.Canceled {
		t.Fatal("fresh evaluation lost parent cancellation")
	}
}

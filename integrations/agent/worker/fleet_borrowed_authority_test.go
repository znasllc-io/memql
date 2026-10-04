//go:build agent

package worker

// AN AUTOMATION UNDER A PERSON'S AUTHORITY USES WHAT IS LENT TO THEM
// (memql#5662, decided 2026-10-04: lending to a person lends to all of their
// work, automated or not).
//
// The ruling holds because of a seam, and this walks it. An automation that
// runs under a person's BORROWED authority has two identities on its context:
// component/automations' AuthorContext stamps the author's AccessContext, and
// the executor's contextWithSystemActor then names the automation on the
// claims and token while leaving that inherited AccessContext alone
// (component/automations' TestSystemActorDoesNotClobberAnAuthorsAccessContext
// holds that half). The model seam must read the ACCESS context -- the person
// -- and not the claims: actingUserFromContext when the provider registry
// resolves `fleet:<model>` with no user named, and fleetProvider.call when it
// dispatches. Were either to read the automation's synthetic id instead, the
// automation would be the cluster's own work and the machine lent to the
// person would refuse it, which is exactly what the contrast case shows.
//
// THE LENT MACHINE IS ON THIS REPLICA. An authored run's context carries no
// forwarded authority, so ForwardModelCall refuses a machine held by another
// replica before start whoever owns it; that is a matter of the hop, not of
// sharing, and the pin and hop tests cover the hop for a person's own context.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

const (
	borrowedPerson     = "v1:identity:user:ana"
	borrowedAutomation = "nightlyDigest"
	borrowedLender     = "v1:identity:user:bob"
)

// automationActingFor is the context an authored automation of person's runs
// under: AuthorContext, then contextWithSystemActor's stamp for a caller it
// inherited -- the claims and the token name the automation, and the
// AccessContext stays the person's.
func automationActingFor(person, automation string) context.Context {
	ctx := automations.AuthorContext(context.Background(), person)
	actor := "system:automation:" + automation
	claims := map[string]any{"sub": actor, "email": actor, "role": "system"}
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
}

// automationAsItself is contextWithSystemActor with nothing inherited: the
// automation is the only actor, synthetic, the cluster's own work.
func automationAsItself(automation string) context.Context {
	actor := "system:automation:" + automation
	claims := map[string]any{"sub": actor, "email": actor, "role": "system"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: actor, Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
}

func TestAnAutomationUnderAPersonsAuthorityIsServedByWhatIsLentToThem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		served bool
	}{
		{"under the person's borrowed authority", automationActingFor(borrowedPerson, borrowedAutomation), true},
		// The contrast: the same automation running as ITSELF is the
		// cluster's own work, and a machine lent to named people never serves
		// that (design G3).
		{"running as itself", automationAsItself(borrowedAutomation), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			w := &workerservice.Worker{
				RegistrationId: "bobs-studio", OwnerUserId: borrowedLender, Name: "bobs-studio",
				Capabilities: []string{workerservice.CapabilityHeadless, workerservice.ModelCapability},
				Labels:       map[string]string{workerservice.ModelLabel(sharedHopModel): "ctx=32768"},
				Concurrency:  map[string]uint32{workerservice.ModelCapability: 2},
			}
			w.SetModelCallFunc(func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
				calls++
				h, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
				go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: "served by bob's studio"})
				return h, nil
			})
			// Lent to ana by name, both consents given, held by this replica.
			studio := machine("bobs-studio")
			studio.OwnerUserId = borrowedLender
			studio.ConnectedNodeId = nodeA
			studio.Capabilities = w.Capabilities
			studio.Labels = w.Labels
			lendTo([]string{"ana"}, nil, true)(&studio)

			f := newFleetInference(t, &sharedFleet{fakeFleet: &fakeFleet{owner: borrowedPerson}, all: []Candidate{studio}})
			f.selfNodeId = nodeA
			f.registry.Add(w)
			registry := memqlengine.NewProviderRegistryForTest()
			registry.SetFleetInference(f)

			// No user named: the registry takes the acting user off the
			// context, as every caller that holds only a context does.
			entry, ok := registry.EntryForUser(tc.ctx, "", memqlengine.FleetReferencePrefix+sharedHopModel)
			if !ok || entry == nil {
				t.Fatal("no fleet entry")
			}
			if !tc.served {
				if entry.Available {
					t.Fatal("the machine lent to ana is available to the automation running as itself")
				}
				if calls != 0 {
					t.Fatalf("calls = %d: the cluster's own work ran on a machine lent to a person", calls)
				}
				return
			}
			if !entry.Available {
				t.Fatal("the machine lent to ana is unavailable to an automation acting under ana's authority: " +
					"the seam took the automation's id, not the person's")
			}
			chat, ok := entry.Client.(common.ChatAIProvider)
			if !ok {
				t.Fatalf("a fleet entry must serve chat, got %T", entry.Client)
			}
			answer, err := chat.CallChat(tc.ctx, []common.ChatMessage{{Role: "user", Content: "digest"}})
			if err != nil || answer != "served by bob's studio" || calls != 1 {
				t.Fatalf("answer %q, calls %d, err %v: work done under ana's authority must run on what is lent to her", answer, calls, err)
			}
			// And it was recorded as BOB's machine serving ANA's call: the
			// attribution compares against the acting person, so it reads
			// the person too.
			if owner, ok := entry.Client.(interface{ MachineOwner() string }); !ok || owner.MachineOwner() != borrowedLender {
				t.Fatalf("whose machine served the call = %v, want the lender %q (D15)", owner, borrowedLender)
			}
		})
	}
}

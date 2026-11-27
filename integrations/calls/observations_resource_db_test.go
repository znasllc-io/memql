package calls

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestObservationManagedDelegatedResourceIsNotAParty(t *testing.T) {
	cw, owner, _ := commerceFixture(t, "observation-managed-resource")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	t.Cleanup(func() {
		for _, concept := range []string{resourceConcept, observationConcept} {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(ctx)
		}
	})
	commerceVerified(t, cw, owner)
	installResourceFake(cw.svc, &resourceProviderFake{values: map[string]*ResourceIngress{}})
	source := registerTestResource(t, cw, owner, "observation-resource-registration")
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "host", "resources_publish", "observations"}
	grant.ResourceIDs = []string{source.ID}
	token, _ := delegationTestGrant(t, cw, owner, grant)
	raw, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "quote", ParticipantLimit: 4, DurationMinutes: 30, Insights: true, Resources: true})
	require.NoError(t, err)
	quote := raw.(*callrules.CallQuote)
	raw, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, token, callrules.AgentRequest{Action: "start", QuoteID: quote.QuoteID, IdempotencyKey: "observation-resource-room", Name: "Reviewed host assistant"})
	require.NoError(t, err)
	started := raw.(callrules.StartResult)
	_, observer, err := cw.join(started.Call.GuestToken, "Observer")
	require.NoError(t, err)
	_, subject, err := cw.join(started.Call.GuestToken, "Subject")
	require.NoError(t, err)
	raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "resource", Resource: &callrules.ResourceRequest{Action: "start", ResourceID: source.ID, RoomID: started.Call.ID, ExpectedRevision: source.Revision, RequestID: "observation-resource-publication"}})
	require.NoError(t, err)
	publication := raw.(callrules.ResourceResult).Publication
	require.NotNil(t, publication)
	room, err := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, err)
	d, err := cw.svc.store.resourceState(ctx, cw.owner, cw.site, room.KeyID)
	require.NoError(t, err)
	require.Len(t, d.State.Leases, 1)
	resourceSeat, err := cw.svc.store.seatByID(ctx, cw.owner, d.State.Leases[0].SeatID)
	require.NoError(t, err)
	require.NotEmpty(t, resourceSeat.AgentData, "actual delegated creation inherits reviewed context; blank-metadata filtering misses this case")
	require.NotEmpty(t, d.State.Leases[0].GrantID)
	base, err := cw.svc.backendFor(ctx, *room)
	require.NoError(t, err)
	media := &observationTestMedia{MediaBackend: base}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }

	observerView, err := cw.svc.SessionCall(ctx, observer)
	require.NoError(t, err)
	require.Nil(t, observerView.Roster, "guest observation access must not expose the host roster")
	subjectView, err := cw.svc.SessionCall(ctx, subject)
	require.NoError(t, err)
	legitimate := []string{started.Join.Identity, observerView.Identity, subjectView.Identity}
	checkChoices := func(view callrules.WorkflowResult) {
		t.Helper()
		identities := []string{}
		for _, party := range view.Observations.Participants {
			identities = append(identities, party.Identity)
		}
		require.ElementsMatch(t, legitimate, identities, "only real people and the reviewed agent are observation parties")
	}
	checkChoices(observationRead(t, cw, observer))
	raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &callrules.WorkflowRequest{Action: "observations", Observation: &callrules.ObservationRequest{RoomID: room.ID, Identity: started.Join.Identity}}})
	require.NoError(t, err)
	checkChoices(raw.(callrules.WorkflowResult))

	for _, role := range []string{"subject", "recipient"} {
		t.Run(role, func(t *testing.T) {
			q := observationAsk(t, cw, observer, subject, "resource-cannot-be-"+role)
			if role == "subject" {
				q.Observation.SubjectIdentity = publication.Identity
				q.Observation.RecipientIdentities = []string{observerView.Identity, publication.Identity}
			} else {
				q.Observation.RecipientIdentities = append(q.Observation.RecipientIdentities, publication.Identity)
			}
			_, err := cw.svc.Workflow(ctx, cw.ref, observer.Token, q)
			require.ErrorContains(t, err, "observations_admission_changed")
		})
	}
	// Real parties retain the existing explicit subject-consent/publication path,
	// including a reviewed agent recipient under its independent observations scope.
	q := observationAsk(t, cw, observer, subject, "real-parties-permission")
	q.Observation.RecipientIdentities = legitimate
	requested := observationDo(t, cw, observer, q)
	p := requested.Observations.Permissions[0]
	approved := observationDo(t, cw, subject, observationCommand(t, cw, subject, p, "observation_consent", "real-subject-consent"))
	p = approved.Observations.Permissions[0]
	result := observationDo(t, cw, observer, observationCommand(t, cw, observer, p, "observation_publish", "real-parties-publication"))
	require.Equal(t, "published", result.ObservationReceipt.Status)
	require.Len(t, media.packets, 1)
	require.ElementsMatch(t, legitimate, media.recipients[0])

	// Older journals can name a delegated source as an additional recipient. The
	// same authority projection must close delivery, not merely hide new choices.
	j, err := cw.svc.store.observationState(ctx, cw.owner, cw.site, room.ID)
	require.NoError(t, err)
	resourceParty, err := cw.svc.observationParty(ctx, *room, *resourceSeat)
	require.NoError(t, err)
	j.Permissions[0].Recipients = append(j.Permissions[0].Recipients, resourceParty)
	require.NoError(t, cw.svc.store.saveObservation(ctx, cw.owner, j, cw.clock.Now()))
	read := observationRead(t, cw, observer)
	require.Equal(t, "expired", read.Observations.Permissions[0].Status)
	require.False(t, read.Observations.Permissions[0].CanPublish)
	_, err = cw.svc.Workflow(ctx, cw.ref, observer.Token, observationCommand(t, cw, observer, p, "observation_publish", "legacy-resource-must-not-publish"))
	require.ErrorContains(t, err, "observations_expired")
	require.Len(t, media.packets, 1)
}
